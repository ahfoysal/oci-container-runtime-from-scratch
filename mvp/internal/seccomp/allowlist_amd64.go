//go:build linux && amd64

// Syscall numbers for x86_64, sourced from
// arch/x86/entry/syscalls/syscall_64.tbl. We hand-pick the set Docker's
// default profile allows (see moby/profiles/seccomp/default.json) minus
// a handful we never want in a myrun container: mount(), ptrace(),
// kexec_load(), reboot(), init_module, finit_module, delete_module,
// iopl(), ioperm(), swapon/swapoff, settimeofday, clock_settime, etc.
//
// This is intentionally verbose — every line is a syscall we consciously
// allow. Adding a new one means reviewing it. Deny-by-default is the
// whole point of seccomp.
package seccomp

// DefaultAllowList returns the x86_64 allow list as raw syscall numbers.
// Callers should treat the slice as read-only.
func DefaultAllowList() []uint32 {
	return []uint32{
		0,   // read
		1,   // write
		2,   // open
		3,   // close
		4,   // stat
		5,   // fstat
		6,   // lstat
		7,   // poll
		8,   // lseek
		9,   // mmap
		10,  // mprotect
		11,  // munmap
		12,  // brk
		13,  // rt_sigaction
		14,  // rt_sigprocmask
		15,  // rt_sigreturn
		16,  // ioctl
		17,  // pread64
		18,  // pwrite64
		19,  // readv
		20,  // writev
		21,  // access
		22,  // pipe
		23,  // select
		24,  // sched_yield
		25,  // mremap
		26,  // msync
		27,  // mincore
		28,  // madvise
		32,  // dup
		33,  // dup2
		34,  // pause
		35,  // nanosleep
		36,  // getitimer
		37,  // alarm
		38,  // setitimer
		39,  // getpid
		40,  // sendfile
		41,  // socket
		42,  // connect
		43,  // accept
		44,  // sendto
		45,  // recvfrom
		46,  // sendmsg
		47,  // recvmsg
		48,  // shutdown
		49,  // bind
		50,  // listen
		51,  // getsockname
		52,  // getpeername
		53,  // socketpair
		54,  // setsockopt
		55,  // getsockopt
		56,  // clone   — needed for threads; flags are filtered by caps in rootless
		57,  // fork
		58,  // vfork
		59,  // execve
		60,  // exit
		61,  // wait4
		62,  // kill
		63,  // uname
		72,  // fcntl
		73,  // flock
		74,  // fsync
		75,  // fdatasync
		76,  // truncate
		77,  // ftruncate
		78,  // getdents
		79,  // getcwd
		80,  // chdir
		81,  // fchdir
		82,  // rename
		83,  // mkdir
		84,  // rmdir
		85,  // creat
		86,  // link
		87,  // unlink
		88,  // symlink
		89,  // readlink
		90,  // chmod
		91,  // fchmod
		92,  // chown
		93,  // fchown
		94,  // lchown
		95,  // umask
		96,  // gettimeofday
		97,  // getrlimit
		99,  // sysinfo
		100, // times
		102, // getuid
		104, // getgid
		105, // setuid
		106, // setgid
		107, // geteuid
		108, // getegid
		109, // setpgid
		110, // getppid
		111, // getpgrp
		112, // setsid
		113, // setreuid
		114, // setregid
		115, // getgroups
		116, // setgroups
		117, // setresuid
		118, // getresuid
		119, // setresgid
		120, // getresgid
		121, // getpgid
		124, // getsid
		125, // capget
		126, // capset
		127, // rt_sigpending
		128, // rt_sigtimedwait
		129, // rt_sigqueueinfo
		130, // rt_sigsuspend
		131, // sigaltstack
		132, // utime
		137, // statfs
		138, // fstatfs
		186, // gettid
		201, // time
		202, // futex
		203, // sched_setaffinity
		204, // sched_getaffinity
		217, // getdents64
		218, // set_tid_address
		228, // clock_gettime
		229, // clock_getres
		230, // clock_nanosleep
		231, // exit_group
		232, // epoll_wait
		233, // epoll_ctl
		257, // openat
		262, // newfstatat
		263, // unlinkat
		264, // renameat
		267, // readlinkat
		268, // fchmodat
		269, // faccessat
		270, // pselect6
		271, // ppoll
		272, // unshare   — allowed; rootless mode already uses it
		273, // set_robust_list
		274, // get_robust_list
		281, // epoll_pwait
		284, // eventfd
		288, // accept4
		290, // eventfd2
		291, // epoll_create1
		292, // dup3
		293, // pipe2
		302, // prlimit64
		309, // getcpu
		318, // getrandom
		332, // statx
		435, // clone3
		439, // faccessat2
	}
}
