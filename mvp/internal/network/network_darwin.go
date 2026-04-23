//go:build darwin

// Package network: macOS stub. Linux bridge networking, veth pairs, netns
// manipulation, and iptables are all Linux-specific. This file exists only
// so `go build ./...` stays green on darwin — the runtime package's darwin
// stub already short-circuits `myrun run` on macOS, so these stubs should
// never be invoked at runtime.
package network

import "errors"

var errDarwinUnsupported = errors.New("network requires Linux (bridge/veth/netns/iptables). Run inside a Linux VM")

// PortMapping describes a host-port -> container-port forward. Mirrors the
// Linux shape so main.go can build on both platforms without build tags.
type PortMapping struct {
	HostPort      int
	ContainerPort int
	Protocol      string // "tcp" or "udp"
}

// Config mirrors the Linux struct so cross-platform callers compile.
type Config struct {
	ContainerID  string
	ChildPID     int
	Rootfs       string
	PortMappings []PortMapping
}

// Network is an opaque handle returned by Setup; mirrors Linux shape.
type Network struct {
	ContainerIP string
}

// EnsureBridge is a macOS stub.
func EnsureBridge() error { return errDarwinUnsupported }

// Setup is a macOS stub.
func Setup(cfg Config) (*Network, error) { return nil, errDarwinUnsupported }

// Teardown is a macOS stub.
func (n *Network) Teardown(cfg Config) error { return nil }
