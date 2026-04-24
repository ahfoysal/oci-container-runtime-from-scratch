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

## M4 Status

**Shipped:** Linux bridge networking, per-container veth pair, outbound NAT, and `--publish` port forwarding via iptables DNAT — no CNI plugins, stdlib-only Go, shells out to `ip` and `iptables`.

```
mvp/internal/network/
├── bridge_linux.go     # myrun0 bridge, veth pair, netns IP/route, iptables DNAT + MASQUERADE
└── network_darwin.go   # stub so `go build ./...` stays green on macOS
```

**Flow** (Linux):

1. **Bridge:** first container lazily creates `myrun0` (`ip link add ... type bridge`), assigns it `10.44.0.1/24`, brings it up, enables `ip_forward`, installs a `MASQUERADE` rule for `10.44.0.0/24 ! -o myrun0` and opens `FORWARD` both directions on the bridge. Subsequent runs reuse the bridge.
2. **Per-container:** parent creates a veth pair `vm<id>` / `vc<id>`, attaches host side to `myrun0`, and moves the peer into the child's netns (`ip link set <peer> netns <child-pid>`). Then, still from the parent but targeting the child's netns via `ip -n <pid>`: rename peer to `eth0`, bring `lo` up, assign `10.44.0.X/24` (deterministic `sha1(container-id) → .2-.254`), bring `eth0` up, add `default via 10.44.0.1`. A minimal `/etc/resolv.conf` (8.8.8.8, 1.1.1.1) is written into the rootfs so DNS works.
3. **Port forwarding (`--publish host:container[/proto]`, repeatable):** parent installs DNAT rules in `nat/PREROUTING` and `nat/OUTPUT` to rewrite the destination to `<container-ip>:<container-port>`, plus a matching `FORWARD ACCEPT`. Each rule carries `-m comment --comment myrun:<id>` so teardown can delete exactly its own rules.
4. **Teardown:** deletes the host-side veth (which also evicts the peer in the now-gone netns) and removes all iptables rules tagged with the container id. The `myrun0` bridge is intentionally left in place.

The child doesn't need any new code — by the time the parent closes the sync pipe, `eth0` is already up inside the netns, and the child chroots/execs as before.

**CLI:**

```sh
sudo ./myrun run alpine:3.20 /bin/sh
sudo ./myrun run --publish 8080:80 alpine:3.20 /bin/sh
sudo ./myrun run --publish 8080:80 --publish 8443:443/tcp alpine:3.20 /bin/sh
sudo ./myrun run --memory=64M --cpu=0.5 --publish 8080:80 alpine:3.20 /bin/sh
```

### Linux test recipe

Requires `iproute2` (`ip`) and `iptables` on the host (both default on Ubuntu 22.04+). Inside your Linux VM:

1. Build and pull an image that ships `nc` or `httpd`:
   ```sh
   cd mvp
   go build -o myrun ./cmd/myrun
   sudo ./myrun pull alpine:3.20
   ```

2. **Outbound + DNS:** run a shell and hit the bridge, gateway, and the public internet:
   ```sh
   sudo ./myrun run alpine:3.20 /bin/sh
   # inside container:
   ip addr show eth0          # 10.44.0.X/24
   ip route                   # default via 10.44.0.1
   ping -c 2 10.44.0.1        # the myrun0 bridge
   ping -c 2 8.8.8.8          # outbound via MASQUERADE
   nslookup google.com        # resolv.conf → 8.8.8.8 works
   ```

3. **Port forwarding:** publish 8080 → 80 and curl it from the host:
   ```sh
   # terminal 1 — start a tiny HTTP listener inside the container:
   sudo ./myrun run --publish 8080:80 alpine:3.20 \
     /bin/sh -c 'apk add --no-cache busybox-extras >/dev/null; httpd -f -p 80 -h /'

   # terminal 2 — hit it from the host:
   curl -v http://127.0.0.1:8080/
   curl -v http://<vm-ip>:8080/     # works from outside the VM too
   ```

4. **Verify the rules while the container runs:**
   ```sh
   ip link show myrun0                 # bridge, state UP
   ip -br link | grep '^vm'            # host-side veth, master myrun0
   sudo iptables -t nat -S PREROUTING | grep myrun
   sudo iptables -t nat -S POSTROUTING | grep 10.44.0
   ```

