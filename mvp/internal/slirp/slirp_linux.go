//go:build linux

// Package slirp integrates slirp4netns(1) so rootless containers can have
// outbound network connectivity without needing CAP_NET_ADMIN on the host
// or a root-owned bridge.
//
// Background
//
// In M5 we shipped rootless mode but explicitly skipped bridge networking:
// creating veth pairs, adding interfaces to a bridge, and installing
// iptables rules all require real root. That left rootless containers
// with only a loopback interface. slirp4netns is the standard way out:
// it is a userspace program that runs on the host, opens the container's
// netns, creates a TAP device inside it, and bridges L3 traffic to the
// host via a userspace network stack (libslirp). Packets leaving the
// container go through libslirp, out the host as if they came from the
// slirp4netns process itself — no root, no kernel bridging, no iptables.
//
// Podman and rootlesskit both ship this exact pattern. We reuse it
// verbatim; the only work on our side is spawning slirp4netns at the
// right moment (after the child is cloned, before it execs) and cleaning
// it up on container exit.
//
// Contract
//
//   - slirp4netns must be installed on the host. We detect it with
//     exec.LookPath and return a helpful error pointing at the distro
//     package names (`slirp4netns` on Debian/Ubuntu/Fedora/Arch) when
//     missing.
//   - The container must already be cloned into its own netns. We take
//     the child PID and pass it as slirp4netns's `--netns-type=path`
//     target via /proc/<pid>/ns/net.
//   - We wire slirp4netns's "ready" pipe (--ready-fd) so Setup blocks
//     until the TAP device is configured inside the netns. Without this
//     the child could exec before the interface exists and see ENETUNREACH
//     on its first connect().
//   - On Teardown we SIGTERM the slirp4netns process. It cleans up the
//     TAP device on exit; we Wait() to reap the child.
//
// Defaults match slirp4netns's own defaults so containers see a familiar
// network: 10.0.2.0/24, gateway 10.0.2.2, DNS 10.0.2.3, container IP
// 10.0.2.100. These are independent of the M4 bridge network (10.44.0/24)
// because rootless and root modes never coexist for a single container.
package slirp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	// TAPDevice is the interface name slirp4netns creates inside the
	// container's netns. Matches the slirp4netns(1) default so users
	// debugging from inside the container see the same `tap0` every
	// rootless tutorial mentions.
	TAPDevice = "tap0"
	// CIDR is the /24 slirp4netns hands out by default.
	CIDR = "10.0.2.0/24"
	// GatewayIP is the userspace gateway address inside the slirp
	// network. Traffic to this address is terminated by libslirp and
	// re-emitted by the host.
	GatewayIP = "10.0.2.2"
	// DNSIP is the stub resolver slirp4netns exposes; it forwards to
	// the host's /etc/resolv.conf.
	DNSIP = "10.0.2.3"
	// ContainerIP is the address slirp assigns to the TAP inside the
	// container. Fixed because rootless containers typically run one at
	// a time per invocation and don't share a namespace.
	ContainerIP = "10.0.2.100"
)

// Handle represents a live slirp4netns subprocess. Callers retain it until
// the container exits and then call Teardown.
type Handle struct {
	cmd      *exec.Cmd
	readyEnd *os.File // parent-retained read end; closed in Setup after we sync.
}

// Config is the set of knobs Setup needs. Currently just the child PID.
// Left as a struct so we can grow port-forwarding (--publish in rootless
// mode via slirp4netns's `api-socket`) without churning the signature.
type Config struct {
	// ChildPID is the host-side PID of the container process; slirp4netns
	// joins its netns via /proc/<pid>/ns/net.
	ChildPID int
	// Rootfs, if set, receives a minimal /etc/resolv.conf pointing at
	// slirp's stub resolver. Empty means skip — caller may manage DNS
	// themselves.
	Rootfs string
}

