//go:build linux && riscv64

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

type procState struct {
	entering bool
	last     syscallFrame
}

type syscallFrame struct {
	num  uint64
	args [6]uint64
	pc   uint64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rstrace: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var outPath string
	var follow bool
	var attachPID int
	flag.StringVar(&outPath, "o", "", "write trace to file instead of stderr")
	flag.BoolVar(&follow, "f", true, "follow fork/vfork/clone children")
	flag.IntVar(&attachPID, "p", 0, "attach to an existing pid instead of starting a command")
	flag.Parse()

	var out io.Writer = os.Stderr
	var outFile *os.File
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer f.Close()
		outFile = f
		out = f
	}
	_ = outFile

	procs := map[int]*procState{}
	if attachPID > 0 {
		if flag.NArg() != 0 {
			return fmt.Errorf("-p cannot be combined with a command")
		}
		if err := syscall.PtraceAttach(attachPID); err != nil {
			return fmt.Errorf("attach pid %d: %w", attachPID, err)
		}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(attachPID, &ws, 0, nil); err != nil {
			return fmt.Errorf("wait attached pid %d: %w", attachPID, err)
		}
		procs[attachPID] = &procState{entering: true}
		if err := setPtraceOptions(attachPID, follow); err != nil {
			return fmt.Errorf("set ptrace options pid %d: %w", attachPID, err)
		}
		if err := syscall.PtraceSyscall(attachPID, 0); err != nil {
			return fmt.Errorf("resume pid %d: %w", attachPID, err)
		}
	} else {
		args := flag.Args()
		if len(args) == 0 {
			return fmt.Errorf("usage: rstrace [-f] [-o file] command [args...]")
		}
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		pid := cmd.Process.Pid
		procs[pid] = &procState{entering: true}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
			return fmt.Errorf("initial wait pid %d: %w", pid, err)
		}
		if err := setPtraceOptions(pid, follow); err != nil {
			return fmt.Errorf("set ptrace options pid %d: %w", pid, err)
		}
		if err := syscall.PtraceSyscall(pid, 0); err != nil {
			return fmt.Errorf("resume pid %d: %w", pid, err)
		}
	}

	for len(procs) > 0 {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if err == syscall.ECHILD {
				return nil
			}
			return err
		}
		st := procs[pid]
		if st == nil {
			st = &procState{entering: true}
			procs[pid] = st
		}

		switch {
		case ws.Exited():
			fmt.Fprintf(out, "%d +++ exited %d +++\n", pid, ws.ExitStatus())
			delete(procs, pid)
			continue
		case ws.Signaled():
			fmt.Fprintf(out, "%d +++ killed by %s +++\n", pid, ws.Signal())
			delete(procs, pid)
			continue
		case !ws.Stopped():
			continue
		}

		stop := int(ws.StopSignal())
		if stop == int(syscall.SIGTRAP)|0x80 {
			if err := traceSyscallStop(out, pid, st); err != nil {
				fmt.Fprintf(out, "%d !!! getregs: %v !!!\n", pid, err)
			}
			_ = syscall.PtraceSyscall(pid, 0)
			continue
		}

		if ws.StopSignal() == syscall.SIGTRAP {
			if follow {
				if child, ok := ptraceEventChild(pid, ws); ok {
					procs[child] = &procState{entering: true}
					_ = setPtraceOptions(child, follow)
					_ = syscall.PtraceSyscall(child, 0)
				}
			}
			_ = syscall.PtraceSyscall(pid, 0)
			continue
		}

		sig := int(ws.StopSignal())
		if ws.StopSignal() == syscall.SIGSTOP {
			sig = 0
		}
		_ = syscall.PtraceSyscall(pid, sig)
	}
	return nil
}

func setPtraceOptions(pid int, follow bool) error {
	opts := syscall.PTRACE_O_TRACESYSGOOD
	if follow {
		opts |= syscall.PTRACE_O_TRACEFORK |
			syscall.PTRACE_O_TRACEVFORK |
			syscall.PTRACE_O_TRACECLONE |
			syscall.PTRACE_O_TRACEEXEC
	}
	return syscall.PtraceSetOptions(pid, opts)
}

func ptraceEventChild(pid int, ws syscall.WaitStatus) (int, bool) {
	switch ws.TrapCause() {
	case syscall.PTRACE_EVENT_FORK, syscall.PTRACE_EVENT_VFORK, syscall.PTRACE_EVENT_CLONE:
	default:
		return 0, false
	}
	msg, err := syscall.PtraceGetEventMsg(pid)
	if err != nil || msg == 0 {
		return 0, false
	}
	return int(msg), true
}