5. **After the container exits:** the host veth disappears and the DNAT rules are gone. The `myrun0` bridge is left in place for the next run:
   ```sh
   ip link show myrun0                 # still present
   ip -br link | grep '^vm'            # nothing
   sudo iptables -t nat -S | grep myrun    # nothing
   ```

**macOS:** `go build ./...` compiles the darwin stub; `myrun run` still errors out with the existing "requires Linux" message.

## M5 Status

**Shipped:** default seccomp BPF profile, rootless (user-namespace) mode, and partial OCI runtime spec compliance — `myrun run --spec config.json` now works for the subset of the spec the runtime can honour.

```
mvp/internal/seccomp/
├── seccomp_linux.go        # hand-rolled cBPF filter, PR_SET_NO_NEW_PRIVS + SECCOMP_SET_MODE_FILTER + TSYNC
├── allowlist_amd64.go      # x86_64 syscall numbers (mirrors Docker default profile)
├── allowlist_arm64.go      # aarch64 syscall numbers
└── seccomp_darwin.go       # stub

mvp/internal/userns/
├── userns_linux.go         # CLONE_NEWUSER, uid/gid maps, setgroups=deny, sysctl preflight
└── userns_darwin.go        # stub

mvp/internal/ocispec/
├── spec.go                 # parser for ociVersion/process/root/mounts/linux.*
├── spec_test.go
└── testdata/
    ├── config.json            # sample spec (memory 64M, cpu 0.5, pids 100, default seccomp)
    └── config_rootless.json   # sample spec with linux.namespaces[user] + uid/gid maps
```

### What M5 enforces

- **Seccomp BPF (default on, `--no-seccomp` to disable).** The child installs the filter on every thread via `SECCOMP_FILTER_FLAG_TSYNC` right before `execve`. The program is a single ld-arch-check + unrolled JEQ cascade that kills the process on any syscall outside the allow list. Dangerous syscalls explicitly excluded on x86_64: `ptrace`, `mount`, `umount2`, `kexec_load`, `kexec_file_load`, `reboot`, `init_module`, `finit_module`, `delete_module`, `iopl`, `ioperm`, `swapon`, `swapoff`, `settimeofday`, `clock_settime`, `create_module`, `query_module`, `get_kernel_syms`, `nfsservctl`, `lookup_dcookie`, `perf_event_open`, `bpf`, `userfaultfd`, `mbind`, `move_pages`, `set_mempolicy`, `get_mempolicy`, `acct`, `add_key`, `request_key`, `keyctl`.
- **Rootless mode (`--rootless`, auto-enabled when invoked as non-root).** Adds `CLONE_NEWUSER`, sets uid/gid maps to `0 → <real-uid> 1`, disables `setgroups` so the mapping is legal without `CAP_SETGID` in the parent, and the runtime skips cgroups + bridge networking because those need real root. Container comes up with only `lo` and inherits the host's network via the user ns — good enough to prove isolation works; slirp4netns integration is a future milestone.
- **OCI runtime spec (`--spec config.json`).** The spec is folded into `runtime.Config` before Run proceeds; CLI flags that were explicitly set win over spec values. Supported fields: `ociVersion`, `process.{args,env,cwd,user,noNewPrivileges,capabilities}` (args/cwd honoured; rest surfaced), `root.{path,readonly}`, `hostname`, `mounts` (parsed, partial consumption — /proc still always bind-mounted), `linux.namespaces` (type=user triggers rootless), `linux.{uidMappings,gidMappings}`, `linux.resources.{memory.limit, cpu.{quota,period}, pids.limit}`, `linux.seccomp` (any non-nil value turns on our default profile — per-syscall rule translation is intentionally omitted).

### CLI

```sh
# Seccomp (default on)
sudo ./myrun run alpine:3.20 /bin/sh

# Disable seccomp for debugging
sudo ./myrun run --no-seccomp alpine:3.20 /bin/sh

# Rootless (no sudo)
./myrun run --rootless alpine:3.20 /bin/sh

# OCI spec drive
./myrun run --spec mvp/internal/ocispec/testdata/config.json
```

### Linux verification recipes

