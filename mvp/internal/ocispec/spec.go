// Package ocispec parses a subset of the OCI Runtime Specification
// (runtime-spec v1.1.0) config.json and translates it into the
// primitives the myrun runtime already consumes: rootfs path, command,
// env, hostname, mounts, uid/gid mappings, resource limits, and the
// list of capabilities to keep.
//
// We intentionally implement only the fields we can honour: everything
// under `process`, `root`, `hostname`, `mounts`, `linux.namespaces`,
// `linux.resources`, `linux.uidMappings`/`gidMappings`, and a minimal
// `linux.seccomp` acknowledgment (defer to our default profile). Things
// like selinuxLabel, apparmorProfile, rdma, intelRdt, personality,
// hugepageLimits, or the full `process.capabilities` bounding/inherit
// sets we surface as warnings but don't error on — "best-effort runc
// compatibility" rather than "full spec conformance".
//
// The file is pure stdlib and tagged for all platforms: parsing runs
// the same on darwin and linux, only the *consumer* of Spec (the
// runtime package) is platform-gated.
package ocispec

import (
	"encoding/json"
	"fmt"
	"os"
)

// Spec mirrors the shape of the OCI runtime spec we care about. We
// keep the field names aligned with the spec so config.json parses
// directly without custom UnmarshalJSON.
type Spec struct {
	OCIVersion string      `json:"ociVersion"`
	Process    *Process    `json:"process,omitempty"`
	Root       *Root       `json:"root,omitempty"`
	Hostname   string      `json:"hostname,omitempty"`
	Mounts     []Mount     `json:"mounts,omitempty"`
	Linux      *LinuxBlock `json:"linux,omitempty"`
}

// Process captures the executable + env + cwd. We skip rlimits and
// scheduler fields for now; adding them is a couple of prlimit64 calls
// in Child(), out of scope for M5.
type Process struct {
	Terminal     bool          `json:"terminal,omitempty"`
	User         User          `json:"user"`
	Args         []string      `json:"args"`
	Env          []string      `json:"env,omitempty"`
	Cwd          string        `json:"cwd"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	NoNewPrivs   bool          `json:"noNewPrivileges,omitempty"`
}

// User sets the uid/gid the container's PID 1 runs as. We only honour
// UID / GID. `username` lookup from /etc/passwd inside the rootfs is a
// future refinement.
type User struct {
	UID            uint32   `json:"uid"`
	GID            uint32   `json:"gid"`
	AdditionalGids []uint32 `json:"additionalGids,omitempty"`
}

// Capabilities follows the five-set model from capabilities(7). We
// take the intersection of Bounding and Effective as the set to keep;
// everything outside it is dropped before exec.
type Capabilities struct {
	Bounding    []string `json:"bounding,omitempty"`
	Effective   []string `json:"effective,omitempty"`
	Inheritable []string `json:"inheritable,omitempty"`
	Permitted   []string `json:"permitted,omitempty"`
	Ambient     []string `json:"ambient,omitempty"`
}

// Root is where the container's filesystem lives. `path` is resolved
// relative to the directory that holds config.json.
type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

// Mount is a single bind/tmpfs/proc mount. We honour `destination`,
// `type`, `source`, and `options` (as a comma-joined string for the
// data arg to mount(2)).
type Mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// LinuxBlock holds the Linux-specific knobs. Everything inside is
// optional; a minimal config.json can omit it entirely and just rely
// on the top-level process/root.
type LinuxBlock struct {
	Namespaces   []Namespace   `json:"namespaces,omitempty"`
	UIDMappings  []IDMapping   `json:"uidMappings,omitempty"`
	GIDMappings  []IDMapping   `json:"gidMappings,omitempty"`
	Resources    *Resources    `json:"resources,omitempty"`
	Seccomp      *SeccompBlock `json:"seccomp,omitempty"`
	MaskedPaths  []string      `json:"maskedPaths,omitempty"`
	ReadonlyPaths []string     `json:"readonlyPaths,omitempty"`
}

// Namespace: `type` matches the CLONE_NEW* flags we set. A `path`
// field (for joining an existing ns) is recognised but not yet
// consumed — we warn and proceed with a fresh ns.
type Namespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// IDMapping mirrors {containerID, hostID, size}.
type IDMapping struct {
	ContainerID uint32 `json:"containerID"`
	HostID      uint32 `json:"hostID"`
	Size        uint32 `json:"size"`
}

// Resources is a trimmed cgroups v2 knob set matching what M2
// already supports: memory.limit, cpu shares, pids.max.
type Resources struct {
	Memory *MemoryResources `json:"memory,omitempty"`
	CPU    *CPUResources    `json:"cpu,omitempty"`
	Pids   *PidsResources   `json:"pids,omitempty"`
}

type MemoryResources struct {
	Limit int64 `json:"limit,omitempty"`
}

type CPUResources struct {
	// Quota / Period in microseconds — matches cgroups v2 cpu.max.
	// If Period is 0 we default to 100000 (100ms).
	Quota  int64  `json:"quota,omitempty"`
	Period uint64 `json:"period,omitempty"`
}

type PidsResources struct {
	Limit int64 `json:"limit,omitempty"`
}

// SeccompBlock is acknowledged but not interpreted field-by-field.
// We treat any non-nil value as "install the myrun default profile".
// Full syscall-action translation can be added later.
type SeccompBlock struct {
	DefaultAction string `json:"defaultAction,omitempty"`
	// Syscalls elided on purpose — see package doc.
}

// Load reads + parses config.json from the given path. The path may
// be either the JSON file itself or a directory containing it.
func Load(path string) (*Spec, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("ocispec: stat %q: %w", path, err)
	}
	if info.IsDir() {
		path = path + "/config.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ocispec: read %q: %w", path, err)
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("ocispec: parse %q: %w", path, err)
	}
	if s.OCIVersion == "" {
		return nil, fmt.Errorf("ocispec: %q missing ociVersion", path)
	}
	if s.Root == nil || s.Root.Path == "" {
		return nil, fmt.Errorf("ocispec: %q missing root.path", path)
	}
	if s.Process == nil || len(s.Process.Args) == 0 {
		return nil, fmt.Errorf("ocispec: %q missing process.args", path)
	}
	return &s, nil
}
