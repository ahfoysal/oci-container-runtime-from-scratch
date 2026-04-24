//go:build linux

// Package runtime implements the low-level container-start primitives for
// myrun on Linux: clone with namespace flags, chroot into the provided
// rootfs, mount a fresh /proc so tools like `ps` work, then exec the user's
// command. Since M2 it also attaches the container process to a cgroups v2
// subgroup before exec so resource limits take effect from PID 1 onwards.
// Since M3 it resolves the rootfs argument against the local image store and
// mounts an OverlayFS stack when the argument is an image reference rather
// than a plain directory.
package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/cgroups"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/image"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/network"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/ocispec"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/overlay"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/pivot"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/seccomp"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/slirp"
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/userns"
)

// Config bundles the knobs the parent can pass to Run. Keeping this in one
// struct lets us add more options (user ns, readonly rootfs, etc.) without
// churning the signature on every milestone.
type Config struct {
	// Rootfs is either an existing directory (M1 classic mode) or an image
	// reference previously pulled into StoreRoot (M3 overlay mode).
	Rootfs    string
	Cmd       string
	Args      []string
	Limits    cgroups.Limits
	StoreRoot string
	// PortMappings lists --publish host:container entries. Empty means no
	// DNAT rules are installed; the container still gets a bridge IP and
	// outbound connectivity via MASQUERADE.
	PortMappings []network.PortMapping

	// Rootless enables user-namespace rootless mode. When set, the
	// runtime adds CLONE_NEWUSER, installs uid/gid maps, and skips
	// host-privileged steps (cgroups, bridge networking, iptables).
	Rootless userns.Config

	// Seccomp toggles installation of the default seccomp profile just
	// before exec inside the child. Defaults to true; --no-seccomp flips
	// it off for debugging.
	Seccomp bool

	// Spec, when non-nil, is a parsed OCI runtime config.json. If set,
	// it overrides Cmd/Args/Rootfs/Limits with the spec's values before
	// Run proceeds — allowing myrun to be invoked as a drop-in target
	// for tools that speak the OCI runtime spec.
	Spec *ocispec.Spec
}

// resolveRootfs decides whether cfg.Rootfs is a plain directory or an image
// reference. If it is a directory, we use it as-is. Otherwise we try to load
// its manifest from the store and mount an OverlayFS stack; the caller
// receives the merged dir and a cleanup func.
func resolveRootfs(cfg Config) (rootfs string, cleanup func(), err error) {
	// Plain directory takes precedence — lets users keep using the M1 flow.
	if fi, serr := os.Stat(cfg.Rootfs); serr == nil && fi.IsDir() {
		return cfg.Rootfs, func() {}, nil
	}

	ref, perr := image.ParseRef(cfg.Rootfs)
	if perr != nil {
		return "", nil, fmt.Errorf("rootfs %q is neither a directory nor a valid image ref: %w", cfg.Rootfs, perr)
	}
	store, serr := image.OpenStore(cfg.StoreRoot)
	if serr != nil {
		return "", nil, serr
	}
	info, lerr := store.LoadManifest(ref)
	if lerr != nil {
		return "", nil, fmt.Errorf("image %s not in local store — run `myrun pull %s` first: %w", ref, ref, lerr)
	}

	// Fresh per-container scratch dir under <store>/containers/<id>/.
	id := newContainerID()
	containerDir := filepath.Join(cfg.StoreRoot, "containers", id)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return "", nil, err
	}

	mnt, merr := overlay.MountOverlay(info.OverlayLowerDirs(), containerDir)
	if merr != nil {
		os.RemoveAll(containerDir)
		return "", nil, merr
	}

	cleanup = func() {
		if err := mnt.Unmount(); err != nil {
			log.Printf("overlay: unmount: %v", err)
		}
		if err := mnt.Cleanup(); err != nil {
			log.Printf("overlay: cleanup: %v", err)
		}
	}
	return mnt.Merged, cleanup, nil
}

