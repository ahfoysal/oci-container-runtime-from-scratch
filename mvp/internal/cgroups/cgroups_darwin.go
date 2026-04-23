//go:build darwin

// Package cgroups: macOS stub. cgroups are Linux-only; on darwin we keep the
// same public API but every call logs a one-line notice and returns nil so
// the rest of the runtime can compile and run (no-op) during dev iteration.
package cgroups

import "log"

// Limits mirrors the Linux struct so callers need no build-tag branching.
type Limits struct {
	MemoryBytes int64
	CPUQuota    float64
	PidsMax     int64
}

// Any reports whether any limit is set.
func (l Limits) Any() bool {
	return l.MemoryBytes > 0 || l.CPUQuota > 0 || l.PidsMax > 0
}

// Cgroup is an opaque no-op handle on darwin.
type Cgroup struct{ Path string }

// Create logs a skip notice and returns an empty handle.
func Create(ownerPID int, lim Limits) (*Cgroup, error) {
	if lim.Any() {
		log.Printf("cgroups: darwin stub — skipping resource limits (mem=%d cpu=%.2f pids=%d). Test on Linux.",
			lim.MemoryBytes, lim.CPUQuota, lim.PidsMax)
	}
	return &Cgroup{}, nil
}

// AddPID is a no-op on darwin.
func (c *Cgroup) AddPID(pid int) error { return nil }

// Close is a no-op on darwin.
func (c *Cgroup) Close() error { return nil }
