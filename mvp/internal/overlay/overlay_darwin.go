//go:build darwin

// Package overlay: macOS stub. OverlayFS is a Linux kernel feature and has
// no macOS equivalent we can realistically emulate, so this file just lets
// `go build ./...` succeed on darwin — the runtime package's darwin stub is
// what actually short-circuits `myrun run` on macOS.
package overlay

import "errors"

var errDarwinUnsupported = errors.New("overlay requires Linux (OverlayFS). Run inside a Linux VM")

// Mount mirrors the Linux type shape so cross-platform callers compile.
type Mount struct {
	ContainerDir string
	Merged       string
}

// MountOverlay is a macOS stub.
func MountOverlay(lowerDirs, containerDir string) (*Mount, error) {
	return nil, errDarwinUnsupported
}

// Unmount is a macOS stub.
func (m *Mount) Unmount() error { return nil }

// Cleanup is a macOS stub.
func (m *Mount) Cleanup() error { return nil }