// applySpec folds an OCI runtime spec into the runtime Config. CLI flags
// that were already non-zero win over the spec — callers can layer
// ad-hoc overrides on top of a base spec. We intentionally only pull
// the fields we can honour end-to-end; everything else (seccomp per-
// syscall rules, capability bounding sets, mount order) is either
// handled elsewhere (our default seccomp profile) or surfaced as a log
// warning so the user knows we're ignoring it.
func applySpec(cfg *Config) {
	s := cfg.Spec
	if s.Process != nil && len(s.Process.Args) > 0 {
		if cfg.Cmd == "" {
			cfg.Cmd = s.Process.Args[0]
			if len(s.Process.Args) > 1 {
				cfg.Args = s.Process.Args[1:]
			}
		}
	}
	if s.Root != nil && cfg.Rootfs == "" {
		cfg.Rootfs = s.Root.Path
	}
	if s.Linux != nil && s.Linux.Resources != nil {
		r := s.Linux.Resources
		if r.Memory != nil && cfg.Limits.MemoryBytes == 0 {
			cfg.Limits.MemoryBytes = r.Memory.Limit
		}
		if r.CPU != nil && cfg.Limits.CPUQuota == 0 && r.CPU.Quota > 0 {
			period := r.CPU.Period
			if period == 0 {
				period = 100000
			}
			cfg.Limits.CPUQuota = float64(r.CPU.Quota) / float64(period)
		}
		if r.Pids != nil && cfg.Limits.PidsMax == 0 {
			cfg.Limits.PidsMax = r.Pids.Limit
		}
	}
	if s.Linux != nil && !cfg.Rootless.Enabled {
		// If the spec asked for a user namespace via linux.namespaces
		// and supplied uid/gid mappings, honour it.
		for _, ns := range s.Linux.Namespaces {
			if ns.Type == "user" && len(s.Linux.UIDMappings) > 0 && len(s.Linux.GIDMappings) > 0 {
				um := s.Linux.UIDMappings[0]
				gm := s.Linux.GIDMappings[0]
				cfg.Rootless = userns.Config{
					Enabled: true,
					HostUID: int(um.HostID),
					HostGID: int(gm.HostID),
					Size:    int(um.Size),
				}
				break
			}
		}
	}
	if s.Linux != nil && s.Linux.Seccomp != nil {
		// Spec asked for seccomp: honour it by installing our default
		// profile. Per-syscall action tables in the spec are noted but
		// not applied — see package doc.
		cfg.Seccomp = true
	}
}

