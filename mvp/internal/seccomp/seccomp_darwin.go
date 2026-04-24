//go:build darwin

// Package seccomp: macOS stub. Seccomp is a Linux-only facility (it sits
// on top of the Linux BPF infrastructure and the SECCOMP_SET_MODE_FILTER
// syscall). On darwin we just return nil so the runtime can still be
// built and its CLI tested — actual enforcement only happens in the Linux
// build.
package seccomp

// Apply is a no-op on darwin.
func Apply() error { return nil }

// DefaultAllowList returns an empty slice on darwin so callers that
// introspect the profile don't crash on nil.
func DefaultAllowList() []uint32 { return nil }