func traceSyscallStop(out io.Writer, pid int, st *procState) error {
	var regs syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(pid, &regs); err != nil {
		return err
	}
	if st.entering {
		st.last = syscallFrame{
			num: regs.A7,
			args: [6]uint64{
				regs.A0,
				regs.A1,
				regs.A2,
				regs.A3,
				regs.A4,
				regs.A5,
			},
			pc: regs.Pc,
		}
		fmt.Fprintf(out, "%d -> %s(%s) pc=%#x\n", pid, syscallName(st.last.num), formatArgs(st.last.args), st.last.pc)
		st.entering = false
		return nil
	}
	ret := int64(regs.A0)
	fmt.Fprintf(out, "%d <- %s = %s\n", pid, syscallName(st.last.num), formatReturn(ret))
	st.entering = true
	return nil
}

func formatArgs(args [6]uint64) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a > 9 {
			parts[i] = fmt.Sprintf("%#x", a)
		} else {
			parts[i] = strconv.FormatUint(a, 10)
		}
	}
	return strings.Join(parts, ", ")
}

func formatReturn(ret int64) string {
	if ret < 0 && ret >= -4095 {
		return fmt.Sprintf("-1 %s", errnoName(uint64(-ret)))
	}
	if ret > 9 {
		return fmt.Sprintf("%#x", uint64(ret))
	}
	return strconv.FormatInt(ret, 10)
}

func syscallName(num uint64) string {
	if name := syscallNames[num]; name != "" {
		return name
	}
	return "sys_" + strconv.FormatUint(num, 10)
}

func errnoName(num uint64) string {
	if name := errnoNames[num]; name != "" {
		return name
	}
	return "errno_" + strconv.FormatUint(num, 10)
}

var syscallNames = map[uint64]string{
	17:  "getcwd",
	23:  "dup",
	24:  "dup3",
	25:  "fcntl",
	26:  "inotify_init1",
	27:  "inotify_add_watch",
	28:  "inotify_rm_watch",
	29:  "ioctl",
	32:  "flock",
	34:  "mkdirat",
	35:  "unlinkat",
	37:  "linkat",
	49:  "chdir",
	56:  "openat",
	57:  "close",
	59:  "pipe2",
	61:  "getdents64",
	62:  "lseek",
	63:  "read",
	64:  "write",
	65:  "readv",
	66:  "writev",
	67:  "pread64",
	68:  "pwrite64",
	78:  "readlinkat",
	79:  "newfstatat",
	80:  "fstat",
	82:  "fsync",
	88:  "utimensat",
	93:  "exit",
	94:  "exit_group",
	95:  "waitid",
	96:  "set_tid_address",
	98:  "futex",
	99:  "set_robust_list",
	101: "nanosleep",
	113: "clock_gettime",
	117: "ptrace",
	129: "kill",
	130: "tkill",
	131: "tgkill",
	134: "rt_sigaction",
	135: "rt_sigprocmask",
	139: "rt_sigreturn",
	153: "times",
	160: "uname",
	165: "getrusage",
	172: "getpid",
	173: "getppid",
	174: "getuid",
	175: "geteuid",
	176: "getgid",
	177: "getegid",
	178: "gettid",
	179: "sysinfo",
	180: "mq_open",
	214: "brk",
	215: "munmap",
	216: "mremap",
	222: "mmap",
	226: "mprotect",
	233: "madvise",
	260: "wait4",
	261: "prlimit64",
	278: "getrandom",
	280: "bpf",
	281: "execveat",
	282: "userfaultfd",
	283: "membarrier",
	285: "copy_file_range",
	291: "statx",
	293: "rseq",
	434: "pidfd_open",
	437: "openat2",
	439: "faccessat2",
	440: "process_madvise",
	449: "futex_waitv",
	450: "set_mempolicy_home_node",
}

var errnoNames = map[uint64]string{
	1:   "EPERM",
	2:   "ENOENT",
	3:   "ESRCH",
	4:   "EINTR",
	5:   "EIO",
	6:   "ENXIO",
	9:   "EBADF",
	10:  "ECHILD",
	11:  "EAGAIN",
	12:  "ENOMEM",
	13:  "EACCES",
	14:  "EFAULT",
	16:  "EBUSY",
	17:  "EEXIST",
	18:  "EXDEV",
	20:  "ENOTDIR",
	21:  "EISDIR",
	22:  "EINVAL",
	24:  "EMFILE",
	28:  "ENOSPC",
	30:  "EROFS",
	32:  "EPIPE",
	38:  "ENOSYS",
	95:  "EOPNOTSUPP",
	110: "ETIMEDOUT",
}
