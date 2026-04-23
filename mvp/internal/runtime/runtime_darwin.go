//go:build darwin

// Package runtime: macOS stub. The container primitives used by myrun
// (clone() with namespace flags, chroot + /proc mount) are Linux-only. This
// stub lets the binary compile on macOS for development so you can iterate
// on CLI parsing, types, and structure — but actual container execution
// must happen inside a Linux VM (Multipass, UTM, Lima, Docker Desktop VM).
package runtime

import (
	"errors"

	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/cgroups"
)

var errDarwinUnsupported = errors.New("myrun runtime requires Linux (namespaces + chroot). Run inside a Linux VM — see mvp/README")

// Config mirrors the Linux struct so main.go needs no build-tag branches.
type Config struct {
	Rootfs    string
	Cmd       string
	Args      []string
	Limits    cgroups.Limits
	StoreRoot string // M3: image-store root for pulled images + container scratch.
}

// Run is a macOS stub; returns an error explaining the platform limitation.
func Run(cfg Config) error {
	return errDarwinUnsupported
}

// Child is a macOS stub; should never actually be invoked on macOS because
// Run returns before re-execing, but provided for symmetry.
func Child(rootfs, cmd string, args []string) error {
	return errDarwinUnsupported
}
