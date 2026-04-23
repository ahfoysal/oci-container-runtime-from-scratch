// myrun is a minimal OCI-style container runtime (MVP).
//
// Usage:
//
//	myrun run <rootfs-dir> <cmd> [args...]
//
// On Linux, `run` clones a child with new PID/UTS/MNT/IPC/NET namespaces,
// chroots into <rootfs-dir>, and execs <cmd>. On macOS, the runtime subcommand
// is a stub that prints a message and exits non-zero — the binary still
// compiles so development on macOS works, but actual execution requires Linux.
package main

import (
	"fmt"
	"os"

	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/runtime"
)

func usage() {
	fmt.Fprintf(os.Stderr, `myrun — MVP OCI-style container runtime

Usage:
  myrun run <rootfs-dir> <cmd> [args...]   Run <cmd> inside a new container
  myrun child <rootfs-dir> <cmd> [args...] (internal) re-exec entrypoint after clone

Examples:
  myrun run ./rootfs /bin/sh

Notes:
  Requires Linux (namespaces, chroot). On macOS this binary compiles but the
  runtime subcommands will exit with an error. Test inside a Multipass/UTM VM.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		if len(os.Args) < 4 {
			usage()
			os.Exit(2)
		}
		rootfs := os.Args[2]
		cmd := os.Args[3]
		args := os.Args[4:]
		if err := runtime.Run(rootfs, cmd, args); err != nil {
			fmt.Fprintf(os.Stderr, "myrun: run failed: %v\n", err)
			os.Exit(1)
		}
	case "child":
		// Internal re-exec path: this is the process that already has the new
		// namespaces; it performs the chroot/mount setup and execs the user cmd.
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