// newContainerID returns a short random hex id for the container scratch dir.
func newContainerID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Run is the parent-side entrypoint invoked by `myrun run ...`. It:
//  1. resolves the rootfs (plain dir, or image ref -> OverlayFS merged dir)
//  2. starts /proc/self/exe child inside new namespaces (paused on a sync pipe)
//  3. creates a cgroup keyed on the child PID and writes the requested limits
//  4. adds the child PID to cgroup.procs
//  5. closes the sync pipe so the child continues into chroot + exec
//  6. waits for the child, then removes the cgroup and overlay mount
func Run(cfg Config) error {
	// M5: if an OCI spec is provided, fold its values into cfg before
	// any work happens. Fields the caller already set win ties — the
	// spec only fills holes. This keeps `myrun run --spec config.json`
	// composable with the existing CLI flags.
	if cfg.Spec != nil {
		applySpec(&cfg)
	}

	// M5: rootless preflight — surface the exact sysctl to flip before
	// we blow up inside clone(). No-op when Rootless.Enabled is false.
	if err := cfg.Rootless.Preflight(); err != nil {
		return err
	}

	rootfs, cleanupRootfs, err := resolveRootfs(cfg)
	if err != nil {
		return err
	}
	defer cleanupRootfs()

	// Container id is used to name veth interfaces + tag iptables rules so
	// teardown can find them. We generate fresh every run even in classic
	// M1 mode where the rootfs is a plain directory.
	containerID := newContainerID()

	// syncPipe: parent keeps the write end; child reads from fd 3. The child
	// blocks on a read until the parent closes the pipe — that happens only
	// after the cgroup is fully set up with the child already inside it.
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	defer w.Close()

	// The child argv gets a leading flag that tells it whether seccomp
	// should be applied — we can't easily pass Config through re-exec,
	// so a positional prefix "seccomp=1" / "seccomp=0" does the job.
	seccompArg := "seccomp=0"
	if cfg.Seccomp {
		seccompArg = "seccomp=1"
	}
	selfArgs := append([]string{"child", seccompArg, rootfs, cfg.Cmd}, cfg.Args...)
	c := exec.Command("/proc/self/exe", selfArgs...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	// ExtraFiles[0] is mapped to fd 3 in the child.
	c.ExtraFiles = []*os.File{r}

	c.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWNET,
	}
	// M5: layer on CLONE_NEWUSER + uid/gid maps if rootless.
	cfg.Rootless.ApplyToSysProcAttr(c.SysProcAttr)

	if err := c.Start(); err != nil {
		r.Close()
		return fmt.Errorf("start child: %w", err)
	}
	// Parent no longer needs the read end.
	r.Close()

	var cg *cgroups.Cgroup
	// In rootless mode we skip cgroups entirely — delegated cgroups via
	// systemd-run are the production answer but require systemd + user
	// cgroup delegation, which is out of scope for this milestone.
	if cfg.Rootless.Enabled && cfg.Limits.Any() {
		log.Printf("rootless: ignoring resource limits (cgroups need a systemd delegation setup)")
	}
	if !cfg.Rootless.Enabled && cfg.Limits.Any() {
		cg, err = cgroups.Create(c.Process.Pid, cfg.Limits)
		if err != nil {
			// Kill the paused child; no point proceeding without limits the
			// user explicitly asked for.
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
			return fmt.Errorf("create cgroup: %w", err)
		}
		if err := cg.AddPID(c.Process.Pid); err != nil {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
			_ = cg.Close()
			return fmt.Errorf("add pid to cgroup: %w", err)
		}
	}

	// M4: network setup. The child was cloned with CLONE_NEWNET so it
	// already has its own netns; we create a veth pair, attach the host
	// side to the `myrun0` bridge, move the peer into the child's netns
	// (addressable as /proc/<pid>/ns/net via `ip link set ... netns <pid>`),
	// and configure the peer's IP/route from the parent using `ip -n`.
	// After this, when the child proceeds past the sync pipe, its netns
	// already has eth0 up with a live default route.
	// In rootless mode, we can't open /sys/class/net or call iptables —
	// so we skip bridge setup and let the container come up with only
	// the loopback interface that CLONE_NEWNET gave it for free. A
	// future milestone could integrate slirp4netns for user-mode
	// outbound connectivity.
	var netHandle *network.Network
	var netCfg network.Config
	var slirpHandle *slirp.Handle
	if !cfg.Rootless.Enabled {
		netCfg = network.Config{
			ContainerID:  containerID,
			ChildPID:     c.Process.Pid,
			Rootfs:       rootfs,
			PortMappings: cfg.PortMappings,
		}
		var nerr error
		netHandle, nerr = network.Setup(netCfg)
		if nerr != nil {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
			if cg != nil {
				_ = cg.Close()
			}
			return fmt.Errorf("network setup: %w", nerr)
		}
	} else {
		// M6: rootless containers get a userspace NAT via slirp4netns.
		// We spawn it now — after the child exists (so /proc/<pid>/ns/net
		// is resolvable) but before we close the sync pipe (so the TAP
		// device is guaranteed present by the time the child execs).
		// If slirp4netns isn't installed we fall back to loopback-only
		// and print the install hint rather than aborting: preserves
		// the M5 behaviour for users who deliberately want no network.
		sh, serr := slirp.Setup(slirp.Config{ChildPID: c.Process.Pid, Rootfs: rootfs})
		if serr != nil {
			log.Printf("rootless: slirp4netns unavailable (%v); container will have lo only", serr)
		} else {
			slirpHandle = sh
			log.Printf("rootless: slirp4netns attached — container IP %s via %s", slirp.ContainerIP, slirp.TAPDevice)
		}
	}

	// Release the child — it will now chroot and exec the user command
	// from inside the cgroup with a live network interface already present.
	w.Close()

	waitErr := c.Wait()

	// Always try to clean up the cgroup, even on error, so repeated runs
	// don't leave a trail of myrun-<pid> dirs under /sys/fs/cgroup.
	if cg != nil {
		if cerr := cg.Close(); cerr != nil {
			log.Printf("cgroups: cleanup: %v", cerr)
		}
	}

	// Tear down the network regardless of exit status. The host veth is
	// deleted (which also removes the peer) and all iptables rules we
	// tagged with this container's id are removed. We leave the `myrun0`
	// bridge in place so subsequent runs can reuse it.
	if netHandle != nil {
		if nerr := netHandle.Teardown(netCfg); nerr != nil {
			log.Printf("network: teardown: %v", nerr)
		}
	}
	// M6: reap the slirp4netns subprocess. No-op if we didn't start one.
	if slirpHandle != nil {
		if serr := slirpHandle.Teardown(); serr != nil {
			log.Printf("slirp: teardown: %v", serr)
		}
	}

	if waitErr != nil {
		return fmt.Errorf("child exited: %w", waitErr)
	}
	return nil
}

