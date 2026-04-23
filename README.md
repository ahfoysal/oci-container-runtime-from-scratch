# Container Runtime

**Stack:** Go 1.26 · `syscall` (Linux namespaces + chroot) · `runc`-style libcontainer patterns · OCI image/runtime spec · Linux 6.x · tested in Ubuntu 24.04 VM

> **Note:** Requires Linux. Develop in a Multipass/UTM/Lima VM on macOS.

## Full Vision
OCI-compliant runtime: all namespaces (pid/net/mnt/uts/ipc/user/cgroup), cgroups v2, overlayfs, seccomp+AppArmor, CNI networking, image registry pull, rootless, CRIU checkpoint/restore.

## MVP Status

**Shipped (M1-lite):** `mvp/cmd/myrun` — a Go binary that forks a child into new **PID / MNT / UTS / IPC / NET** namespaces, chroots into a user-supplied rootfs, mounts `/proc`, and execs the requested command as PID 1.

```
mvp/
├── go.mod
├── cmd/myrun/main.go               # CLI entrypoint
├── internal/runtime/
│   ├── runtime_linux.go            # real impl: Cloneflags + chroot + mount /proc + exec
│   └── runtime_darwin.go           # stub: returns "requires Linux"
└── scripts/fetch-busybox-rootfs.sh # pulls busybox via `docker export` into ./rootfs
```

### Build

```sh
cd mvp
go build -o myrun ./cmd/myrun
```

`go build ./...` works on **macOS** (darwin stub) and **Linux** (real implementation). Build tags (`//go:build linux` / `//go:build darwin`) select the correct file.

### Run (Linux only)

1. Prepare a rootfs:
   ```sh
   ./scripts/fetch-busybox-rootfs.sh ./rootfs
   ```
   (Requires Docker. If Docker is unavailable, the script prints a manual tarball fallback using the `docker-library/busybox` GitHub mirror.)

2. Launch:
   ```sh
   sudo ./myrun run ./rootfs /bin/sh
   ```

   Expected output — you land in a shell where:
   ```
   / # hostname
   container
   / # ps
   PID   USER     TIME  COMMAND
       1 root      0:00 /bin/sh
       2 root      0:00 ps
   / # ip link          # only `lo` (down), because NEWNET gave us a fresh netns
   ```

### macOS development

`go build ./...` on macOS compiles the darwin stub; invoking `myrun run ...` there prints:

```
myrun: run failed: myrun runtime requires Linux (namespaces + chroot). Run inside a Linux VM — see README
```

To actually execute containers, spin up a Linux VM. Recommended paths on Apple Silicon:

- **Multipass:** `brew install --cask multipass && multipass launch --name dev 24.04 && multipass shell dev`
- **UTM:** install an Ubuntu 24.04 ARM64 image
- **Lima:** `brew install lima && limactl start template://ubuntu-lts`

Then mount/copy the repo in, `go build ./cmd/myrun`, and run as root.

## Milestones
- **M1 (done — this MVP):** PID/MNT/UTS/IPC/NET namespaces + chroot + `mount /proc` + basic `run`
- **M2:** cgroups v2 resource limits (cpu/mem/pids)
- **M3:** OverlayFS + OCI image pull from Docker Hub
- **M4:** CNI bridge networking + port forwarding
- **M5:** Seccomp profiles + rootless + full OCI spec compliance · USER namespace · pivot_root instead of chroot

## Key References
- OCI Runtime Spec
- `runc` source
- "Containers from Scratch" (Liz Rice talk)
