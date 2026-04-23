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

## M2 Status

**Shipped:** cgroups v2 resource limits for memory, CPU, and PID count, wired into `myrun run` via `--memory`, `--cpu`, and `--pids` flags.

```
mvp/internal/cgroups/
├── cgroups_linux.go     # creates /sys/fs/cgroup/myrun-<pid>/, writes memory.max / cpu.max / pids.max, cleans up on exit
└── cgroups_darwin.go    # no-op stub that logs and continues
```

**Flow** (Linux): parent starts the child paused on a sync pipe, creates `/sys/fs/cgroup/myrun-<child-pid>/`, writes the requested limits, writes the child PID to `cgroup.procs`, then closes the pipe. The child then chroots and execs the user command — already inside the cgroup, so every descendant inherits the limits. On child exit the parent `rmdir`s the subgroup.

**CLI:**

```sh
sudo ./myrun run --memory=64M --cpu=0.5 --pids=100 ./rootfs /bin/sh
```

### Linux test instructions (memory OOM kill)

Requires a cgroups v2 host (Ubuntu 22.04+, Fedora, Debian 12+). Inside your Linux VM:

1. Build and prep rootfs:
   ```sh
   cd mvp
   go build -o myrun ./cmd/myrun
   ./scripts/fetch-busybox-rootfs.sh ./rootfs
   ```

2. Run with a 64 MiB memory ceiling:
   ```sh
   sudo ./myrun run --memory=64M ./rootfs /bin/sh
   ```

3. Inside the container, try to allocate more than the cap. The simplest busybox-only option:
   ```sh
   # inside container — should OOM-kill around 64 MiB
   dd if=/dev/zero of=/dev/shm/balloon bs=1M count=128
   ```
   Expected: the process is terminated with `Killed`, and `dmesg` on the host shows `Memory cgroup out of memory: ... oom-kill:constraint=CONSTRAINT_MEMCG`.

4. Verify the limits are live from another host shell while the container runs (replace `<pid>` with the container's host-side PID):
   ```sh
   cat /sys/fs/cgroup/myrun-<pid>/memory.max   # 67108864
   cat /sys/fs/cgroup/myrun-<pid>/cpu.max      # 50000 100000   (if --cpu=0.5)
   cat /sys/fs/cgroup/myrun-<pid>/pids.max     # 100            (if --pids=100)
   cat /sys/fs/cgroup/myrun-<pid>/cgroup.procs # lists the container PID
   ```

5. After the container exits, the `myrun-<pid>` directory is removed automatically.

**macOS:** cgroup operations are stubbed — `go build ./...` still succeeds, and passing `--memory`/`--cpu`/`--pids` logs a one-line skip notice before the runtime itself errors out with the existing "requires Linux" message.

## M3 Status

**Shipped:** OCI image pull from Docker Hub (stdlib HTTP only, no external tools) and OverlayFS-backed container rootfs, wired into `myrun`.

```
mvp/internal/image/
├── pull.go             # Docker Hub v2 client: token -> manifest -> layer blobs, tar+gzip extract
└── store.go            # content-addressed blob cache + per-image extracted-layer tree

mvp/internal/overlay/
├── overlay_linux.go    # mounts lowerdir=<image layers>,upperdir,workdir -> merged rootfs
└── overlay_darwin.go   # stub so `go build ./...` stays green on macOS
```

**Flow:**

1. `myrun pull alpine:3.20` — fetch an anonymous bearer token from `auth.docker.io`, fetch the manifest (negotiating Docker v2 + OCI media types; resolves manifest lists to `linux/<GOARCH>`), stream each layer blob into `data/blobs/sha256/<hex>`, verify sha256, and untar each layer into `data/images/library/alpine/3.20/rootfs/<hex>/`. Whiteout files (`.wh.*`) are honored.
2. `myrun run alpine:3.20 /bin/sh` — if the first positional isn't an existing directory, it is parsed as an image reference. The runtime stacks the image's layer dirs as OverlayFS `lowerdir` (topmost first), creates a fresh `upperdir` + `workdir` under `data/containers/<id>/`, mounts the `merged` dir, and passes that merged path to the child which then does the usual chroot / mount /proc / exec from M1 + cgroups from M2. On exit the overlay is unmounted and the scratch dir is removed.

**CLI:**

```sh
# Pull once (works on macOS too — it's just HTTPS + tar)
./myrun pull alpine:3.20

# Run (Linux only)
sudo ./myrun run alpine:3.20 /bin/sh
sudo ./myrun run --memory=64M --cpu=0.5 --pids=100 alpine:3.20 /bin/sh

# M1 behaviour still works: pass a directory instead of an image ref
sudo ./myrun run ./rootfs /bin/sh
```

Override the store root with `MYRUN_STORE=/var/lib/myrun`. Default is `./data/` relative to the current working directory.

**macOS:** `go build ./...` compiles the darwin stub for overlay; `myrun pull` works fine (useful for pre-fetching images from the host before SSHing into a Linux VM); `myrun run` still errors out with the existing "requires Linux" message.

## Milestones
- **M1 (done):** PID/MNT/UTS/IPC/NET namespaces + chroot + `mount /proc` + basic `run`
- **M2 (done):** cgroups v2 resource limits (cpu/mem/pids)
- **M3 (done):** OverlayFS + OCI image pull from Docker Hub
- **M4:** CNI bridge networking + port forwarding
- **M5:** Seccomp profiles + rootless + full OCI spec compliance · USER namespace · pivot_root instead of chroot

## Key References
- OCI Runtime Spec
- `runc` source
- "Containers from Scratch" (Liz Rice talk)