1. **Seccomp hard-deny works** — inside the container, calling a blocked syscall should kill the process:

   ```sh
   sudo ./myrun run alpine:3.20 /bin/sh
   # inside container — mount() is not in the allow list:
   mount -t tmpfs tmpfs /mnt 2>/dev/null ; echo exit=$?   # shell dies with "Bad system call"
   ```

   Confirm on the host:
   ```sh
   dmesg | tail   # "audit: type=1326 ... comm=\"sh\" ... syscall=165 ... code=0x80000000"
   ```

   With `--no-seccomp` the same `mount` call returns `EPERM` or `EACCES` instead of killing the shell — use that to prove the filter is what's enforcing, not the caps.

2. **Rootless works without sudo** (requires `sysctl kernel.unprivileged_userns_clone=1`):

   ```sh
   ./myrun run --rootless alpine:3.20 /bin/sh
   # inside:
   id           # uid=0(root) gid=0(root)
   cat /proc/self/uid_map   # "0 <your-host-uid> 1"
   ip link      # only lo — by design, rootless has no bridge
   ```

   On the host, the container process actually runs as your user:
   ```sh
   ps -eo pid,user,comm | grep myrun    # user is YOU, not root
   ```

3. **OCI spec** — `--spec` drives rootfs + command + limits from config.json:

   ```sh
   # Prepare a rootfs dir next to the config, or edit root.path to point at one.
   cp -r /path/to/alpine-rootfs mvp/internal/ocispec/testdata/rootfs
   sudo ./myrun run --spec mvp/internal/ocispec/testdata/config.json
   # limits applied: verify in another shell
   cat /sys/fs/cgroup/myrun-<pid>/memory.max   # 67108864
   cat /sys/fs/cgroup/myrun-<pid>/cpu.max      # 50000 100000
   cat /sys/fs/cgroup/myrun-<pid>/pids.max     # 100
   ```

**macOS:** `go build ./...` compiles the darwin stubs for seccomp + userns + ocispec. Parsing a config.json works on macOS too — execution still errors out with the existing "requires Linux" message.

## M6 Status

**Shipped:** `pivot_root(2)` replacing `chroot(2)` in the container entrypoint, slirp4netns-backed outbound networking for rootless mode, and CRIU-driven checkpoint/restore subcommands.

```
mvp/internal/pivot/
├── pivot_linux.go     # rprivate / -> bind newroot -> mkdir .pivot_old -> pivot_root -> chroot(".") -> umount old -> mount /proc
└── pivot_darwin.go    # stub

mvp/internal/slirp/
├── slirp_linux.go     # spawns slirp4netns, waits on --ready-fd, teardown via SIGTERM
└── slirp_darwin.go    # stub

mvp/internal/criu/
├── criu_linux.go      # shells `criu dump` / `criu restore` with shell-job + tcp-established + link-remap
└── criu_darwin.go     # stub
```

### What M6 changes

- **pivot_root replaces chroot.** The child process no longer calls `syscall.Chroot` directly. `pivot.Do` performs the full runc-style sequence: make `/` rprivate+recursive, bind-mount the newroot onto itself (so it becomes a mount point distinct from the current root), `mkdir .pivot_old`, `chdir(newroot)`, `pivot_root(".", ".pivot_old")`, `chroot(".")` as belt-and-braces, `umount("/.pivot_old", MNT_DETACH)`, `rmdir /.pivot_old`, then `mount proc /proc`. After this the host filesystem is structurally unreachable — the `double-chroot ../..` and `fchdir` escapes that work against plain chroot cannot produce a valid path to the old root because the mount namespace no longer contains one.
- **slirp4netns powers rootless networking.** M5 left rootless containers with loopback only. M6 spawns `slirp4netns` as a host-side subprocess pointed at the child's `/proc/<pid>/ns/net`, with `--configure --mtu=65520 --disable-host-loopback --ready-fd=3`. The runtime blocks on the ready pipe until libslirp has a `tap0` device configured inside the netns (10.0.2.100, gw 10.0.2.2, DNS stub 10.0.2.3), then closes the sync pipe so the child execs into a fully-networked environment. Teardown SIGTERMs slirp4netns with a 2s SIGKILL fallback. If the binary isn't installed we fall back to the M5 loopback-only behaviour with a pointer to `apt/dnf/pacman install slirp4netns` — consistent with `podman rootless` / `rootlesskit`.
- **CRIU checkpoint/restore.** Two new subcommands. `myrun checkpoint <pid> <images-dir>` shells `criu dump --tree <pid> --images-dir <dir> --shell-job --tcp-established --file-locks --link-remap --manage-cgroups=soft`; `myrun restore <images-dir>` mirrors the flags for `criu restore`. `dump.log` / `restore.log` land in the images dir next to the `*.img` files so failures (missing kernel feature, unsupported fd, seccomp blocking ptrace) are diagnosable. We shell out rather than link libcriu to keep the dependency surface stdlib-only Go + a runtime prerequisite — the same pattern runc, podman, and containerd used for years before CRIU's RPC mode matured.

