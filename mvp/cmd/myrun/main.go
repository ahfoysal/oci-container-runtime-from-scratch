// myrun is a minimal OCI-style container runtime (MVP).
//
// Usage:
//
//	myrun pull <image[:tag]>
//	myrun run [--memory=512M] [--cpu=0.5] [--pids=100] <rootfs-or-image> <cmd> [args...]
//
// As of M3, the rootfs argument can be either a local directory (classic
// behaviour) or an image reference that has been pulled into the local
// store (`data/images/...`). In the latter case the runtime stacks the
// image layers as an OverlayFS lowerdir and creates a writable upperdir for
// the container — so each `run` gets a fresh copy-on-write rootfs without
// duplicating the image on disk.
//
// On macOS, the runtime subcommands are stubs that print a message and exit
// non-zero — the binary still compiles so development on macOS works, but
// actual execution requires Linux (namespaces, chroot, OverlayFS).
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/cgroups"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/image"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/network"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/runtime"
)

// defaultStoreRoot is the on-disk location for pulled images + container
// scratch dirs. Override with MYRUN_STORE.
const defaultStoreRoot = "data"

func storeRoot() string {
	if v := os.Getenv("MYRUN_STORE"); v != "" {
		return v
	}
	return defaultStoreRoot
}

func usage() {
	fmt.Fprintf(os.Stderr, `myrun — MVP OCI-style container runtime

Usage:
  myrun pull <image[:tag]>                          Download image layers from Docker Hub into %s/
  myrun run [flags] <rootfs-or-image> <cmd> [args]  Run <cmd> inside a new container
  myrun child <rootfs-dir> <cmd> [args...]          (internal) re-exec entrypoint after clone

Flags for 'run':
  --memory=SIZE            Hard memory limit, e.g. 64M, 512M, 1G. Unset = unlimited.
  --cpu=N                  CPU cores (fractional allowed), e.g. 0.5 = half a core. Unset = unlimited.
  --pids=N                 Max number of PIDs in the container. Unset = unlimited.
  --publish=H:C[/proto]    Forward host port H to container port C (tcp default). Repeatable.

Image reference resolution for 'run':
  If the first positional is an existing directory, it is used as the rootfs
  (M1 behavior). Otherwise it is parsed as an image reference (e.g.
  "alpine:3.20") and, if already pulled, mounted as an OverlayFS stack.

Examples:
  myrun pull alpine:3.20
  myrun run alpine:3.20 /bin/sh
  myrun run --memory=64M --cpu=0.5 --pids=100 ./rootfs /bin/sh

Notes:
  Requires Linux (namespaces, chroot, cgroups v2, OverlayFS). On macOS this
  binary compiles but runtime subcommands exit with an error. 'pull' works
  on both — image download is just HTTP — so you can pre-fetch images from
  the host before dropping into your Linux VM.

Environment:
  MYRUN_STORE   Override the store root (default: %s/).
`, defaultStoreRoot, defaultStoreRoot)
}

// publishFlag collects repeated --publish host:container[/proto] values.
// Implementing flag.Value lets us accept the flag multiple times on the
// command line (e.g. `--publish 8080:80 --publish 8443:443`).
type publishFlag []network.PortMapping

func (p *publishFlag) String() string {
	if p == nil || len(*p) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*p))
	for _, pm := range *p {
		parts = append(parts, fmt.Sprintf("%d:%d/%s", pm.HostPort, pm.ContainerPort, pm.Protocol))
	}
	return strings.Join(parts, ",")
}

// Set parses one --publish value of the form `host:container` or
// `host:container/proto` and appends it to the list.
func (p *publishFlag) Set(v string) error {
	pm, err := parsePublish(v)
	if err != nil {
		return err
	}
	*p = append(*p, pm)
	return nil
}

