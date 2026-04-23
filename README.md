# 03 — Container Runtime

**Stack:** Go 1.22 · `golang.org/x/sys/unix` (syscalls) · `runc`-style libcontainer patterns · OCI image/runtime spec · Linux 6.x (kernel features) · tested in Ubuntu 24.04 VM

> **Note:** Requires Linux. Develop in a Multipass/UTM VM on macOS.

## Full Vision
OCI-compliant runtime: all namespaces (pid/net/mnt/uts/ipc/user/cgroup), cgroups v2, overlayfs, seccomp+AppArmor, CNI networking, image registry pull, rootless, CRIU checkpoint/restore.

## MVP (1 weekend)
`myrun run <image> <cmd>` — new PID+mount namespace, chroot to extracted rootfs, exec command.

## Milestones
- **M1:** All 6 namespaces + chroot + basic `run`
- **M2:** cgroups v2 resource limits (cpu/mem/pids)
- **M3:** OverlayFS + OCI image pull from Docker Hub
- **M4:** CNI bridge networking + port forwarding
- **M5:** Seccomp profiles + rootless + full OCI spec compliance

## Key References
- OCI Runtime Spec
- `runc` source
- "Containers from Scratch" (Liz Rice talk)