### Host prerequisites

- **pivot_root:** none beyond what M1 already needed.
- **slirp4netns:** `apt install slirp4netns` / `dnf install slirp4netns` / `pacman -S slirp4netns`. Standard on Ubuntu 22.04+ as a podman dependency.
- **CRIU:** `apt install criu` / `dnf install criu`. Kernel must have `CONFIG_CHECKPOINT_RESTORE=y` (every mainstream distro). The caller needs `CAP_SYS_ADMIN` or `CAP_CHECKPOINT_RESTORE` — run as root for the toy.

### CLI

```sh
# Rootless with real outbound connectivity (slirp4netns on host required)
./myrun run --rootless alpine:3.20 /bin/sh
# inside container: wget https://example.com now works via 10.0.2.100/tap0

# Checkpoint a running container. Find its PID with `ps -ef | grep myrun child`.
sudo ./myrun checkpoint 12345 /tmp/ckpt

# Resume the checkpointed process tree. Blocks until it exits.
sudo ./myrun restore /tmp/ckpt
```

### Linux verification recipes

1. **pivot_root escape-resistance** — in M1 a process with a lingering fd above the chroot could climb out via `fchdir`. After M6, the old root is gone from the mount namespace entirely:

   ```sh
   sudo ./myrun run alpine:3.20 /bin/sh
   # inside container:
   cat /proc/self/mountinfo | head    # only rootfs + /proc, no /.pivot_old
   ls /.pivot_old                     # "No such file or directory"
   # host's /etc is unreachable no matter how many ../.. you type.
   ```

2. **Rootless + slirp4netns outbound works** (needs `kernel.unprivileged_userns_clone=1` + slirp4netns on PATH):

   ```sh
   ./myrun run --rootless alpine:3.20 /bin/sh
   # inside container:
   ip addr show tap0                  # 10.0.2.100/24
   ip route                           # default via 10.0.2.2
   cat /etc/resolv.conf               # nameserver 10.0.2.3
   wget -qO- https://example.com      # traverses libslirp -> host
   ```

3. **CRIU round-trip** (requires criu >= 3.15, run as root):

   ```sh
   # terminal 1 — start a long-running counter inside a container:
   sudo ./myrun run alpine:3.20 /bin/sh -c 'i=0; while true; do echo $i; i=$((i+1)); sleep 1; done'

   # terminal 2 — snapshot it by the container PID-1:
   PID=$(pgrep -f 'myrun child' | head -n1)
   sudo ./myrun checkpoint "$PID" /tmp/ckpt
   ls /tmp/ckpt                        # pages-*.img, core-*.img, dump.log ...

   # terminal 3 — resume; the counter continues from where it stopped:
   sudo ./myrun restore /tmp/ckpt
   ```

**macOS:** `go build ./...` compiles the darwin stubs for pivot/slirp/criu. The `checkpoint` and `restore` subcommands still parse their args on macOS and then exit with a clear "requires Linux" / "criu not found" message before any syscall happens.

## Milestones
- **M1 (done):** PID/MNT/UTS/IPC/NET namespaces + chroot + `mount /proc` + basic `run`
- **M2 (done):** cgroups v2 resource limits (cpu/mem/pids)
- **M3 (done):** OverlayFS + OCI image pull from Docker Hub
- **M4 (done):** Bridge networking (`myrun0`) + veth pairs + iptables DNAT port forwarding
- **M5 (done):** Seccomp BPF default profile + rootless user namespaces + OCI runtime spec (`--spec config.json`)
- **M6 (done):** `pivot_root` in place of `chroot`, slirp4netns for rootless outbound networking, CRIU checkpoint/restore
- **M7 (future):** AppArmor profiles, containerd-shim-compatible lifecycle, CRI plug, registry auth beyond anonymous Docker Hub

## Key References
- OCI Runtime Spec
- `runc` source
- "Containers from Scratch" (Liz Rice talk)
