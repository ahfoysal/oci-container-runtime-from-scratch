//go:build linux

// Package runtime implements the low-level container-start primitives for
// myrun on Linux: clone with namespace flags, chroot into the provided
// rootfs, mount a fresh /proc so tools like `ps` work, then exec the user's
// command.
package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Run is the parent-side entrypoint invoked by `myrun run ...`. It re-execs
// /proc/self/exe with the `child` subcommand inside new namespaces so the
// child starts as PID 1 in its own PID namespace.
func Run(rootfs, cmd string, args []string) error {
	selfArgs := append([]string{"child", rootfs, cmd}, args...)
	c := exec.Command("/proc/self/exe", selfArgs...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	c.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWNET,
	}

	if err := c.Run(); err != nil {
		return fmt.Errorf("clone/run child: %w", err)
	}
	return nil
}

// Child runs inside the freshly-cloned process. It is already in new
// namespaces; here we set hostname, chroot into rootfs, mount /proc, then
// exec the user-specified command as PID 1.
func Child(rootfs, cmd string, args []string) error {
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
