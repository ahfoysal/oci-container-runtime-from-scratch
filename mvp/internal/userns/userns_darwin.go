//go:build darwin

// Package userns: macOS stub. User namespaces are a Linux feature; on
// darwin we compile this stub so the rest of the codebase can import
// the package without build tags.
package userns

import "syscall"

// Config mirrors the Linux struct so callers can share code.
type Config struct {
	Enabled bool
	HostUID int
	HostGID int
	Size    int
}

// Detect always returns a disabled config on darwin — the runtime stub
// already rejects `run` before we get far enough for this to matter.
func Detect(force bool) Config { return Config{} }

// Preflight is a no-op on darwin.
func (c Config) Preflight() error { return nil }

// ApplyToSysProcAttr is a no-op on darwin.
func (c Config) ApplyToSysProcAttr(_ *syscall.SysProcAttr) {}
