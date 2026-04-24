//go:build darwin

// Package pivot: macOS stub. pivot_root(2) is a Linux syscall, so on
// darwin we just surface a clear error — the runtime package never calls
// Do() on macOS (it errors out earlier with the generic "requires Linux"
// message), but we keep the symbol around so the package imports cleanly.
package pivot

import "errors"

// Do is a no-op stub on darwin; returns a sentinel error if ever called.
func Do(newroot string) error {
	return errors.New("pivot_root requires Linux")
}
