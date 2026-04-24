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
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/network"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/ocispec"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/userns"
)

var errDarwinUnsupported = errors.New("myrun runtime requires Linux (namespaces + chroot). Run inside a Linux VM — see mvp/README")

// Config mirrors the Linux struct so main.go needs no build-tag branches.
type Config struct {
	Rootfs    string
	Cmd       string
	Args      []string
	Limits    cgroups.Limits
	StoreRoot string // M3: image-store root for pulled images + container scratch.
	// PortMappings mirrors the Linux field so main.go doesn't need build tags.
	// macOS never consumes this — Run returns errDarwinUnsupported first.
	PortMappings []network.PortMapping

	// M5 fields — present for build-tag symmetry; never consulted.
	Rootless userns.Config
	Seccomp  bool
	Spec     *ocispec.Spec
}

// Run is a macOS stub; returns an error explaining the platform limitation.
func Run(cfg Config) error {
	return errDarwinUnsupported
}

// Child is a macOS stub; should never actually be invoked on macOS because
// Run returns before re-execing, but provided for symmetry.
func Child(rootfs, cmd string, args []string, seccompEnabled bool) error {
	return errDarwinUnsupported
}