// parsePublish parses `host:container[/proto]` into a PortMapping. proto
// defaults to tcp. We deliberately keep this tiny — no interface binding,
// no ranges, no IPv6 — to match the M4 scope.
func parsePublish(s string) (network.PortMapping, error) {
	spec := s
	proto := "tcp"
	if i := strings.IndexByte(spec, '/'); i >= 0 {
		proto = strings.ToLower(spec[i+1:])
		spec = spec[:i]
	}
	if proto != "tcp" && proto != "udp" {
		return network.PortMapping{}, fmt.Errorf("publish %q: protocol must be tcp or udp", s)
	}
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return network.PortMapping{}, fmt.Errorf("publish %q: expected host:container[/proto]", s)
	}
	host, herr := strconv.Atoi(parts[0])
	cont, cerr := strconv.Atoi(parts[1])
	if herr != nil || cerr != nil || host <= 0 || host > 65535 || cont <= 0 || cont > 65535 {
		return network.PortMapping{}, fmt.Errorf("publish %q: invalid port numbers", s)
	}
	return network.PortMapping{HostPort: host, ContainerPort: cont, Protocol: proto}, nil
}

// parseMemory accepts sizes like "512", "512K", "64M", "1G" (case-insensitive,
// IEC/binary multipliers) and returns bytes. An empty string yields 0.
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	last := s[len(s)-1]
	switch last {
	case 'k', 'K':
		mult = 1 << 10
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = 1 << 20
		s = s[:len(s)-1]
	case 'g', 'G':
		mult = 1 << 30
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("memory size must be >= 0")
	}
	return n * mult, nil
}

// pullCmd implements `myrun pull <ref>`.
func pullCmd(argv []string) error {
	if len(argv) < 1 {
		usage()
		return fmt.Errorf("pull requires an image reference")
	}
	ref, err := image.ParseRef(argv[0])
	if err != nil {
		return err
	}
	fmt.Printf("Pulling %s from Docker Hub...\n", ref)
	c := &image.Client{}
	dir, err := c.Pull(ref, storeRoot())
	if err != nil {
		return err
	}
	fmt.Printf("Pulled %s into %s\n", ref, dir)
	return nil
}

// runCmd parses flags + positionals for the `run` subcommand and dispatches
// into the runtime package. The rootfs argument may be a directory (classic)
// or an image reference that has been pulled.
func runCmd(argv []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	memStr := fs.String("memory", "", "memory limit (e.g. 64M, 512M, 1G)")
	cpu := fs.Float64("cpu", 0, "CPU cores (fractional, e.g. 0.5)")
	pids := fs.Int64("pids", 0, "max PIDs in container")
	var publish publishFlag
	fs.Var(&publish, "publish", "publish host:container[/proto] (repeatable)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		usage()
		return fmt.Errorf("run requires <rootfs-or-image> and <cmd>")
	}

	memBytes, err := parseMemory(*memStr)
	if err != nil {
		return err
	}

	cfg := runtime.Config{
		Rootfs: fs.Arg(0),
		Cmd:    fs.Arg(1),
		Args:   fs.Args()[2:],
		Limits: cgroups.Limits{
			MemoryBytes: memBytes,
			CPUQuota:    *cpu,
			PidsMax:     *pids,
		},
		StoreRoot:    storeRoot(),
		PortMappings: []network.PortMapping(publish),
	}
	return runtime.Run(cfg)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "pull":
		if err := pullCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "myrun: pull failed: %v\n", err)
			os.Exit(1)
		}
	case "run":
		if err := runCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "myrun: run failed: %v\n", err)
			os.Exit(1)
		}
	case "child":
		// Internal re-exec path: this is the process that already has the new
		// namespaces; it performs the chroot/mount setup and execs the user
		// cmd. The parent has already placed us into a cgroup (if any) and
		// prepared the rootfs (possibly an overlay mount) before releasing
		// the sync pipe we block on inside Child.
		if len(os.Args) < 4 {
			usage()
			os.Exit(2)
		}
		rootfs := os.Args[2]
		cmd := os.Args[3]
		args := os.Args[4:]
		if err := runtime.Child(rootfs, cmd, args); err != nil {
			fmt.Fprintf(os.Stderr, "myrun: child failed: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}
