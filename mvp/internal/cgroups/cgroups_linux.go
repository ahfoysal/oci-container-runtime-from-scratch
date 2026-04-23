//go:build linux

// Package cgroups provides minimal cgroups v2 resource-limit support for
// myrun. It creates a dedicated subgroup under /sys/fs/cgroup, writes the
// requested memory/cpu/pids limits, adds the container PID to cgroup.procs,
// and tears the subgroup down on container exit.
//
// This is intentionally small — it assumes a unified (cgroups v2) hierarchy
// mounted at /sys/fs/cgroup, which is the default on modern systemd-based
// distros (Ubuntu 22.04+, Fedora, Arch, Debian 12+). If the host is still on
// v1 or a hybrid layout, writes will fail with a descriptive error.
package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// root is the cgroups v2 unified hierarchy mount point. Callers may override
// via the MYRUN_CGROUP_ROOT env var (mainly for tests).
const defaultRoot = "/sys/fs/cgroup"

// Limits captures the optional resource caps myrun can apply to a container.
// Zero / empty values mean "do not set" — the kernel default (max) applies.
type Limits struct {
	// MemoryBytes is the hard memory ceiling written to memory.max. 0 = unset.
	MemoryBytes int64
	// CPUQuota is cores-worth of CPU (e.g. 0.5 = half a core). 0 = unset.
	// Encoded as "<quota> <period>" where period is 100000us.
	CPUQuota float64
	// PidsMax is the max number of PIDs in the cgroup. 0 = unset.
	PidsMax int64
}

// Any reports whether any limit is set — callers can skip cgroup setup
// entirely when no limits are requested.
func (l Limits) Any() bool {
	return l.MemoryBytes > 0 || l.CPUQuota > 0 || l.PidsMax > 0
}

// Cgroup is a handle to a created v2 subgroup. Close() removes it.
type Cgroup struct {
	Path string // absolute path, e.g. /sys/fs/cgroup/myrun-12345
}

func root() string {
	if r := os.Getenv("MYRUN_CGROUP_ROOT"); r != "" {
		return r
	}
	return defaultRoot
}

// Create makes a fresh subgroup named "myrun-<pid>" under the v2 root and
// writes the requested limits. The caller must later invoke AddPID (to put
// the container into the group) and Close (to remove it).
//
// Enabling controllers: on many distros you must write "+memory +cpu +pids"
// to the parent's cgroup.subtree_control before child groups can use those
// controllers. We try that best-effort; if it fails (e.g. because the
// controllers are already enabled or the caller lacks permission on the
// root group), we continue — the per-controller writes below will surface
// the real error.
func Create(ownerPID int, lim Limits) (*Cgroup, error) {
	name := fmt.Sprintf("myrun-%d", ownerPID)
	path := filepath.Join(root(), name)

	// Best-effort: ask the parent cgroup to delegate the controllers we need.
	_ = os.WriteFile(
		filepath.Join(root(), "cgroup.subtree_control"),
		[]byte("+memory +cpu +pids"),
		0644,
	)

	if err := os.Mkdir(path, 0755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("mkdir cgroup %q: %w (is this a cgroups v2 host? run as root?)", path, err)
	}

	cg := &Cgroup{Path: path}

	if lim.MemoryBytes > 0 {
		if err := writeFile(path, "memory.max", strconv.FormatInt(lim.MemoryBytes, 10)); err != nil {
			_ = cg.Close()
			return nil, err
		}
	}
	if lim.CPUQuota > 0 {
		// cpu.max format: "<quota_us> <period_us>". Period 100000 = 100ms.
		const period = 100000
		quota := int64(lim.CPUQuota * float64(period))
		if quota < 1000 {
			quota = 1000 // floor to avoid 0 → "max"
		}
		val := fmt.Sprintf("%d %d", quota, period)
		if err := writeFile(path, "cpu.max", val); err != nil {
			_ = cg.Close()
			return nil, err
		}
	}
	if lim.PidsMax > 0 {
		if err := writeFile(path, "pids.max", strconv.FormatInt(lim.PidsMax, 10)); err != nil {
			_ = cg.Close()
			return nil, err
		}
	}
	return cg, nil
}

// AddPID moves pid into this cgroup by writing to cgroup.procs. All of pid's
// future children inherit the cgroup automatically.
func (c *Cgroup) AddPID(pid int) error {
	return writeFile(c.Path, "cgroup.procs", strconv.Itoa(pid))
}

// Close removes the cgroup directory. Safe to call multiple times; errors
// are returned but the caller typically just logs them.
func (c *Cgroup) Close() error {
	if c == nil || c.Path == "" {
		return nil
	}
	// Note: rmdir only works once the group is empty (all procs exited).
	if err := os.Remove(c.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cgroup %q: %w", c.Path, err)
	}
	return nil
}

func writeFile(dir, name, content string) error {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		return fmt.Errorf("write %s=%q: %w", p, content, err)
	}
	return nil
}
