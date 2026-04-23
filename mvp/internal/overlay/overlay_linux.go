//go:build linux

// Package overlay mounts an OverlayFS stack for a container's rootfs:
//
//   - lowerdirs: the read-only image layers (topmost first, base last)
//   - upperdir:  a per-container writable layer where all changes land
//   - workdir:   overlay's scratch space (must be on the same fs as upperdir)
//   - merged:    the unified view mounted and used as the container rootfs
//
// The caller is responsible for eventually invoking Unmount to clean up.
package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Mount is a live OverlayFS mount. Unmount removes the merged mount and
// (optionally) the container-scratch directory.
type Mount struct {
	// ContainerDir is the per-container directory holding upper/work/merged.
	ContainerDir string
	// Merged is the mountpoint the caller should use as the container rootfs.
	Merged string
}

// Mount creates upperdir/workdir/merged under containerDir and mounts an
// overlay with the given lowerdirs (colon-separated, topmost first).
func MountOverlay(lowerDirs string, containerDir string) (*Mount, error) {
	if lowerDirs == "" {
		return nil, fmt.Errorf("overlay: lowerdirs is empty")
	}
	upper := filepath.Join(containerDir, "upper")
	work := filepath.Join(containerDir, "work")
	merged := filepath.Join(containerDir, "merged")
	for _, d := range []string{upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Overlay rejects colons/commas inside any dir path, but a standard
	// myrun install under data/containers/<id> will not produce them.
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerDirs, upper, work)
	if err := syscall.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return nil, fmt.Errorf("mount overlay on %s: %w (opts=%s)", merged, err, truncate(opts, 200))
	}
	return &Mount{ContainerDir: containerDir, Merged: merged}, nil
}

// Unmount lazily unmounts the merged dir. Upper/work are preserved so the
// caller can snapshot/inspect them post-exit; they live under ContainerDir.
func (m *Mount) Unmount() error {
	if m == nil || m.Merged == "" {
		return nil
	}
	// MNT_DETACH (= 2) — safe even if processes still have cwd there.
	if err := syscall.Unmount(m.Merged, 2); err != nil {
		return fmt.Errorf("unmount overlay %s: %w", m.Merged, err)
	}
	return nil
}

// Cleanup removes the entire container scratch dir (upper + work + merged).
// Call this after Unmount when you don't need to keep the writable layer.
func (m *Mount) Cleanup() error {
	if m == nil || m.ContainerDir == "" {
		return nil
	}
	return os.RemoveAll(m.ContainerDir)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(" + strings.TrimSpace(fmt.Sprintf("%d", len(s)-n)) + " more)"
}
