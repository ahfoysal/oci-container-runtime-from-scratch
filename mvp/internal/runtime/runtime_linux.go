//go:build linux

// Package runtime implements the low-level container-start primitives for
// myrun on Linux: clone with namespace flags, chroot into the provided
// rootfs, mount a fresh /proc so tools like `ps` work, then exec the user's
// command. Since M2 it also attaches the container process to a cgroups v2
// subgroup before exec so resource limits take effect from PID 1 onwards.
package runtime

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"

	"github.com/ahfoysal/oci-container-runtime-from-scratch/mvp/internal/cgroups"
)

// Config bundles the knobs the parent can pass to Run. Keeping this in one
// struct lets us add more options (user ns, readonly rootfs, etc.) without
// churning the signature on every milestone.
type Config struct {
	Rootfs string
	Cmd    string
	Args   []string
	Limits cgroups.Limits
}

// Run is the parent-side entrypoint invoked by `myrun run ...`. It:
//  1. starts /proc/self/exe child inside new namespaces (paused on a sync pipe)
//  2. creates a cgroup keyed on the child PID and writes the requested limits
//  3. adds the child PID to cgroup.procs
//  4. closes the sync pipe so the child continues into chroot + exec
//  5. waits for the child, then removes the cgroup
func Run(cfg Config) error {
	// syncPipe: parent keeps the write end; child reads from fd 3. The child
	// blocks on a read until the parent closes the pipe — that happens only
	// after the cgroup is fully set up with the child already inside it.
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	defer w.Close()

	selfArgs := append([]string{"child", cfg.Rootfs, cfg.Cmd}, cfg.Args...)
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

	// Release the child — it will now chroot and exec the user command
	// from inside the cgroup.
	w.Close()

	waitErr := c.Wait()

	// Always try to clean up the cgroup, even on error, so repeated runs
	// don't leave a trail of myrun-<pid> dirs under /sys/fs/cgroup.
	if cg != nil {
		if cerr := cg.Close(); cerr != nil {
			log.Printf("cgroups: cleanup: %v", cerr)
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

	// /proc must exist inside the rootfs for this to succeed.
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
