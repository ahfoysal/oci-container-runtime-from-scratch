//go:build linux

// Package seccomp installs a hand-rolled BPF seccomp filter on the calling
// thread. The filter mirrors the spirit of Docker/runc's default profile:
// allow the ~350 syscalls a normal Linux program uses, and hard-deny a
// curated list of dangerous ones (ptrace, kexec_load, mount, reboot, …).
//
// We deliberately avoid libseccomp. Cgo would complicate static builds and
// cross-compilation, and the kernel's cBPF + SECCOMP_SET_MODE_FILTER ABI is
// small enough to emit by hand. The filter we build is the classic three
// step sequence:
//
//  1. Load arch into the accumulator, compare against AUDIT_ARCH_X86_64 /
//     AUDIT_ARCH_AARCH64. Mismatch → KILL (defeats architecture confusion
//     attacks where a 32-bit syscall slips past a 64-bit allow list).
//  2. Load nr (the syscall number). For every allowed number, emit a JEQ
//     that jumps to a terminal ALLOW. The big allow list is unrolled into
//     one equality per syscall — it's O(n) but n ≤ 512 and the kernel JITs
//     the program on modern kernels so the cost is negligible.
//  3. Terminal: KILL the thread. This is SECCOMP_RET_KILL_THREAD on older
//     kernels / SECCOMP_RET_KILL_PROCESS on 4.14+. We use _PROCESS because
//     killing a single thread in a multi-threaded container leaves a
//     zombie-ish state that is hard to debug.
//
// Before SECCOMP_SET_MODE_FILTER the caller must have set PR_SET_NO_NEW_PRIVS
// (or have CAP_SYS_ADMIN). Apply() does that.
package seccomp

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sockFilter is the cBPF instruction layout, matching `struct sock_filter`
// in <linux/filter.h>. We keep it byte-identical so we can hand it to
// prctl/seccomp as a pointer without any conversion.
type sockFilter struct {
	Code uint16
	JT   uint8
	JF   uint8
	K    uint32
}

// sockFprog mirrors `struct sock_fprog` — a (len, *filter) pair.
type sockFprog struct {
	Len    uint16
	_      [6]byte // pad to 8-byte alignment of the pointer on 64-bit
	Filter *sockFilter
}

// BPF opcodes we emit. Matching the names in <linux/bpf_common.h> keeps the
// filter easy to cross-reference with kernel source / iovisor docs.
const (
	bpfLD  = 0x00
	bpfJMP = 0x05
	bpfRET = 0x06

	bpfW   = 0x00
	bpfABS = 0x20
	bpfK   = 0x00
	bpfJEQ = 0x10
	bpfJGE = 0x30
)

// seccomp_data offsets — we load the `nr` (syscall number) and `arch`
// fields from the 64-byte struct the kernel passes to every filter.
const (
	seccompDataNrOffset   = 0
	seccompDataArchOffset = 4
)

// SECCOMP_RET_* action codes. The low 16 bits are the KILL/ALLOW/… action,
// the high 16 bits are data.
const (
	retAllow        uint32 = 0x7fff0000
	retKillProcess  uint32 = 0x80000000
	retKillThread   uint32 = 0x00000000
	retErrnoEPERM   uint32 = 0x00050000 | uint32(syscall.EPERM)
)

// archAuditCurrent returns the AUDIT_ARCH_* constant for this binary. We
// only support the two architectures we actually test on: x86_64 and
// arm64. Trying to run myrun on, say, riscv64 will fall through to the
// "unsupported" error rather than silently installing a wrong-arch filter.
func archAuditCurrent() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xc000003e, nil // AUDIT_ARCH_X86_64
	case "arm64":
		return 0xc00000b7, nil // AUDIT_ARCH_AARCH64
	default:
		return 0, fmt.Errorf("seccomp: unsupported GOARCH %q", runtime.GOARCH)
	}
}

