//go:build darwin

// Package slirp: macOS stub. slirp4netns is a Linux userspace tool that
// needs /proc/<pid>/ns/net to join; there is no equivalent on darwin.
// The runtime never calls Setup here (it errors out earlier) — this file
// exists so `go build ./...` stays green.
package slirp

import "errors"

// Config mirrors the Linux struct for cross-platform symmetry.
type Config struct {
	ChildPID int
	Rootfs   string
}

// Handle is an empty placeholder on darwin.
type Handle struct{}

// Setup always returns a sentinel error on darwin.
func Setup(cfg Config) (*Handle, error) {
	return nil, errors.New("slirp4netns requires Linux")
}

// Teardown is a no-op on darwin.
func (h *Handle) Teardown() error { return nil }
