//go:build linux

// Package network implements minimal Linux bridge networking for myrun
// containers: a single host-side bridge `myrun0` (10.44.0.1/24) that every
// container attaches to via a dedicated veth pair, plus optional iptables
// DNAT rules for `--publish host:container` port forwarding.
//
// Design
//
//  1. On first container start the host lazily creates the `myrun0` bridge,
//     assigns it 10.44.0.1/24, brings it up, and enables ip_forward +
//     a MASQUERADE rule on the subnet so containers can reach the outside
//     world via the host's default interface.
//
//  2. For each container the parent:
//     - picks a /24 host number (hashed from the container id for
//       idempotency in this toy — real runtimes use an IPAM plugin)
//     - creates a veth pair `vm<id>` (host side) / `vc<id>` (peer)
//     - attaches host side to `myrun0`, brings it up
//     - moves the peer into the child's netns (`ip link set netns <pid>`)
//     - inside the child's netns (via `ip -n <pid>`): sets the peer name
//       back to `eth0`, assigns 10.44.0.X/24, brings it + lo up, adds a
//       default route via the bridge
//     - writes /etc/resolv.conf into the rootfs (8.8.8.8, 1.1.1.1)
//     - for each --publish mapping adds an iptables DNAT rule in the
//       `nat/PREROUTING` chain and a matching FORWARD accept.
//
//  3. On Teardown: delete iptables rules, delete the host-side veth (which
//     also removes the peer), leave `myrun0` in place for the next run.
//
// This is intentionally shell-based (`ip`, `iptables`) — writing a netlink
// client in stdlib-only Go would dwarf the rest of the runtime. `ip` and
// `iptables` are present on every distro we target. We do not depend on
// CNI plugins.
package network

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// BridgeName is the single shared host bridge. Using a fixed name
	// keeps teardown simple and idempotent across container runs.
	BridgeName = "myrun0"
	// BridgeCIDR is the /24 assigned to the bridge. Containers get
	// 10.44.0.2 – 10.44.0.254 from this subnet.
	BridgeCIDR = "10.44.0.1/24"
	// BridgeIP is the bridge's own IP — the default gateway for
	// every container on `myrun0`.
	BridgeIP = "10.44.0.1"
	// Subnet is used for the MASQUERADE rule so outbound traffic from
	// containers is SNAT'd to the host's default egress interface.
	Subnet = "10.44.0.0/24"
)

// PortMapping describes a host-port -> container-port forward.
type PortMapping struct {
	HostPort      int
	ContainerPort int
	Protocol      string // "tcp" or "udp" — defaults to tcp if empty
}

// Config captures everything Setup needs: who the container is (id + pid for
// its netns) and what ports to publish.
type Config struct {
	// ContainerID is a short hex id shared with the runtime's scratch dir.
	// We derive deterministic veth names + IP offset from it so cleanup
	// can find the interfaces again even if state was lost.
	ContainerID string
	// ChildPID is the PID of the child process whose netns we set up.
	ChildPID int
	// Rootfs is the merged rootfs path — we write /etc/resolv.conf into it.
	Rootfs string
	// PortMappings lists --publish entries. Empty = no DNAT rules.
	PortMappings []PortMapping
}

// Network is a handle returned by Setup. It records enough state for
// Teardown to remove everything it created even if Config is lost.
type Network struct {
	// ContainerIP is the address assigned inside the container (10.44.0.X).
	ContainerIP string
	// HostVeth is the host-side veth name (deleting this also removes the peer).
	HostVeth string
	// PortMappings is a copy of what was published, needed for rule removal.
	PortMappings []PortMapping
}

// EnsureBridge creates `myrun0` if missing and makes sure it is up with the
// expected IP, ip_forward is on, and the MASQUERADE rule for our subnet is
// installed. Idempotent — safe to call on every container start.
func EnsureBridge() error {
	// Is the bridge already present?
	if err := run("ip", "link", "show", BridgeName); err != nil {
		// Not present — create it.
		if err := run("ip", "link", "add", BridgeName, "type", "bridge"); err != nil {
			return fmt.Errorf("create bridge %s: %w", BridgeName, err)
		}
		if err := run("ip", "addr", "add", BridgeCIDR, "dev", BridgeName); err != nil {
			return fmt.Errorf("assign %s to %s: %w", BridgeCIDR, BridgeName, err)
		}
	}
	if err := run("ip", "link", "set", BridgeName, "up"); err != nil {
		return fmt.Errorf("bring %s up: %w", BridgeName, err)
	}

	// Enable IPv4 forwarding — without this, packets from containers won't
	// traverse the bridge → host → egress-nic path.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}

	// Install MASQUERADE for our subnet unless it's already there. We use
	// `-C` (check) first so repeated runs don't stack duplicate rules.
	if err := run("iptables", "-t", "nat", "-C", "POSTROUTING",
		"-s", Subnet, "!", "-o", BridgeName, "-j", "MASQUERADE"); err != nil {
		if err := run("iptables", "-t", "nat", "-A", "POSTROUTING",
			"-s", Subnet, "!", "-o", BridgeName, "-j", "MASQUERADE"); err != nil {
			return fmt.Errorf("install MASQUERADE: %w", err)
		}
	}

	// Allow forwarding across the bridge. Some distros ship with a default
	// DROP in FORWARD, which would otherwise block all container traffic.
	if err := run("iptables", "-C", "FORWARD", "-i", BridgeName, "-j", "ACCEPT"); err != nil {
		_ = run("iptables", "-A", "FORWARD", "-i", BridgeName, "-j", "ACCEPT")
	}
	if err := run("iptables", "-C", "FORWARD", "-o", BridgeName, "-j", "ACCEPT"); err != nil {
		_ = run("iptables", "-A", "FORWARD", "-o", BridgeName, "-j", "ACCEPT")
	}

	return nil
}

