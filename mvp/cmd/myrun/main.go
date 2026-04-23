// myrun is a minimal OCI-style container runtime (MVP).
//
// Usage:
//
//	myrun run [--memory=512M] [--cpu=0.5] [--pids=100] <rootfs-dir> <cmd> [args...]
//
// On Linux, `run` clones a child with new PID/UTS/MNT/IPC/NET namespaces,
// places the child in a cgroups v2 subgroup with the requested limits,
// chroots into <rootfs-dir>, and execs <cmd>. On macOS, the runtime
// subcommand is a stub that prints a message and exits non-zero — the binary
// still compiles so development on macOS works, but actual execution
// requires Linux.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/cgroups"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/runtime"
)

func usage() {
	fmt.Fprintf(os.Stderr, `myrun — MVP OCI-style container runtime

Usage:
  myrun run [flags] <rootfs-dir> <cmd> [args...]   Run <cmd> inside a new container
  myrun child <rootfs-dir> <cmd> [args...]         (internal) re-exec entrypoint after clone

Flags for 'run':
  --memory=SIZE   Hard memory limit, e.g. 64M, 512M, 1G. Unset = unlimited.
  --cpu=N         CPU cores (fractional allowed), e.g. 0.5 = half a core. Unset = unlimited.
  --pids=N        Max number of PIDs in the container. Unset = unlimited.

Examples:
  myrun run ./rootfs /bin/sh
  myrun run --memory=64M --cpu=0.5 --pids=100 ./rootfs /bin/sh

Notes:
  Requires Linux (namespaces, chroot, cgroups v2). On macOS this binary
  compiles but the runtime subcommands will exit with an error, and cgroup
  operations are stubbed out. Test inside a Multipass/UTM/Lima VM.
`)
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

// runCmd parses flags + positionals for the `run` subcommand and dispatches
// into the runtime package.
func runCmd(argv []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	memStr := fs.String("memory", "", "memory limit (e.g. 64M, 512M, 1G)")
	cpu := fs.Float64("cpu", 0, "CPU cores (fractional, e.g. 0.5)")
	pids := fs.Int64("pids", 0, "max PIDs in container")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		usage()
		return fmt.Errorf("run requires <rootfs-dir> and <cmd>")
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
	}
	return runtime.Run(cfg)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		if err := runCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "myrun: run failed: %v\n", err)
			os.Exit(1)
		}
	case "child":
		// Internal re-exec path: this is the process that already has the new
		// namespaces; it performs the chroot/mount setup and execs the user
		// cmd. The parent has already placed us into a cgroup (if any) before
		// releasing the sync pipe we block on inside Child.
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