// buildFilter emits the BPF program. The layout we emit, annotated:
//
//	ld  [arch]                ; A = seccomp_data.arch
//	jeq #AUDIT_ARCH_CURRENT, +1, 0   ; skip to syscall check on match
//	ret KILL                  ; else: wrong arch → kill
//	ld  [nr]                  ; A = seccomp_data.nr
//	jeq #sys_read,   +N, 0    ; N decrements for each remaining allowed nr
//	jeq #sys_write,  +N-1, 0
//	...
//	ret KILL                  ; no match → kill
//	ret ALLOW                 ; terminal allow target
//
// Note we put the single ALLOW at the end and have every JEQ jump to it.
// That way each allowed syscall only costs one instruction in the program
// (plus the shared tail), keeping the filter compact.
func buildFilter(auditArch uint32, allowed []uint32) []sockFilter {
	// Count syscalls to compute jump offsets. We append the epilogue
	// (KILL + ALLOW) after the comparisons; each JEQ must jump to the
	// ALLOW which sits at `lastIdx`.
	//
	// Indices in the final program:
	//   0: ld arch
	//   1: jeq AUDIT_ARCH, +1, 0   → if match, skip the kill at 2
	//   2: ret KILL                 (wrong arch)
	//   3: ld nr
	//   4..4+len(allowed)-1: jeq sys_X, +K, 0 where K points to ALLOW
	//   4+len(allowed):     ret KILL (no-match tail)
	//   5+len(allowed):     ret ALLOW

	n := uint32(len(allowed))
	prog := make([]sockFilter, 0, 6+n)

	// 0: load arch
	prog = append(prog, sockFilter{Code: bpfLD | bpfW | bpfABS, K: seccompDataArchOffset})
	// 1: if arch == expected, jump over the kill
	prog = append(prog, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, JT: 1, JF: 0, K: auditArch})
	// 2: bad arch → kill
	prog = append(prog, sockFilter{Code: bpfRET | bpfK, K: retKillProcess})
	// 3: load syscall nr
	prog = append(prog, sockFilter{Code: bpfLD | bpfW | bpfABS, K: seccompDataNrOffset})

	// 4..: one JEQ per allowed syscall, each jumping to the ALLOW at the
	// end. The offset is "number of instructions to skip", so for the i-th
	// JEQ (0-indexed) we need to skip (n-1-i) remaining JEQs plus the
	// final KILL instruction → (n - i).
	for i, nr := range allowed {
		skip := n - uint32(i) // to reach the ALLOW sitting after the tail KILL
		prog = append(prog, sockFilter{
			Code: bpfJMP | bpfJEQ | bpfK,
			JT:   uint8(skip),
			JF:   0,
			K:    nr,
		})
	}

	// Tail: no-match → kill
	prog = append(prog, sockFilter{Code: bpfRET | bpfK, K: retKillProcess})
	// Final target for matched JEQs
	prog = append(prog, sockFilter{Code: bpfRET | bpfK, K: retAllow})

	return prog
}

// Apply installs the default profile on the current thread. It MUST be
// called after all runtime setup (chroot, exec arg prep) because the
// filter denies some syscalls the Go runtime needs at startup. In myrun
// we call it from Child() right before syscall.Exec.
//
// We lock the OS thread for the duration: PR_SET_NO_NEW_PRIVS is per-task
// and filters are per-thread (they propagate to new threads after install,
// but only the installing thread is guaranteed to have the filter until
// the next clone).
func Apply() error {
	runtime.LockOSThread()
	// Intentionally never Unlock — this thread is about to exec anyway.

	// PR_SET_NO_NEW_PRIVS = 38. Without this the kernel refuses
	// SECCOMP_SET_MODE_FILTER unless the caller has CAP_SYS_ADMIN, and we
	// want the filter to work in rootless mode too.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}

	auditArch, err := archAuditCurrent()
	if err != nil {
		return err
	}
	allowed := DefaultAllowList()
	prog := buildFilter(auditArch, allowed)

	fprog := sockFprog{
		Len:    uint16(len(prog)),
		Filter: &prog[0],
	}

	// seccomp(SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_TSYNC, &fprog).
	// TSYNC makes the kernel install the filter on every thread in this
	// thread group — relevant because the Go runtime can spawn threads
	// before we reach Apply().
	const (
		SECCOMP_SET_MODE_FILTER  = 1
		SECCOMP_FILTER_FLAG_TSYNC = 1
	)
	_, _, errno := syscall.Syscall(unix.SYS_SECCOMP, SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog)))
	if errno != 0 {
		return fmt.Errorf("seccomp(SET_MODE_FILTER): %w", errno)
	}
	return nil
}