// Setup ensures the bridge exists, wires up a veth pair for this container,
// moves the peer into the child's netns, assigns it an IP, and (if requested)
// installs DNAT rules for published ports.
//
// Must be called from the parent after the child has been started with
// CLONE_NEWNET but before the sync pipe is closed — the child is expected to
// inherit the already-configured interface when it chroots/execs.
func Setup(cfg Config) (*Network, error) {
	if err := EnsureBridge(); err != nil {
		return nil, err
	}

	// Veth names are capped at 15 chars (IFNAMSIZ-1). "vm" + 12 hex chars
	// of the container id fits; we truncate defensively.
	idShort := cfg.ContainerID
	if len(idShort) > 10 {
		idShort = idShort[:10]
	}
	hostVeth := "vm" + idShort
	peerVeth := "vc" + idShort

	// Pick a deterministic host-portion IP in .2-.254. Hashing the id is
	// toy-level IPAM but good enough: 253 slots, collisions unlikely under
	// normal dev use; real runtimes keep an allocator file.
	hostN := ipFromID(cfg.ContainerID)
	containerIP := fmt.Sprintf("10.44.0.%d", hostN)
	containerCIDR := containerIP + "/24"

	// Create the veth pair. If a stale pair with the same name exists from
	// a previous crashed run, delete it first.
	_ = run("ip", "link", "del", hostVeth) // best-effort
	if err := run("ip", "link", "add", hostVeth, "type", "veth", "peer", "name", peerVeth); err != nil {
		return nil, fmt.Errorf("create veth %s<->%s: %w", hostVeth, peerVeth, err)
	}

	// Plug host side into the bridge and bring it up.
	if err := run("ip", "link", "set", hostVeth, "master", BridgeName); err != nil {
		_ = run("ip", "link", "del", hostVeth)
		return nil, fmt.Errorf("attach %s to %s: %w", hostVeth, BridgeName, err)
	}
	if err := run("ip", "link", "set", hostVeth, "up"); err != nil {
		_ = run("ip", "link", "del", hostVeth)
		return nil, fmt.Errorf("bring %s up: %w", hostVeth, err)
	}

	// Move peer into the child's netns. After this, the peer is invisible
	// from the host's default netns — we operate on it via `ip -n <pid>`.
	if err := run("ip", "link", "set", peerVeth, "netns", fmt.Sprintf("%d", cfg.ChildPID)); err != nil {
		_ = run("ip", "link", "del", hostVeth)
		return nil, fmt.Errorf("move %s into netns %d: %w", peerVeth, cfg.ChildPID, err)
	}

	// Configure the peer inside the netns: rename to eth0, set IP, bring
	// up, add default route. `ip -n <pid>` runs these in the target netns
	// without needing to enter it ourselves.
	pid := fmt.Sprintf("%d", cfg.ChildPID)
	steps := [][]string{
		{"ip", "-n", pid, "link", "set", peerVeth, "name", "eth0"},
		{"ip", "-n", pid, "link", "set", "lo", "up"},
		{"ip", "-n", pid, "addr", "add", containerCIDR, "dev", "eth0"},
		{"ip", "-n", pid, "link", "set", "eth0", "up"},
		{"ip", "-n", pid, "route", "add", "default", "via", BridgeIP},
	}
	for _, s := range steps {
		if err := run(s[0], s[1:]...); err != nil {
			_ = run("ip", "link", "del", hostVeth)
			return nil, fmt.Errorf("configure netns: %v: %w", s, err)
		}
	}

	// Write a resolv.conf inside the rootfs so DNS works from the container.
	// We do this after overlay is mounted but before the child chroots — it
	// is a plain file write into the merged dir.
	if cfg.Rootfs != "" {
		if err := writeResolvConf(cfg.Rootfs); err != nil {
			// Non-fatal: the container will still come up, DNS just won't work.
			fmt.Fprintf(os.Stderr, "network: warning: resolv.conf: %v\n", err)
		}
	}

	// Publish ports: install DNAT + forward rules. Each rule is tagged
	// with `-m comment --comment myrun:<id>` so Teardown can find and
	// remove exactly its own rules rather than guessing.
	for _, pm := range cfg.PortMappings {
		if err := addPublishRules(cfg.ContainerID, containerIP, pm); err != nil {
			_ = run("ip", "link", "del", hostVeth)
			return nil, err
		}
	}

	return &Network{
		ContainerIP:  containerIP,
		HostVeth:     hostVeth,
		PortMappings: cfg.PortMappings,
	}, nil
}