// Child runs inside the freshly-cloned process. It is already in new
// namespaces; we block on fd 3 until the parent has placed us in our
// cgroup, then do the chroot/mount/exec dance as PID 1.
//
// The seccompEnabled flag is parsed from the first re-exec arg ("seccomp=1"
// or "seccomp=0") by main.go and threaded through here so the decision
// travels across the re-exec boundary without touching env or spec files.
func Child(rootfs, cmd string, args []string, seccompEnabled bool) error {
	// Wait for the parent to finish cgroup setup. fd 3 is the read end of
	// the sync pipe passed via ExtraFiles; when the parent closes its write
	// end, our Read returns EOF and we proceed.
	sync := os.NewFile(3, "myrun-sync")
	if sync != nil {
		buf := make([]byte, 1)
		_, _ = sync.Read(buf) // EOF expected
		sync.Close()
	}

	if err := syscall.Sethostname([]byte("container")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	// M6: pivot_root replaces the M1 chroot. pivot.Do handles the full
	// sequence (rprivate /, bind newroot, mkdir .pivot_old, pivot_root,
	// chroot("."), umount old root, mount /proc). After it returns, the
	// host's filesystem is structurally unreachable from this netns/mntns.
	if err := pivot.Do(rootfs); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	defer func() {
		// pivot.Do already mounted /proc; unmount on exit so the host's
		// cleanup isn't left holding a mount inside a dead namespace.
		_ = syscall.Unmount("/proc", 0)
	}()

	// Resolve command in the new root's PATH.
	bin, err := exec.LookPath(cmd)
	if err != nil {
		// Fall back to the literal path — useful for absolute paths like /bin/sh.
		bin = cmd
	}

	argv := append([]string{bin}, args...)
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm",
		"HOME=/root",
	}

	// M5: install the seccomp filter as the very last step before exec.
	// The filter denies a bunch of syscalls the Go runtime itself might
	// call during init, so doing this any earlier would break us.
	if seccompEnabled {
		if err := seccomp.Apply(); err != nil {
			return fmt.Errorf("seccomp apply: %w", err)
		}
	}

	if err := syscall.Exec(bin, argv, env); err != nil {
		return fmt.Errorf("exec %q: %w", bin, err)
	}
	return nil
}