// Setup launches slirp4netns, blocks until it signals "ready" on the
// sync pipe, and returns a Handle the caller uses for Teardown.
//
// Errors:
//   - slirp4netns binary not in PATH: actionable message with distro hints.
//   - spawn failure: wrapped.
//   - ready-fd timeout: we give slirp4netns up to 5s to come up; if the
//     pipe doesn't close, we SIGKILL and return an error. Slow kernels
//     can be bumped via SLIRP4NETNS_READY_TIMEOUT env var.
func Setup(cfg Config) (*Handle, error) {
	bin, err := exec.LookPath("slirp4netns")
	if err != nil {
		return nil, errors.New("slirp4netns not found on PATH — install it " +
			"(`apt install slirp4netns` / `dnf install slirp4netns` / " +
			"`pacman -S slirp4netns`) or run without --rootless")
	}

	// Pipe used as --ready-fd. slirp4netns writes a single byte and closes
	// the fd once the TAP is fully configured. We pass the write end to
	// the child and keep the read end in the parent to wait on.
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("slirp: pipe: %w", err)
	}

	// slirp4netns arguments:
	//   --configure           : auto-assign IP+route inside the netns
	//   --mtu=65520           : libslirp's internal max; matches default
	//   --disable-host-loopback: containers can't reach 127.0.0.1 on host
	//                           (security: matches podman rootless default)
	//   --ready-fd=3          : write a byte to fd 3 when setup complete
	//   <pid> <tap>           : target netns (via /proc/<pid>) and iface name
	args := []string{
		"--configure",
		"--mtu=65520",
		"--disable-host-loopback",
		"--ready-fd=3",
		strconv.Itoa(cfg.ChildPID),
		TAPDevice,
	}
	c := exec.Command(bin, args...)
	// Map readyW into slirp4netns as fd 3. ExtraFiles[0] -> fd 3+0.
	c.ExtraFiles = []*os.File{readyW}
	// slirp4netns is chatty on stderr even in success; route it to our
	// stderr so users see failures. stdout is unused.
	c.Stderr = os.Stderr
	// Put it in its own process group so a Ctrl-C on the parent terminal
	// doesn't race our explicit SIGTERM during Teardown.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := c.Start(); err != nil {
		readyR.Close()
		readyW.Close()
		return nil, fmt.Errorf("slirp: spawn: %w", err)
	}
	// Once started, only slirp4netns needs the write end.
	readyW.Close()

	// Wait for the ready signal with a timeout so a broken slirp4netns
	// binary doesn't wedge the runtime forever.
	if err := waitReady(readyR, readyTimeout()); err != nil {
		_ = c.Process.Signal(syscall.SIGKILL)
		_, _ = c.Process.Wait()
		readyR.Close()
		return nil, err
	}

	// Optional: install a minimal resolv.conf inside the rootfs pointing
	// at slirp's stub resolver. The M4 bridge path writes 8.8.8.8/1.1.1.1
	// directly; in rootless we prefer slirp's forwarder because it
	// follows the host's own resolver config (including VPN/corporate DNS).
	if cfg.Rootfs != "" {
		_ = writeResolvConf(cfg.Rootfs)
	}

	return &Handle{cmd: c, readyEnd: readyR}, nil
}

// Teardown signals slirp4netns to exit, waits for it to reap, and releases
// any retained file descriptors. Safe to call on a nil Handle (no-op).
func (h *Handle) Teardown() error {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return nil
	}
	// SIGTERM first so slirp4netns can deconfigure the TAP cleanly. If
	// the process has already exited (container crashed and slirp
	// detected the netns vanish), the kill is a harmless no-op.
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	// Don't wait forever — slirp4netns occasionally wedges on libslirp
	// shutdown on old kernels. 2s is more than enough for the common path.
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = h.cmd.Process.Signal(syscall.SIGKILL)
		<-done
	}
	if h.readyEnd != nil {
		h.readyEnd.Close()
	}
	return nil
}

// waitReady blocks until the ready pipe closes or fires a byte. slirp4netns
// writes one byte on success then keeps the fd open for the process
// lifetime, so a successful read of 1 byte means "ready". If the pipe
// closes with no data, slirp4netns crashed and we surface that.
func waitReady(r *os.File, timeout time.Duration) error {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	buf := make([]byte, 1)
	go func() {
		n, err := r.Read(buf)
		ch <- result{n: n, err: err}
	}()
	select {
	case res := <-ch:
		if res.n == 1 {
			return nil
		}
		if res.err != nil {
			return fmt.Errorf("slirp: not ready (pipe closed): %w", res.err)
		}
		return errors.New("slirp: not ready (empty read)")
	case <-time.After(timeout):
		return fmt.Errorf("slirp: timed out after %s waiting for --ready-fd", timeout)
	}
}

// readyTimeout honours SLIRP4NETNS_READY_TIMEOUT (in seconds) for users
// on slow VMs; defaults to 5s.
func readyTimeout() time.Duration {
	if v := os.Getenv("SLIRP4NETNS_READY_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Second
}

// writeResolvConf drops a stub resolv.conf into the rootfs pointing at
// slirp's DNS stub, which forwards to the host's resolver. Best-effort:
// silently ignores failures because the container can still set its own
// /etc/resolv.conf at runtime.
func writeResolvConf(rootfs string) error {
	path := filepath.Join(rootfs, "etc", "resolv.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("nameserver "+DNSIP+"\n"), 0o644)
}
