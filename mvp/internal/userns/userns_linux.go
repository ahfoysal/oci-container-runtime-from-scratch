//go:build linux

// Package userns wires up rootless container support using Linux user
// namespaces. The high-level idea: when myrun is invoked by a non-root
// user with --rootless, we add CLONE_NEWUSER to the clone flags, then
// write uid_map/gid_map files in /proc/<child>/ for the child namespace
// so that uid 0 inside the container maps to the invoking user's real
// uid on the host.
//
// The tricky bits the Go standard library already handles for us if we
// use exec.Cmd: SysProcAttr.UidMappings / GidMappings will populate
// those /proc files at the right moment (after clone, before exec). We
// still need to:
//
//   - deny setgroups first (otherwise writing gid_map is forbidden when
//     the caller lacks CAP_SETGID in the parent ns — the common case
//     for single-entry identity maps from a non-privileged user).
//   - refuse to proceed if /proc/sys/kernel/unprivileged_userns_clone is
//     disabled, with a pointer to the sysctl the user needs to flip.
//   - drop the network and cgroup setup steps that need real root,
//     because even in a user ns our "root" can't open /sys/fs/cgroup or
//     call iptables.
//
// None of the enforcement lives in this file — the runtime package
// consults Config().Enabled and skips the network/cgroup code paths.
// Here we only provide helpers + sanity checks.
package userns

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Config describes the rootless mapping. A zero value means "not
// rootless" (classic M1-M4 behaviour with real root on the host).
type Config struct {
	// Enabled toggles the whole feature. When false all other fields
	// are ignored. Driven by the --rootless flag in main.go.
	Enabled bool

	// HostUID / HostGID are the IDs on the host that will appear as
	// uid 0 / gid 0 inside the container. We default these to the
	// caller's real uid/gid (os.Getuid/Getgid) — see Detect.
	HostUID int
	HostGID int

	// Size is how many IDs to map. 1 is enough for single-user
	// containers. Full sub-uid/sub-gid delegation from /etc/subuid is
	// out of scope for this milestone but would plug in here.
	Size int
}

// Detect fills a Config from the environment: if the process is not
// running as real root, we default to Enabled=true with Host{UID,GID}
// set to the calling user. If the user passed --rootless explicitly,
// they can pre-populate Enabled and we just fill in the IDs.
func Detect(force bool) Config {
	cfg := Config{
		Enabled: force || os.Geteuid() != 0,
		HostUID: os.Getuid(),
		HostGID: os.Getgid(),
		Size:    1,
	}
	return cfg
}

// Preflight bails out early if the kernel can't honour a rootless
// launch. We check the two sysctls that matter and surface a friendly
// message rather than a cryptic EPERM mid-clone. Skipped when not in
// rootless mode.
func (c Config) Preflight() error {
	if !c.Enabled {
		return nil
	}
	// Ubuntu 24.04 ships this enabled by default; older distros may
	// have it off. The file is one byte: "1\n" or "0\n".
	if v, err := readIntFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil && v == 0 {
		return fmt.Errorf("rootless requires unprivileged user namespaces: " +
			"enable via `sudo sysctl -w kernel.unprivileged_userns_clone=1`")
	}
	// kernel.userns_max_user_namespaces — defaults to a large number
	// but can be zero on hardened kernels.
	if v, err := readIntFile("/proc/sys/user/max_user_namespaces"); err == nil && v == 0 {
		return fmt.Errorf("rootless requires user namespaces: " +
			"`sudo sysctl -w user.max_user_namespaces=15000`")
	}
	return nil
}

// ApplyToSysProcAttr wires the rootless configuration into an existing
// SysProcAttr so exec.Cmd.Start can do the right thing: add the USER
// clone flag, set the uid/gid mappings, and deny setgroups (required
// by the kernel before writing gid_map from an unprivileged writer).
func (c Config) ApplyToSysProcAttr(sa *syscall.SysProcAttr) {
	if !c.Enabled {
		return
	}
	sa.Cloneflags |= syscall.CLONE_NEWUSER
	sa.UidMappings = []syscall.SysProcIDMap{{
		ContainerID: 0,
		HostID:      c.HostUID,
		Size:        c.Size,
	}}
	sa.GidMappings = []syscall.SysProcIDMap{{
		ContainerID: 0,
		HostID:      c.HostGID,
		Size:        c.Size,
	}}
	sa.GidMappingsEnableSetgroups = false
}

// readIntFile reads /proc/sys/... files that contain a single integer.
// Everything we peek at follows that shape.
func readIntFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}
