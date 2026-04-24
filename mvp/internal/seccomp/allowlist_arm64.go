//go:build linux && arm64

// Syscall numbers for aarch64 (from include/uapi/asm-generic/unistd.h).
// Notable differences vs x86_64: no "open"/"stat"/"mkdir"/"rename" —
// aarch64 only ships the *at variants, and the numbering starts at 0
// with io_setup rather than read. Be careful copying from x86_64 lists —
// we hand-map each one rather than blindly reusing numbers.
package seccomp

// DefaultAllowList returns the aarch64 allow list as raw syscall numbers.
func DefaultAllowList() []uint32 {
	return []uint32{
		17,  // getcwd
		19,  // eventfd2
		20,  // epoll_create1
		21,  // epoll_ctl
		22,  // epoll_pwait
		23,  // dup
		24,  // dup3
		25,  // fcntl
		26,  // inotify_init1
		29,  // ioctl
		32,  // flock
		33,  // mknodat
		34,  // mkdirat
		35,  // unlinkat
		36,  // symlinkat
		37,  // linkat
		38,  // renameat
		40,  // mount   — we keep it; denied by lack of caps in rootless
		43,  // statfs
		44,  // fstatfs
		45,  // truncate
		46,  // ftruncate
		47,  // fallocate
		48,  // faccessat
		49,  // chdir
		50,  // fchdir
		51,  // chroot
		52,  // fchmod
		53,  // fchmodat
		54,  // fchownat
		55,  // fchown
		56,  // openat
		57,  // close
		59,  // pipe2
		61,  // getdents64
		62,  // lseek
		63,  // read
		64,  // write
		65,  // readv
		66,  // writev
		67,  // pread64
		68,  // pwrite64
		72,  // pselect6
		73,  // ppoll
		78,  // readlinkat
		79,  // fstatat
		80,  // fstat
		82,  // fsync
		83,  // fdatasync
		88,  // utimensat
		90,  // capget
		91,  // capset
		93,  // exit
		94,  // exit_group
		95,  // waitid
		98,  // futex
		99,  // set_robust_list
		100, // get_robust_list
		101, // nanosleep
		113, // clock_gettime
		114, // clock_getres
		115, // clock_nanosleep
		118, // sched_setparam
		119, // sched_setscheduler
		120, // sched_getscheduler
		122, // sched_setaffinity
		123, // sched_getaffinity
		124, // sched_yield
		129, // kill
		130, // tkill
		131, // tgkill
		132, // sigaltstack
		134, // rt_sigaction
		135, // rt_sigprocmask
		136, // rt_sigpending
		137, // rt_sigtimedwait
		138, // rt_sigqueueinfo
		139, // rt_sigreturn
		153, // times
		155, // getpgid
		156, // getsid
		157, // setsid
		158, // getgroups
		159, // setgroups
		160, // uname
		165, // getrusage
		166, // umask
		172, // getpid
		173, // getppid
		174, // getuid
		175, // geteuid
		176, // getgid
		177, // getegid
		178, // gettid
		179, // sysinfo
		198, // socket
		199, // socketpair
		200, // bind
		201, // listen
		202, // accept
		203, // connect
		204, // getsockname
		205, // getpeername
		206, // sendto
		207, // recvfrom
		208, // setsockopt
		209, // getsockopt
		210, // shutdown
		211, // sendmsg
		212, // recvmsg
		214, // brk
		215, // munmap
		216, // mremap
		220, // clone
		221, // execve
		222, // mmap
		226, // mprotect
		233, // madvise
		242, // accept4
		261, // prlimit64
		278, // getrandom
		281, // execveat
		291, // statx
		435, // clone3
		439, // faccessat2
	}
}
