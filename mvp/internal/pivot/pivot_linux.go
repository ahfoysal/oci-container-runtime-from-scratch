//go:build linux

// Package pivot implements the pivot_root(2) dance used by real container
// runtimes (runc/crun) in place of the M1 chroot(2).
//
// Why pivot_root rather than chroot?
//
//   - chroot only changes the filesystem-root pointer for path-resolution
//     lookups; an attacker with CAP_SYS_CHROOT (or any process still holding
//     an fd above the chroot) can break out via the classic "double-chroot
//     ../.." trick, or simply fchdir() to a pre-chroot fd and walk out.
//   - pivot_root actually swaps the process's mount-namespace root so the
//     old root becomes reachable only via an explicit mountpoint we then
//     umount. Once umounted, there is no path from the container back to
//     the host — mount namespaces enforce that structurally, not by
//     convention.
//
// The sequence this package implements is the canonical one recommended in
// pivot_root(2) NOTES and used by runc:
//
//  1. make `/` rprivate so mounts don't propagate to the host
//  2. bind-mount newroot onto itself (pivot_root requires newroot to be a
//     mount point distinct from the current root)
//  3. create newroot/.pivot_old to hold the outgoing root
//  4. chdir(newroot)
//  5. pivot_root(".", ".pivot_old")
//  6. chroot(".")  — belt-and-braces, see below
//  7. umount("/.pivot_old", MNT_DETACH) then rmdir it
//  8. mount a fresh /proc so ps/top/etc. work
//
// The extra chroot("/") after pivot_root isn't strictly required on modern
// kernels but matches runc's behaviour and guards against old bugs where
// the cwd handle kept a reference to the original root. MNT_DETACH (lazy
// umount) is used because something inside the kernel may still hold the
// old root reference briefly; detach lets the pivot succeed immediately
// and the real umount happens when the last ref drops.
//
// This package is Linux-only; the darwin build just stubs Do() with an
// error so the binary still links on macOS.
package pivot

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Do performs the full pivot_root sequence rooted at newroot.
//
// Preconditions:
//   - caller is already inside a new mount namespace (CLONE_NEWNS)
//   - newroot is an absolute path that exists and is a directory
//   - /proc exists inside newroot (empty dir is fine)
//
// On success, the process's root is newroot, cwd is "/", and /proc is
// mounted. The old root has been unmounted and its holding directory
// removed. From here the caller can exec the container entrypoint.
func Do(newroot string) error {
	// 1. Make the whole mount tree private + recursive so nothing we do
	//    below leaks back into the host's mount namespace. This is the
	//    same call runtime_linux.go was already making before chroot.
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("pivot: rprivate /: %w", err)
	}

	// 2. pivot_root requires the new root to be a mount point (and
	//    different from the current root). A bind-mount onto itself is
	//    the lightest way to satisfy that — no data moves.
	if err := syscall.Mount(newroot, newroot, "bind", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("pivot: bind %s: %w", newroot, err)
	}

	// 3. Directory inside newroot that will temporarily host the old root.
	//    We remove it again at the end. A leading dot keeps it out of the
	//    way of normal container processes during the tiny window before
	//    we umount it.
	oldRootRel := ".pivot_old"
	oldRoot := filepath.Join(newroot, oldRootRel)
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return fmt.Errorf("pivot: mkdir %s: %w", oldRoot, err)
	}

	// 4. cwd must be inside newroot for pivot_root's relative-path form.
	if err := os.Chdir(newroot); err != nil {
		return fmt.Errorf("pivot: chdir %s: %w", newroot, err)
	}

	// 5. The actual swap. After this syscall returns:
	//    - "/" now refers to (what was) newroot
	//    - the previous root is accessible at "/" + oldRootRel
	if err := syscall.PivotRoot(".", oldRootRel); err != nil {
		// Clean up the placeholder so the rootfs isn't left littered on
		// a failed pivot (can happen if newroot isn't a mount point, or
		// shares a mount with "/").
		_ = os.Remove(oldRoot)
		return fmt.Errorf("pivot_root: %w", err)
	}

	// 6. Belt-and-braces chroot — matches runc; defends against any
	//    surviving fd that still points at the old root dentry.
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("pivot: post-chroot: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("pivot: chdir /: %w", err)
	}

	// 7. Detach-umount the old root. MNT_DETACH lets the kernel drop the
	//    mount lazily — pivot_root just left it active. Without this,
	//    the container would still see the host's fs tree under
	//    /.pivot_old, defeating the whole point of the pivot.
	oldInNew := "/" + oldRootRel
	if err := syscall.Unmount(oldInNew, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("pivot: umount old root: %w", err)
	}
	if err := os.Remove(oldInNew); err != nil {
		// Non-fatal: the mount is gone, the directory is just a stub.
		// Log via the returned error only if we can't clean it up at all.
		return fmt.Errorf("pivot: rmdir old root: %w", err)
	}

	// 8. Mount /proc so ps/top/stat work. We require /proc to exist in
	//    the rootfs (same contract the M1 chroot path had).
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("pivot: mount /proc: %w", err)
	}
	return nil
}
