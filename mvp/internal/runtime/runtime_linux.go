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
	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/overlay"
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

	selfArgs := append([]string{"child", rootfs, cfg.Cmd}, cfg.Args...)
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

	if err := c.Start(); err != nil {
		r.Close()
		return fmt.Errorf("start child: %w", err)
	}
	// Parent no longer needs the read end.
	r.Close()

	var cg *cgroups.Cgroup
	if cfg.Limits.Any() {
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
	netCfg := network.Config{
		ContainerID:  containerID,
		ChildPID:     c.Process.Pid,
		Rootfs:       rootfs,
		PortMappings: cfg.PortMappings,
	}
	netHandle, nerr := network.Setup(netCfg)
	if nerr != nil {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
		if cg != nil {
			_ = cg.Close()
		}
		return fmt.Errorf("network setup: %w", nerr)
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

	if waitErr != nil {
		return fmt.Errorf("child exited: %w", waitErr)
	}
	return nil
}

// Child runs inside the freshly-cloned process. It is already in new
// namespaces; we block on fd 3 until the parent has placed us in our
// cgroup, then do the chroot/mount/exec dance as PID 1.
func Child(rootfs, cmd string, args []string) error {
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

	// Make mount propagation private so our mounts inside the container
	// don't leak to the host (and vice versa).
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make-rprivate /: %w", err)
	}

	if err := syscall.Chroot(rootfs); err != nil {
		return fmt.Errorf("chroot %q: %w", rootfs, err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}

	// /proc must exist inside the rootfs for this to succeed. Image-pulled
	// rootfs already ships /proc as an empty dir; for manually-prepared
	// rootfs dirs the user is expected to create it.
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	defer func() {
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
	if err := syscall.Exec(bin, argv, env); err != nil {
		return fmt.Errorf("exec %q: %w", bin, err)
	}
	return nil
}