// Teardown removes the host-side veth (which also removes the peer in the
// already-gone netns) and deletes the iptables rules we installed. Safe to
// call even if Setup partially failed.
func (n *Network) Teardown(cfg Config) error {
	var firstErr error
	// iptables rules first — if the interface is already gone, rule removal
	// still works.
	for _, pm := range n.PortMappings {
		if err := delPublishRules(cfg.ContainerID, n.ContainerIP, pm); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if n.HostVeth != "" {
		// Ignore "Cannot find device" — it was already cleaned up.
		_ = run("ip", "link", "del", n.HostVeth)
	}
	return firstErr
}

// addPublishRules installs one DNAT rule (host:hostPort -> containerIP:containerPort)
// and the matching FORWARD accept so the packet survives the default chain.
func addPublishRules(id, containerIP string, pm PortMapping) error {
	proto := pm.Protocol
	if proto == "" {
		proto = "tcp"
	}
	comment := "myrun:" + id
	dport := fmt.Sprintf("%d", pm.HostPort)
	target := fmt.Sprintf("%s:%d", containerIP, pm.ContainerPort)

	// PREROUTING handles inbound traffic hitting the host's external NIC.
	if err := run("iptables", "-t", "nat", "-A", "PREROUTING",
		"-p", proto, "--dport", dport,
		"-m", "comment", "--comment", comment,
		"-j", "DNAT", "--to-destination", target); err != nil {
		return fmt.Errorf("publish DNAT PREROUTING: %w", err)
	}
	// OUTPUT handles loopback traffic originated on the host (so you can
	// curl 127.0.0.1:<hostPort> from the host itself).
	if err := run("iptables", "-t", "nat", "-A", "OUTPUT",
		"-p", proto, "--dport", dport,
		"-m", "comment", "--comment", comment,
		"-j", "DNAT", "--to-destination", target); err != nil {
		return fmt.Errorf("publish DNAT OUTPUT: %w", err)
	}
	// Accept the forwarded flow.
	if err := run("iptables", "-A", "FORWARD",
		"-p", proto, "-d", containerIP, "--dport", fmt.Sprintf("%d", pm.ContainerPort),
		"-m", "comment", "--comment", comment,
		"-j", "ACCEPT"); err != nil {
		return fmt.Errorf("publish FORWARD accept: %w", err)
	}
	return nil
}

// delPublishRules removes what addPublishRules installed. Uses `-D` with the
// exact same arguments — iptables matches rules by full spec, so symmetry
// is load-bearing here.
func delPublishRules(id, containerIP string, pm PortMapping) error {
	proto := pm.Protocol
	if proto == "" {
		proto = "tcp"
	}
	comment := "myrun:" + id
	dport := fmt.Sprintf("%d", pm.HostPort)
	target := fmt.Sprintf("%s:%d", containerIP, pm.ContainerPort)

	_ = run("iptables", "-t", "nat", "-D", "PREROUTING",
		"-p", proto, "--dport", dport,
		"-m", "comment", "--comment", comment,
		"-j", "DNAT", "--to-destination", target)
	_ = run("iptables", "-t", "nat", "-D", "OUTPUT",
		"-p", proto, "--dport", dport,
		"-m", "comment", "--comment", comment,
		"-j", "DNAT", "--to-destination", target)
	_ = run("iptables", "-D", "FORWARD",
		"-p", proto, "-d", containerIP, "--dport", fmt.Sprintf("%d", pm.ContainerPort),
		"-m", "comment", "--comment", comment,
		"-j", "ACCEPT")
	return nil
}

// writeResolvConf drops a minimal resolv.conf into the rootfs so DNS works.
// Uses public resolvers — we don't try to inherit the host's /etc/resolv.conf
// because that often points to 127.0.0.53 (systemd-resolved) which isn't
// reachable from the container netns.
func writeResolvConf(rootfs string) error {
	etc := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(etc, 0755); err != nil {
		return err
	}
	content := "nameserver 8.8.8.8\nnameserver 1.1.1.1\n"
	return os.WriteFile(filepath.Join(etc, "resolv.conf"), []byte(content), 0644)
}

// ipFromID hashes the container id into [2, 254] — the usable host portion
// of our /24. 0/255/1 are reserved (network/broadcast/gateway).
func ipFromID(id string) int {
	h := sha1.Sum([]byte(id))
	n := binary.BigEndian.Uint32(h[:4])
	return int(n%253) + 2
}

// run executes an external command (typically `ip` or `iptables`), squashing
// stdout and capturing stderr into the returned error for useful diagnostics.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, msg)
	}
	return nil
}

