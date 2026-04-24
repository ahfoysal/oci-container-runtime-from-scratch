//go:build linux

// Package criu wraps CRIU (Checkpoint/Restore In Userspace) to freeze a
// running myrun container to disk and later resume it. CRIU is a Linux
// project that uses ptrace + /proc + a kernel interface to snapshot a
// process tree's memory, file descriptors, sockets, and namespaces into
// a directory of "image files" (*.img) that can be re-injected later.
//
// Scope for M6
//
// We support two operations that shell out to the `criu` CLI:
//
//   - Dump(pid, imagesDir): checkpoint the container rooted at `pid`.
//     Produces <imagesDir>/*.img plus a dump.log. The container process
//     tree is killed after a successful dump (criu's default — use
//     --leave-running to keep it live; we don't because resuming a still-
//     running tree isn't useful).
//
//   - Restore(imagesDir): recreate the process tree from a previously
//     dumped directory. CRIU reinstates namespaces, fds, memory maps and
//     resumes execution at the exact instruction where the dump happened.
//
// Both operations require:
//   - CRIU binary (>= 3.15) installed on the host. Detected via PATH.
//   - CAP_SYS_ADMIN / CAP_CHECKPOINT_RESTORE. In practice run as root.
//   - A kernel compiled with CHECKPOINT_RESTORE support (every mainstream
//     distro ships this). CRIU itself prints a clear error if missing.
//
// What this package does NOT cover (out of M6 scope, noted in README):
//   - Lazy migration / live migration between hosts.
//   - Restoring with different network topology (requires --tcp-established
//     and iptables replay; trivially addable later).
//   - Pre-dump iterations for minimizing downtime — the toy runtime is
//     not latency-sensitive.
//
// We deliberately shell out rather than link libcriu: CRIU exposes a
// stable CLI + gRPC but both libcriu-dev and protobuf pull in heavy
// dependencies. Shelling out matches how runc, podman, and containerd
// historically drove CRIU for years before the RPC mode matured.
package criu

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Dump snapshots the process tree rooted at pid into imagesDir. imagesDir
// must exist (or be creatable) and be empty or contain only prior dump
// output that the caller is willing to overwrite.
//
// Flags we pass and why:
//
//	--tree <pid>         : root PID of the process tree to dump.
//	--images-dir <dir>   : where to write *.img files.
//	--shell-job          : allow dumping processes attached to a controlling
//	                       terminal (our container's /bin/sh qualifies).
//	--tcp-established    : snapshot+restore live TCP sockets. No-op if the
//	                       container has none; necessary once it does.
//	--file-locks         : persist flock/fcntl locks across the dump.
//	--link-remap         : handle unlinked-but-open files (common in /tmp).
//	--manage-cgroups=soft: dump cgroup membership but don't try to migrate
//	                       the cgroup itself (we rebuild it fresh on restore).
//	--log-file dump.log  : keep the log next to the images for debugging.
func Dump(pid int, imagesDir string) error {
	if pid <= 0 {
		return fmt.Errorf("criu: invalid pid %d", pid)
	}
	bin, err := exec.LookPath("criu")
	if err != nil {
		return errors.New("criu not found on PATH — install the `criu` package")
	}
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return fmt.Errorf("criu: prepare images dir: %w", err)
	}

	args := []string{
		"dump",
		"--tree", strconv.Itoa(pid),
		"--images-dir", imagesDir,
		"--shell-job",
		"--tcp-established",
		"--file-locks",
		"--link-remap",
		"--manage-cgroups=soft",
		"--log-file", "dump.log",
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Point users at dump.log — CRIU failures are almost always
		// best diagnosed from there (missing kernel feature, unsupported
		// fd type, seccomp blocking ptrace, etc).
		return fmt.Errorf("criu dump: %w (see %s)", err, filepath.Join(imagesDir, "dump.log"))
	}
	return nil
}

// Restore recreates the process tree described by imagesDir. On success
// the restored process runs in the foreground of the calling terminal
// (via --shell-job) and `criu restore` itself exits only when the
// restored process does — same semantics as `myrun run`.
//
// Flags mirror Dump so the symmetric metadata survives the round-trip.
// `--restore-detached` is deliberately NOT passed: we want the restored
// tree's exit to propagate back as our exit code.
func Restore(imagesDir string) error {
	if _, err := os.Stat(imagesDir); err != nil {
		return fmt.Errorf("criu: images dir missing: %w", err)
	}
	bin, err := exec.LookPath("criu")
	if err != nil {
		return errors.New("criu not found on PATH — install the `criu` package")
	}
	args := []string{
		"restore",
		"--images-dir", imagesDir,
		"--shell-job",
		"--tcp-established",
		"--file-locks",
		"--link-remap",
		"--manage-cgroups=soft",
		"--log-file", "restore.log",
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("criu restore: %w (see %s)", err, filepath.Join(imagesDir, "restore.log"))
	}
	return nil
}

// Available reports whether the `criu` binary is on PATH. Used by the CLI
// to fail `myrun checkpoint` / `myrun restore` early with a clear message
// rather than mid-exec.
func Available() bool {
	_, err := exec.LookPath("criu")
	return err == nil
}
