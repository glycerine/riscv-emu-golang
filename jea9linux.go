package riscv

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	mathrand2 "math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

type Jea9LinuxClockMode uint8

const (
	Jea9ClockIdleJump Jea9LinuxClockMode = iota
	Jea9ClockICTick
)

type ClockPolicy uint8

const (
	ClockPolicyOnlyDeadlockAdvances ClockPolicy = iota
	ClockPolicyPRNG
	ClockPolicyFixed
)

func (p ClockPolicy) String() string {
	switch p {
	case ClockPolicyOnlyDeadlockAdvances:
		return "only-deadlock-advances"
	case ClockPolicyPRNG:
		return "prng"
	case ClockPolicyFixed:
		return "fixed"
	default:
		return "unknown"
	}
}

type Jea9LinuxNanosleepAdvanceMode uint8

const (
	Jea9NanosleepAdvanceRequested Jea9LinuxNanosleepAdvanceMode = iota
	Jea9NanosleepAdvanceFixed
)

type Jea9LinuxSchedulerMode uint8

const (
	Jea9SchedulerRoundRobin Jea9LinuxSchedulerMode = iota
	Jea9SchedulerDST
	Jea9SchedulerChaos
)

type Jea9LinuxSchedulerConfig struct {
	Mode Jea9LinuxSchedulerMode

	Seed [32]byte

	MinQuantumRetired uint64
	MaxQuantumRetired uint64

	LowPriorityNumerator   uint64
	LowPriorityDenominator uint64

	PriorityShuffleMinRetired uint64
	PriorityShuffleMaxRetired uint64

	ChaosWindowProbNumerator   uint64
	ChaosWindowProbDenominator uint64
	ChaosWindowMaxNS           int64
	ChaosBudgetNumerator       uint64
	ChaosBudgetDenominator     uint64
}

type jea9LinuxSchedPriority uint8

const (
	jea9LinuxSchedHigh jea9LinuxSchedPriority = iota
	jea9LinuxSchedLow
)

func (p jea9LinuxSchedPriority) String() string {
	switch p {
	case jea9LinuxSchedHigh:
		return "high"
	case jea9LinuxSchedLow:
		return "low"
	default:
		return "unknown"
	}
}

const (
	defaultJea9LinuxInstructionBudget = uint64(65536)
	defaultJea9LinuxStackReserve      = uint64(8 * Size1MB)
	defaultJea9LinuxClockPRNGMinNS    = int64(1_000_000)
	defaultJea9LinuxClockPRNGMaxNS    = int64(500_000_000)
	defaultJea9LinuxRoundRobinQuantum = uint64(65536)
	defaultJea9LinuxSchedMinQuantum   = uint64(100)
	defaultJea9LinuxSchedMaxQuantum   = uint64(1000)
	defaultJea9LinuxChaosWindowMaxNS  = int64(3_000_000_000)
)

var (
	ErrJea9LinuxBudget  = errors.New("jea9linux instruction budget expired")
	ErrJea9LinuxBlocked = errors.New("jea9linux has no runnable contexts")
)

const (
	jea9LinuxErrEFAULT       = int64(-14)
	jea9LinuxErrEINVAL       = int64(-22)
	jea9LinuxErrEBADF        = int64(-9)
	jea9LinuxErrENOENT       = int64(-2)
	jea9LinuxErrENOSYS       = int64(-38)
	jea9LinuxErrEPERM        = int64(-1)
	jea9LinuxErrESRCH        = int64(-3)
	jea9LinuxErrEIO          = int64(-5)
	jea9LinuxErrEACCES       = int64(-13)
	jea9LinuxErrENOMEM       = int64(-12)
	jea9LinuxErrEAGAIN       = int64(-11)
	jea9LinuxErrEEXIST       = int64(-17)
	jea9LinuxErrENOTDIR      = int64(-20)
	jea9LinuxErrEISDIR       = int64(-21)
	jea9LinuxErrESPIPE       = int64(-29)
	jea9LinuxErrENAMETOOLONG = int64(-36)
	jea9LinuxErrENOTTY       = int64(-25)
	jea9LinuxErrETIMEDOUT    = int64(-110)

	jea9LinuxSysEventfd2         = uint64(19)
	jea9LinuxSysEpollCreate1     = uint64(20)
	jea9LinuxSysEpollCtl         = uint64(21)
	jea9LinuxSysEpollPwait       = uint64(22)
	jea9LinuxSysFcntl            = uint64(25)
	jea9LinuxSysIoctl            = uint64(29)
	jea9LinuxSysOpenat           = uint64(56)
	jea9LinuxSysClose            = uint64(57)
	jea9LinuxSysPipe2            = uint64(59)
	jea9LinuxSysLseek            = uint64(62)
	jea9LinuxSysRead             = uint64(63)
	jea9LinuxSysWrite            = uint64(64)
	jea9LinuxSysPread64          = uint64(67)
	jea9LinuxSysPselect6         = uint64(72)
	jea9LinuxSysSetTidAddress    = uint64(96)
	jea9LinuxSysFutex            = uint64(98)
	jea9LinuxSysSetRobustList    = uint64(99)
	jea9LinuxSysSetitimer        = uint64(103)
	jea9LinuxSysTimerCreate      = uint64(107)
	jea9LinuxSysTimerSettime     = uint64(110)
	jea9LinuxSysTimerDelete      = uint64(111)
	jea9LinuxSysKill             = uint64(129)
	jea9LinuxSysTkill            = uint64(130)
	jea9LinuxSysTgkill           = uint64(131)
	jea9LinuxSysSigaltstack      = uint64(132)
	jea9LinuxSysRtSigaction      = uint64(134)
	jea9LinuxSysRtSigprocmask    = uint64(135)
	jea9LinuxSysRtSigreturn      = uint64(139)
	jea9LinuxSysExit             = uint64(93)
	jea9LinuxSysExitGroup        = uint64(94)
	jea9LinuxSysNanosleep        = uint64(101)
	jea9LinuxSysClockGettime     = uint64(113)
	jea9LinuxSysSchedGetAffinity = uint64(123)
	jea9LinuxSysSchedYield       = uint64(124)
	jea9LinuxSysUname            = uint64(160)
	jea9LinuxSysGetrlimit        = uint64(163)
	jea9LinuxSysPrctl            = uint64(167)
	jea9LinuxSysGettimeofday     = uint64(169)
	jea9LinuxSysGetpid           = uint64(172)
	jea9LinuxSysGettid           = uint64(178)
	jea9LinuxSysSysinfo          = uint64(179)
	jea9LinuxSysMunmap           = uint64(215)
	jea9LinuxSysBrk              = uint64(214)
	jea9LinuxSysClone            = uint64(220)
	jea9LinuxSysMmap             = uint64(222)
	jea9LinuxSysMprotect         = uint64(226)
	jea9LinuxSysMincore          = uint64(232)
	jea9LinuxSysMadvise          = uint64(233)
	jea9LinuxSysRiscvHwprobe     = uint64(258)
	jea9LinuxSysPrlimit64        = uint64(261)
	jea9LinuxSysGetrandom        = uint64(278)
	jea9LinuxSysFutexTime64      = uint64(422)

	jea9LinuxClockRealtime        = uint64(0)
	jea9LinuxClockMonotonic       = uint64(1)
	jea9LinuxClockRealtimeCoarse  = uint64(5)
	jea9LinuxClockMonotonicCoarse = uint64(6)

	jea9LinuxATNull   = uint64(0)
	jea9LinuxATPHDR   = uint64(3)
	jea9LinuxATPHENT  = uint64(4)
	jea9LinuxATPHNUM  = uint64(5)
	jea9LinuxATPAGESZ = uint64(6)
	jea9LinuxATENTRY  = uint64(9)
	jea9LinuxATUID    = uint64(11)
	jea9LinuxATEUID   = uint64(12)
	jea9LinuxATGID    = uint64(13)
	jea9LinuxATEGID   = uint64(14)
	jea9LinuxATPLAT   = uint64(15)
	jea9LinuxATHWCAP  = uint64(16)
	jea9LinuxATCLKTCK = uint64(17)
	jea9LinuxATSECURE = uint64(23)
	jea9LinuxATRANDOM = uint64(25)
	jea9LinuxATHWCAP2 = uint64(26)
	jea9LinuxATEXECFN = uint64(31)
)

const (
	jea9LinuxClockTicksPerSecond = uint64(100)
	jea9LinuxPlatform            = "riscv64"

	jea9LinuxFGetFD = uint64(1)
	jea9LinuxFSetFD = uint64(2)
	jea9LinuxFGetFL = uint64(3)
	jea9LinuxFSetFL = uint64(4)

	jea9LinuxTCGETS     = uint64(0x5401)
	jea9LinuxTCSETS     = uint64(0x5402)
	jea9LinuxTCSETSW    = uint64(0x5403)
	jea9LinuxTCSETSF    = uint64(0x5404)
	jea9LinuxTIOCGWINSZ = uint64(0x5413)
	jea9LinuxTIOCSWINSZ = uint64(0x5414)

	jea9LinuxTermiosSize = 44
	jea9LinuxWinsizeSize = 8

	jea9LinuxSeekSet = uint64(0)
	jea9LinuxSeekCur = uint64(1)
	jea9LinuxSeekEnd = uint64(2)

	jea9LinuxRLimitStack  = uint64(3)
	jea9LinuxRLimitNOFile = uint64(7)
	jea9LinuxRLimitAS     = uint64(9)

	jea9LinuxPRSetName = uint64(15)
	jea9LinuxPRGetName = uint64(16)
	jea9LinuxPRSetVMA  = uint64(0x53564d41)

	jea9LinuxProtRead  = uint64(1)
	jea9LinuxProtWrite = uint64(2)
	jea9LinuxProtExec  = uint64(4)
	jea9LinuxProtMask  = jea9LinuxProtRead | jea9LinuxProtWrite | jea9LinuxProtExec

	jea9LinuxPageMapped    = uint64(1 << 63)
	jea9LinuxPageNeedsZero = uint64(1 << 62)

	jea9LinuxMapPrivate   = uint64(2)
	jea9LinuxMapFixed     = uint64(0x10)
	jea9LinuxMapAnonymous = uint64(0x20)

	jea9LinuxFutexWait       = uint64(0)
	jea9LinuxFutexWake       = uint64(1)
	jea9LinuxFutexWaitBitset = uint64(9)
	jea9LinuxFutexWakeBitset = uint64(10)

	jea9LinuxFDNonblock = uint64(0x800)
	jea9LinuxFDCloexec  = uint64(0x80000)

	jea9LinuxOAccmode = uint64(0x3)
	jea9LinuxOWronly  = uint64(0x1)
	jea9LinuxORdwr    = uint64(0x2)
	jea9LinuxOCreat   = uint64(0x40)
	jea9LinuxOExcl    = uint64(0x80)
	jea9LinuxOTrunc   = uint64(0x200)
	jea9LinuxOAppend  = uint64(0x400)

	jea9LinuxEFDSemaphore = uint64(1)

	jea9LinuxEpollCtlAdd = uint64(1)
	jea9LinuxEpollCtlDel = uint64(2)
	jea9LinuxEpollCtlMod = uint64(3)
	jea9LinuxEpollIn     = uint32(0x001)
	jea9LinuxEpollOut    = uint32(0x004)

	jea9LinuxSIGSEGV = uint64(11)

	jea9LinuxSIGBlock   = uint64(0)
	jea9LinuxSIGUnblock = uint64(1)
	jea9LinuxSIGSetmask = uint64(2)

	jea9LinuxSignalDefault = uint64(0)
	jea9LinuxSignalIgnore  = uint64(1)
	jea9LinuxSASiginfo     = uint64(0x00000004)
	jea9LinuxSAOnstack     = uint64(0x08000000)
	jea9LinuxSSDisable     = uint64(2)

	jea9LinuxSignalCodeUser       = int32(0)
	jea9LinuxSignalCodeSEGVMapErr = int32(1)

	jea9LinuxSignalFrameSize       = uint64(2048)
	jea9LinuxSignalFrameSiginfoOff = uint64(128)
	jea9LinuxSignalFrameUctxOff    = uint64(256)
	jea9LinuxUContextSigmaskOff    = uint64(40)
	jea9LinuxUContextMContextOff   = uint64(176)

	jea9LinuxCloneVM            = uint64(0x00000100)
	jea9LinuxCloneFS            = uint64(0x00000200)
	jea9LinuxCloneFiles         = uint64(0x00000400)
	jea9LinuxCloneSighand       = uint64(0x00000800)
	jea9LinuxCloneThread        = uint64(0x00010000)
	jea9LinuxCloneSysvsem       = uint64(0x00040000)
	jea9LinuxCloneSetTLS        = uint64(0x00080000)
	jea9LinuxCloneParentSetTID  = uint64(0x00100000)
	jea9LinuxCloneChildClearTID = uint64(0x00200000)
	jea9LinuxCloneChildSetTID   = uint64(0x01000000)
)

type jea9LinuxFDKind uint8

const (
	jea9LinuxFDStdin jea9LinuxFDKind = iota + 1
	jea9LinuxFDStdout
	jea9LinuxFDStderr
	jea9LinuxFDRandom
	jea9LinuxFDFile
	jea9LinuxFDEventfd
	jea9LinuxFDEpoll
	jea9LinuxFDPipeRead
	jea9LinuxFDPipeWrite
	jea9LinuxFDHostFile
)

type jea9LinuxFD struct {
	kind           jea9LinuxFDKind
	data           []byte
	hostFile       *os.File
	off            int64
	flags          uint64
	eventfdCounter uint64
	epoll          *jea9LinuxEpoll
	pipe           *jea9LinuxPipe
	termios        [jea9LinuxTermiosSize]byte
	termiosSet     bool
	winsize        [jea9LinuxWinsizeSize]byte
	winsizeSet     bool
}

type jea9LinuxEpollRegistration struct {
	events uint32
	data   uint64
}

type jea9LinuxEpoll struct {
	registrations map[int]jea9LinuxEpollRegistration
	order         []int
}

type jea9LinuxPipe struct {
	buf     []byte
	readFD  int
	writeFD int
}

type jea9LinuxSignalAction struct {
	handler  uint64
	flags    uint64
	restorer uint64
	mask     uint64
}

type jea9LinuxSignalInfo struct {
	signo uint64
	code  int32
	pid   uint64
	uid   uint64
	addr  uint64
}

type jea9LinuxPendingSignal struct {
	sig  uint64
	info jea9LinuxSignalInfo
}

type jea9LinuxSignalFrameKey struct {
	tid uint64
	sp  uint64
}

type jea9LinuxSignalFrame struct {
	snapshot   jea9LinuxCPUSnapshot
	signalMask uint64
}

type Jea9LinuxOptions struct {
	EntropySeed       []byte
	ClockMode         Jea9LinuxClockMode
	ClockPolicy       ClockPolicy
	MonotonicStartNS  int64
	RealtimeOffsetNS  int64
	NSPerInstruction  int64
	NanosleepMode     Jea9LinuxNanosleepAdvanceMode
	NanosleepFixedNS  int64
	InstructionBudget uint64
	Scheduler         Jea9LinuxSchedulerConfig
	// Trace enables replay/debug recording. It is off by default for normal runs.
	Trace             bool
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	Files             map[string][]byte
	AllowAllHostFiles bool
	PID               uint64
	TID               uint64
}

type Jea9LinuxStartOptions struct {
	Args     []string
	Env      []string
	ExecPath string
	StackTop uint64
}

type Jea9LinuxSyscallTraceEntry struct {
	TID         uint64
	PC          uint64
	Num         uint64
	Args        [6]uint64
	Ret         int64
	Disposition NoteDisposition
}

type Jea9LinuxScheduleTraceEntry struct {
	Event             string
	TID               uint64
	NextTID           uint64
	FromPC            uint64
	NextPC            uint64
	MonotonicNS       int64
	RiscvInstrBegun   uint64 // cumulative guest instruction attempts begun by the hart
	SchedEventID      uint64
	SchedPRNGDraws    uint64
	SchedPRNGState    []byte
	RiscvInstrRetired uint64
	QuantumRetired    uint64
	Reason            string
	FromPriority      string
	ToPriority        string
	ChaosActive       bool
	ChaosUntilNS      int64
	ClockPolicy       string
	ClockAdvanceNS    int64
	ClockBeforeNS     int64
	ClockAfterNS      int64
}

type Jea9LinuxRandomTraceEntry struct {
	Source string
	TID    uint64
	N      uint64
	Flags  uint64
	Bytes  []byte
}

type Jea9LinuxClockTraceEntry struct {
	Source  string
	TID     uint64
	ClockID uint64
	NS      int64
}

type Jea9LinuxSyscallCount struct {
	Num   uint64
	Count uint64
}

type Jea9LinuxSyscallPCCount struct {
	PC    uint64
	Count uint64
}

type Jea9LinuxTraceSnapshot struct {
	Syscalls []Jea9LinuxSyscallTraceEntry
	Schedule []Jea9LinuxScheduleTraceEntry
	Random   []Jea9LinuxRandomTraceEntry
	Clock    []Jea9LinuxClockTraceEntry
}

type Jea9Linux struct {
	clockMode           Jea9LinuxClockMode
	clockPolicy         ClockPolicy
	clockFixedAdvanceNS int64
	clockPRNGMinNS      int64
	clockPRNGMaxNS      int64
	monotonicNS         int64
	realtimeOffsetNS    int64
	nsPerInstruction    int64
	nanosleepMode       Jea9LinuxNanosleepAdvanceMode
	nanosleepFixedNS    int64
	instructionBudget   uint64
	schedulerConfig     Jea9LinuxSchedulerConfig

	stdin             io.Reader
	stdout            io.Writer
	stderr            io.Writer
	fds               map[int]jea9LinuxFD
	nextFD            int
	files             map[string][]byte
	allowAllHostFiles bool
	pid               uint64
	tid               uint64

	rootSeed      [32]byte
	randomCounter uint64
	randomBuf     [32]byte
	randomOff     int

	schedRNG          *mathrand2.ChaCha8
	schedPRNGSnapshot []byte
	schedDraws        uint64
	schedEventID      uint64

	currentQuantumRetired      uint64
	nextScheduleAtRetired      uint64
	nextPriorityShuffleRetired uint64
	chaosActive                bool
	chaosStartNS               int64
	chaosUntilNS               int64
	chaosBlockedNS             int64
	lastClockAdvanceReason     string
	lastClockAdvanceNS         int64
	lastClockBeforeNS          int64
	lastClockAfterNS           int64

	budgetYields       uint64
	blocked            bool
	blockedUntil       int64
	blockedHasDeadline bool
	threadName         string
	vm                 *jea9LinuxVM

	contexts              map[uint64]*jea9LinuxContext
	contextOrder          []uint64
	currentTID            uint64
	nextTID               uint64
	loadedGuestContexts   int
	futexWaiters          map[uint64][]uint64
	timedFutexWaiters     int
	timedEpollWaiters     int
	timedNanosleepWaiters int
	signalActions         map[uint64]jea9LinuxSignalAction
	signalFrames          map[jea9LinuxSignalFrameKey]jea9LinuxSignalFrame
	signalRestorer        uint64
	traceEnabled          bool
	trace                 Jea9LinuxTraceSnapshot
	syscallCount          uint64
	syscallCounts         [512]uint64
	syscallCountOutside   uint64
	syscallPCCounts       map[uint64]uint64
	nanosleepCount        uint64
	nanosleepTotalNS      uint64
	nanosleepMaxNS        uint64
	activeEcallTrap       jea9LinuxEcallTrapFrame
}

type jea9LinuxAuxEntry struct {
	tag uint64
	val uint64
}

type jea9LinuxVM struct {
	pages    map[uint64]uint64
	brk      uint64
	minBrk   uint64
	mmapNext uint64
}

type jea9LinuxContextState uint8
type jea9LinuxWaitKind uint8

const (
	jea9LinuxContextRunnable jea9LinuxContextState = iota + 1
	jea9LinuxContextWaiting
	jea9LinuxContextExited
)

const (
	jea9LinuxWaitNone jea9LinuxWaitKind = iota
	jea9LinuxWaitFutex
	jea9LinuxWaitEpoll
	jea9LinuxWaitNanosleep
)

func (s jea9LinuxContextState) String() string {
	switch s {
	case jea9LinuxContextRunnable:
		return "runnable"
	case jea9LinuxContextWaiting:
		return "waiting"
	case jea9LinuxContextExited:
		return "exited"
	default:
		return "unknown"
	}
}

type jea9LinuxCPUSnapshot struct {
	// LR/SC reservations intentionally live on CPU, not in saved contexts:
	// a reservation is hart state and must not be resurrected by a thread switch.
	pc      uint64
	x       [32]uint64
	f       [32]uint64
	fcsr    uint32
	mtvec   uint64
	mepc    uint64
	mcause  uint64
	mstatus uint64
	mtval   uint64
}

type jea9LinuxEcallTrapFrame struct {
	active   bool
	trapPC   uint64
	resumePC uint64
	cause    uint64
	insnLen  uint8
}

type jea9LinuxContext struct {
	tid             uint64
	state           jea9LinuxContextState
	snapshot        jea9LinuxCPUSnapshot
	syscallTrap     jea9LinuxEcallTrapFrame
	schedPriority   jea9LinuxSchedPriority
	clearChildTID   uint64
	robustList      uint64
	robustListLen   uint64
	waitKind        jea9LinuxWaitKind
	waitAddr        uint64
	waitDeadlineNS  int64
	waitHasDeadline bool
	waitFD          int
	waitEventAddr   uint64
	waitMaxEvents   uint64
	signalMask      uint64
	pendingSignals  []jea9LinuxPendingSignal
	sigaltSP        uint64
	sigaltSize      uint64
	sigaltFlags     uint64
}

type jea9LinuxStackBuilder struct {
	cpu *CPU
	sp  uint64
}

type jea9LinuxZoneInfoSource struct {
	guestPath string
	hostPath  string
}

var (
	jea9LinuxTimeZoneFilesOnce sync.Once
	jea9LinuxTimeZoneFilesMemo map[string][]byte
)

func NewJea9Linux(opts Jea9LinuxOptions) *Jea9Linux {
	jos := &Jea9Linux{
		clockMode:         opts.ClockMode,
		clockPolicy:       opts.ClockPolicy,
		clockPRNGMinNS:    defaultJea9LinuxClockPRNGMinNS,
		clockPRNGMaxNS:    defaultJea9LinuxClockPRNGMaxNS,
		monotonicNS:       opts.MonotonicStartNS,
		realtimeOffsetNS:  opts.RealtimeOffsetNS,
		nsPerInstruction:  opts.NSPerInstruction,
		nanosleepMode:     opts.NanosleepMode,
		nanosleepFixedNS:  opts.NanosleepFixedNS,
		instructionBudget: opts.InstructionBudget,
		schedulerConfig:   opts.Scheduler,
		stdin:             opts.Stdin,
		stdout:            opts.Stdout,
		stderr:            opts.Stderr,
		fds:               make(map[int]jea9LinuxFD),
		nextFD:            3,
		files:             make(map[string][]byte),
		allowAllHostFiles: opts.AllowAllHostFiles,
		pid:               opts.PID,
		tid:               opts.TID,
		threadName:        "jea9linux",
		signalActions:     make(map[uint64]jea9LinuxSignalAction),
		signalFrames:      make(map[jea9LinuxSignalFrameKey]jea9LinuxSignalFrame),
		traceEnabled:      opts.Trace,
	}
	if jos.instructionBudget == 0 {
		jos.instructionBudget = defaultJea9LinuxInstructionBudget
	}
	if jos.nsPerInstruction == 0 {
		jos.nsPerInstruction = 1
	}
	jos.normalizeSchedulerConfig()
	if jos.stdout == nil {
		jos.stdout = io.Discard
	}
	if jos.stderr == nil {
		jos.stderr = io.Discard
	}
	if jos.pid == 0 {
		jos.pid = 1
	}
	if jos.tid == 0 {
		jos.tid = jos.pid
	}
	jos.fds[0] = jea9LinuxFD{kind: jea9LinuxFDStdin}
	jos.fds[1] = jea9LinuxFD{kind: jea9LinuxFDStdout}
	jos.fds[2] = jea9LinuxFD{kind: jea9LinuxFDStderr}
	for path, data := range jea9LinuxTimeZoneFiles() {
		jos.files[path] = data
	}
	for path, data := range opts.Files {
		jos.files[path] = append([]byte(nil), data...)
	}
	jos.rootSeed = deriveJea9LinuxRootSeed(opts.EntropySeed)
	jos.randomOff = len(jos.randomBuf)
	jos.initSchedulerPRNG()
	return jos
}

func (jos *Jea9Linux) normalizeSchedulerConfig() {
	if jos.schedulerConfig.MinQuantumRetired == 0 {
		if jos.schedulerConfig.Mode == Jea9SchedulerRoundRobin {
			jos.schedulerConfig.MinQuantumRetired = defaultJea9LinuxRoundRobinQuantum
		} else {
			jos.schedulerConfig.MinQuantumRetired = defaultJea9LinuxSchedMinQuantum
		}
	}
	if jos.schedulerConfig.MaxQuantumRetired < jos.schedulerConfig.MinQuantumRetired {
		jos.schedulerConfig.MaxQuantumRetired = jos.schedulerConfig.MinQuantumRetired
	}
	if jos.schedulerConfig.LowPriorityDenominator == 0 {
		jos.schedulerConfig.LowPriorityDenominator = 10
	}
	if jos.schedulerConfig.LowPriorityNumerator > jos.schedulerConfig.LowPriorityDenominator {
		jos.schedulerConfig.LowPriorityNumerator = jos.schedulerConfig.LowPriorityDenominator
	}
	if jos.schedulerConfig.PriorityShuffleMinRetired == 0 {
		jos.schedulerConfig.PriorityShuffleMinRetired = jos.schedulerConfig.MinQuantumRetired * 10
	}
	if jos.schedulerConfig.PriorityShuffleMaxRetired < jos.schedulerConfig.PriorityShuffleMinRetired {
		jos.schedulerConfig.PriorityShuffleMaxRetired = jos.schedulerConfig.PriorityShuffleMinRetired
	}
	if jos.schedulerConfig.ChaosWindowProbDenominator == 0 {
		jos.schedulerConfig.ChaosWindowProbDenominator = 100
	}
	if jos.schedulerConfig.ChaosWindowMaxNS <= 0 {
		jos.schedulerConfig.ChaosWindowMaxNS = defaultJea9LinuxChaosWindowMaxNS
	}
	if jos.schedulerConfig.ChaosBudgetDenominator == 0 {
		jos.schedulerConfig.ChaosBudgetDenominator = 5
	}
	if jos.schedulerConfig.ChaosBudgetNumerator == 0 {
		jos.schedulerConfig.ChaosBudgetNumerator = 1
	}
}

func jea9LinuxTimeZoneFiles() map[string][]byte {
	jea9LinuxTimeZoneFilesOnce.Do(func() {
		jea9LinuxTimeZoneFilesMemo = loadJea9LinuxTimeZoneFiles(defaultJea9LinuxTimeZoneSources())
	})
	return jea9LinuxTimeZoneFilesMemo
}

func defaultJea9LinuxTimeZoneSources() []jea9LinuxZoneInfoSource {
	sources := []jea9LinuxZoneInfoSource{
		{guestPath: "/usr/share/zoneinfo/", hostPath: "/usr/share/zoneinfo/"},
		{guestPath: "/usr/share/lib/zoneinfo/", hostPath: "/usr/share/lib/zoneinfo/"},
		{guestPath: "/usr/lib/locale/TZ/", hostPath: "/usr/lib/locale/TZ/"},
		{guestPath: "/etc/zoneinfo", hostPath: "/etc/zoneinfo"},
	}
	for _, goroot := range uniqueJea9LinuxStrings([]string{runtime.GOROOT(), "/usr/local/go"}) {
		if goroot == "" {
			continue
		}
		zip := filepath.ToSlash(filepath.Join(goroot, "lib", "time", "zoneinfo.zip"))
		sources = append(sources, jea9LinuxZoneInfoSource{guestPath: zip, hostPath: zip})
	}
	return sources
}

func uniqueJea9LinuxStrings(in []string) []string {
	var out []string
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func loadJea9LinuxTimeZoneFiles(sources []jea9LinuxZoneInfoSource) map[string][]byte {
	files := make(map[string][]byte)
	for _, src := range sources {
		if strings.HasSuffix(src.guestPath, ".zip") {
			addJea9LinuxTimeZoneFile(files, src.guestPath, src.hostPath)
			continue
		}
		addJea9LinuxTimeZoneDir(files, src.guestPath, src.hostPath)
	}
	if len(files) == 0 {
		return nil
	}
	return files
}

func addJea9LinuxTimeZoneFile(files map[string][]byte, guestPath, hostPath string) {
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return
	}
	files[guestPath] = data
}

func addJea9LinuxTimeZoneDir(files map[string][]byte, guestRoot, hostRoot string) {
	walkRoot, err := filepath.EvalSymlinks(hostRoot)
	if err != nil {
		walkRoot = hostRoot
	}
	info, err := os.Stat(walkRoot)
	if err != nil || !info.IsDir() {
		return
	}
	_ = filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeType != 0 && d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(walkRoot, path)
		if err != nil || rel == "." {
			return nil
		}
		files[guestRoot+"/"+filepath.ToSlash(rel)] = data
		return nil
	})
}

func deriveJea9LinuxRootSeed(seed []byte) [32]byte {
	if seed == nil {
		return sha256.Sum256([]byte("jea9linux default deterministic seed v1"))
	}
	h := sha256.New()
	_, _ = h.Write([]byte("jea9linux entropy root v1"))
	_, _ = h.Write(seed)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func deriveJea9LinuxSchedulerSeed(rootSeed [32]byte, override [32]byte) [32]byte {
	if !jea9LinuxZero32(override) {
		return override
	}
	h := sha256.New()
	_, _ = h.Write(rootSeed[:])
	_, _ = h.Write([]byte("jea9linux-scheduler-chacha8-v1"))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func jea9LinuxZero32(v [32]byte) bool {
	for _, b := range v {
		if b != 0 {
			return false
		}
	}
	return true
}

func (jos *Jea9Linux) initSchedulerPRNG() {
	seed := deriveJea9LinuxSchedulerSeed(jos.rootSeed, jos.schedulerConfig.Seed)
	jos.schedRNG = mathrand2.NewChaCha8(seed)
	jos.schedDraws = 0
	jos.schedEventID = 0
	jos.commitSchedulerPRNGState(nil)
}

func (jos *Jea9Linux) commitSchedulerPRNGState(cpu *CPU) {
	_ = cpu
	if jos == nil || jos.schedRNG == nil {
		return
	}
	snap, err := jos.schedRNG.MarshalBinary()
	if err != nil {
		panic("jea9linux: marshal scheduler PRNG: " + err.Error())
	}
	jos.schedPRNGSnapshot = append(jos.schedPRNGSnapshot[:0], snap...)
}

func (jos *Jea9Linux) schedUint64(cpu *CPU, why string) uint64 {
	_ = why
	if jos.schedRNG == nil {
		jos.initSchedulerPRNG()
	}
	v := jos.schedRNG.Uint64()
	jos.schedDraws++
	jos.commitSchedulerPRNGState(cpu)
	return v
}

func (jos *Jea9Linux) schedN(cpu *CPU, n uint64, why string) uint64 {
	if n == 0 {
		return 0
	}
	limit := ^uint64(0) - (^uint64(0) % n)
	for {
		v := jos.schedUint64(cpu, why)
		if v < limit {
			return v % n
		}
	}
}

func (jos *Jea9Linux) schedulerActive() bool {
	return true
}

func (jos *Jea9Linux) nextSchedulerEvent(cpu *CPU, reason string) {
	_ = reason
	jos.schedEventID++
	jos.commitSchedulerPRNGState(cpu)
}

func (jos *Jea9Linux) drawSchedulerQuantum(cpu *CPU) uint64 {
	minQ := jos.schedulerConfig.MinQuantumRetired
	maxQ := jos.schedulerConfig.MaxQuantumRetired
	if maxQ < minQ {
		maxQ = minQ
	}
	if jos.schedulerConfig.Mode == Jea9SchedulerRoundRobin || minQ == maxQ {
		return minQ
	}
	span := maxQ - minQ + 1
	return minQ + jos.schedN(cpu, span, "scheduler-quantum")
}

func (jos *Jea9Linux) installNextSchedulerQuantum(cpu *CPU) uint64 {
	q := jos.drawSchedulerQuantum(cpu)
	jos.currentQuantumRetired = q
	jos.nextScheduleAtRetired = cpu.RiscvInstrRetired() + q
	return q
}

func (jos *Jea9Linux) schedulerRemainingRetired(cpu *CPU) (uint64, bool) {
	if !jos.schedulerActive() {
		return 0, false
	}
	if jos.nextScheduleAtRetired == 0 {
		jos.installNextSchedulerQuantum(cpu)
	}
	retired := cpu.RiscvInstrRetired()
	if retired >= jos.nextScheduleAtRetired {
		return 0, true
	}
	return jos.nextScheduleAtRetired - retired, true
}

func (jos *Jea9Linux) drawSchedulerPriority(cpu *CPU) jea9LinuxSchedPriority {
	denom := jos.schedulerConfig.LowPriorityDenominator
	if denom == 0 {
		denom = 10
	}
	if jos.schedulerConfig.LowPriorityNumerator != 0 &&
		jos.schedN(cpu, denom, "scheduler-priority") < jos.schedulerConfig.LowPriorityNumerator {
		return jea9LinuxSchedLow
	}
	return jea9LinuxSchedHigh
}

func (jos *Jea9Linux) reshuffleSchedulerPriorities(cpu *CPU) {
	for _, tid := range jos.contextOrder {
		ctx := jos.contexts[tid]
		if ctx == nil || ctx.state == jea9LinuxContextExited {
			continue
		}
		ctx.schedPriority = jos.drawSchedulerPriority(cpu)
	}
	span := jos.schedulerConfig.PriorityShuffleMaxRetired - jos.schedulerConfig.PriorityShuffleMinRetired + 1
	next := jos.schedulerConfig.PriorityShuffleMinRetired + jos.schedN(cpu, span, "scheduler-priority-shuffle")
	jos.nextPriorityShuffleRetired = cpu.RiscvInstrRetired() + next
}

func (jos *Jea9Linux) maybeReshuffleSchedulerPriorities(cpu *CPU) {
	if jos.schedulerConfig.Mode != Jea9SchedulerDST && jos.schedulerConfig.Mode != Jea9SchedulerChaos {
		return
	}
	if jos.nextPriorityShuffleRetired == 0 || cpu.RiscvInstrRetired() >= jos.nextPriorityShuffleRetired {
		jos.reshuffleSchedulerPriorities(cpu)
	}
}

func (jos *Jea9Linux) ClockMode() Jea9LinuxClockMode { return jos.clockMode }

func (jos *Jea9Linux) SetClockMode(mode Jea9LinuxClockMode) { jos.clockMode = mode }

func (jos *Jea9Linux) ClockPolicy() ClockPolicy { return jos.clockPolicy }

func (jos *Jea9Linux) SetClockPolicy(policy ClockPolicy) { jos.clockPolicy = policy }

func (jos *Jea9Linux) ClockFixedAdvanceNS() int64 { return jos.clockFixedAdvanceNS }

func (jos *Jea9Linux) SetClockFixedAdvanceNS(ns int64) { jos.clockFixedAdvanceNS = ns }

func (jos *Jea9Linux) SchedulerPRNGState() []byte {
	if jos == nil {
		return nil
	}
	return append([]byte(nil), jos.schedPRNGSnapshot...)
}

func (jos *Jea9Linux) SchedulerPRNGDraws() uint64 {
	if jos == nil {
		return 0
	}
	return jos.schedDraws
}

func (jos *Jea9Linux) SchedulerEventID() uint64 {
	if jos == nil {
		return 0
	}
	return jos.schedEventID
}

func (jos *Jea9Linux) InstructionBudget() uint64 { return jos.instructionBudget }

func (jos *Jea9Linux) SetNSPerInstruction(ns int64) {
	if ns == 0 {
		ns = 1
	}
	jos.nsPerInstruction = ns
}

func (jos *Jea9Linux) SetMonotonicNS(ns int64) {
	jos.monotonicNS = ns
	jos.refreshBlocked()
}

func (jos *Jea9Linux) MonotonicNS() int64 { return jos.monotonicNS }

func (jos *Jea9Linux) BudgetYields() uint64 { return jos.budgetYields }

func (jos *Jea9Linux) TraceSnapshot() Jea9LinuxTraceSnapshot {
	out := Jea9LinuxTraceSnapshot{
		Syscalls: append([]Jea9LinuxSyscallTraceEntry(nil), jos.trace.Syscalls...),
		Schedule: append([]Jea9LinuxScheduleTraceEntry(nil),
			jos.trace.Schedule...),
		Clock: append([]Jea9LinuxClockTraceEntry(nil), jos.trace.Clock...),
	}
	for i := range out.Schedule {
		out.Schedule[i].SchedPRNGState = append([]byte(nil), out.Schedule[i].SchedPRNGState...)
	}
	if len(jos.trace.Random) > 0 {
		out.Random = make([]Jea9LinuxRandomTraceEntry, len(jos.trace.Random))
		for i := range jos.trace.Random {
			out.Random[i] = jos.trace.Random[i]
			out.Random[i].Bytes = append([]byte(nil), jos.trace.Random[i].Bytes...)
		}
	}
	return out
}

func (jos *Jea9Linux) SyscallCount() uint64 {
	if jos == nil {
		return 0
	}
	return jos.syscallCount
}

func (jos *Jea9Linux) SyscallCountByNumber(num uint64) uint64 {
	if jos == nil {
		return 0
	}
	if num < uint64(len(jos.syscallCounts)) {
		return jos.syscallCounts[num]
	}
	if num == ^uint64(0) {
		return jos.syscallCountOutside
	}
	return 0
}

func (jos *Jea9Linux) TopSyscallCounts(limit int) []Jea9LinuxSyscallCount {
	if jos == nil || limit <= 0 {
		return nil
	}
	counts := make([]Jea9LinuxSyscallCount, 0, limit)
	used := make([]bool, len(jos.syscallCounts))
	for len(counts) < limit {
		var bestNum uint64
		var bestCount uint64
		for i, count := range jos.syscallCounts {
			if used[i] || count <= bestCount {
				continue
			}
			bestNum = uint64(i)
			bestCount = count
		}
		if bestCount == 0 {
			break
		}
		used[bestNum] = true
		counts = append(counts, Jea9LinuxSyscallCount{Num: bestNum, Count: bestCount})
	}
	if jos.syscallCountOutside != 0 && len(counts) < limit {
		counts = append(counts, Jea9LinuxSyscallCount{Num: ^uint64(0), Count: jos.syscallCountOutside})
	}
	return counts
}

func (jos *Jea9Linux) TopSyscallPCCounts(limit int) []Jea9LinuxSyscallPCCount {
	if jos == nil || limit <= 0 || len(jos.syscallPCCounts) == 0 {
		return nil
	}
	counts := make([]Jea9LinuxSyscallPCCount, 0, limit)
	used := make(map[uint64]bool, limit)
	for len(counts) < limit {
		var bestPC uint64
		var bestCount uint64
		for pc, count := range jos.syscallPCCounts {
			if used[pc] || count <= bestCount {
				continue
			}
			bestPC = pc
			bestCount = count
		}
		if bestCount == 0 {
			break
		}
		used[bestPC] = true
		counts = append(counts, Jea9LinuxSyscallPCCount{PC: bestPC, Count: bestCount})
	}
	return counts
}

func (jos *Jea9Linux) NanosleepStats() (count, totalNS, maxNS uint64) {
	if jos == nil {
		return 0, 0, 0
	}
	return jos.nanosleepCount, jos.nanosleepTotalNS, jos.nanosleepMaxNS
}

func (jos *Jea9Linux) traceTID() uint64 {
	if jos.currentTID != 0 {
		return jos.currentTID
	}
	if jos.tid != 0 {
		return jos.tid
	}
	return jos.pid
}

func (jos *Jea9Linux) recordSyscallTrace(cpu *CPU, n Note, args SyscallArgs, d NoteDisposition) {
	if !jos.traceEnabled {
		return
	}
	jos.trace.Syscalls = append(jos.trace.Syscalls, Jea9LinuxSyscallTraceEntry{
		TID: jos.traceTID(),
		PC:  n.PC,
		Num: args.Num,
		Args: [6]uint64{
			args.A0,
			args.A1,
			args.A2,
			args.A3,
			args.A4,
			args.A5,
		},
		Ret:         int64(cpu.Reg(10)),
		Disposition: d,
	})
}

func (jos *Jea9Linux) recordScheduleTrace(cpu *CPU, event string, tid, nextTID uint64) {
	jos.recordScheduleTracePC(cpu, event, tid, nextTID, cpu.PC(), cpu.PC())
}

func (jos *Jea9Linux) recordScheduleTracePC(cpu *CPU, event string, tid, nextTID, fromPC, nextPC uint64) {
	if !jos.traceEnabled {
		jos.clearLastClockAdvance()
		return
	}
	jos.trace.Schedule = append(jos.trace.Schedule, Jea9LinuxScheduleTraceEntry{
		Event:             event,
		TID:               tid,
		NextTID:           nextTID,
		FromPC:            fromPC,
		NextPC:            nextPC,
		MonotonicNS:       jos.monotonicNS,
		RiscvInstrBegun:   cpu.RiscvInstrBegun(),
		SchedEventID:      jos.schedEventID,
		SchedPRNGDraws:    jos.schedDraws,
		SchedPRNGState:    append([]byte(nil), jos.schedPRNGSnapshot...),
		RiscvInstrRetired: cpu.RiscvInstrRetired(),
		QuantumRetired:    jos.currentQuantumRetired,
		Reason:            jos.scheduleTraceReason(event),
		FromPriority:      jos.contextPriorityString(tid),
		ToPriority:        jos.contextPriorityString(nextTID),
		ChaosActive:       jos.chaosActive,
		ChaosUntilNS:      jos.chaosUntilNS,
		ClockPolicy:       jos.clockPolicy.String(),
		ClockAdvanceNS:    jos.lastClockAdvanceNS,
		ClockBeforeNS:     jos.lastClockBeforeNS,
		ClockAfterNS:      jos.lastClockAfterNS,
	})
	jos.clearLastClockAdvance()
}

func (jos *Jea9Linux) scheduleTraceReason(event string) string {
	if jos.lastClockAdvanceReason != "" {
		return jos.lastClockAdvanceReason
	}
	return event
}

func (jos *Jea9Linux) contextPriorityString(tid uint64) string {
	ctx := jos.contexts[tid]
	if ctx == nil {
		return ""
	}
	return ctx.schedPriority.String()
}

func (jos *Jea9Linux) noteClockAdvance(reason string, before int64) {
	jos.lastClockAdvanceReason = reason
	jos.lastClockBeforeNS = before
	jos.lastClockAfterNS = jos.monotonicNS
	jos.lastClockAdvanceNS = jos.monotonicNS - before
}

func (jos *Jea9Linux) clearLastClockAdvance() {
	jos.lastClockAdvanceReason = ""
	jos.lastClockAdvanceNS = 0
	jos.lastClockBeforeNS = 0
	jos.lastClockAfterNS = 0
}

func (jos *Jea9Linux) recordRandomTrace(source string, n, flags uint64, b []byte) {
	if !jos.traceEnabled {
		return
	}
	jos.trace.Random = append(jos.trace.Random, Jea9LinuxRandomTraceEntry{
		Source: source,
		TID:    jos.traceTID(),
		N:      n,
		Flags:  flags,
		Bytes:  append([]byte(nil), b...),
	})
}

func (jos *Jea9Linux) recordClockTrace(source string, clockID uint64, ns int64) {
	if !jos.traceEnabled {
		return
	}
	jos.trace.Clock = append(jos.trace.Clock, Jea9LinuxClockTraceEntry{
		Source:  source,
		TID:     jos.traceTID(),
		ClockID: clockID,
		NS:      ns,
	})
}

func (jos *Jea9Linux) Blocked() bool {
	jos.refreshBlocked()
	return jos.blocked
}

func (jos *Jea9Linux) fillRandom(dst []byte) {
	for len(dst) > 0 {
		if jos.randomOff >= len(jos.randomBuf) {
			jos.randomBuf = jos.randomBlock("sys-random-v1", jos.randomCounter)
			jos.randomCounter++
			jos.randomOff = 0
		}
		n := copy(dst, jos.randomBuf[jos.randomOff:])
		jos.randomOff += n
		dst = dst[n:]
	}
}

func (jos *Jea9Linux) randomBlock(label string, counter uint64) [32]byte {
	h := sha256.New()
	_, _ = h.Write(jos.rootSeed[:])
	_, _ = h.Write([]byte(label))
	var ctr [8]byte
	binary.LittleEndian.PutUint64(ctr[:], counter)
	_, _ = h.Write(ctr[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func newJea9LinuxVM(memSize uint64) *jea9LinuxVM {
	brk := jea9LinuxAlignUp(Size1MB * 2)
	if brk >= memSize {
		brk = jea9LinuxAlignUp(memSize / 4)
	}
	return &jea9LinuxVM{
		pages:    make(map[uint64]uint64),
		brk:      brk,
		minBrk:   brk,
		mmapNext: jea9LinuxDefaultMmapBase(memSize),
	}
}

func jea9LinuxDefaultMmapBase(memSize uint64) uint64 {
	base := jea9LinuxAlignUp(memSize / 4)
	if base < Size1MB*4 && memSize > Size1MB*8 {
		base = Size1MB * 4
	}
	if base >= memSize {
		base = GuestPageSize
	}
	return base
}

func (jos *Jea9Linux) ensureVM(cpu *CPU) *jea9LinuxVM {
	if jos.vm == nil {
		jos.vm = newJea9LinuxVM(cpu.mem.Size())
	}
	(&cpu.mem).setAccessOverlay(jos.vm)
	return jos.vm
}

func (jos *Jea9Linux) ensureScheduler(cpu *CPU) *jea9LinuxContext {
	jos.ensureSignalState()
	if jos.contexts == nil {
		jos.contexts = make(map[uint64]*jea9LinuxContext)
		jos.futexWaiters = make(map[uint64][]uint64)
		jos.currentTID = jos.tid
		if jos.currentTID == 0 {
			jos.currentTID = jos.pid
		}
		jos.tid = jos.currentTID
		jos.nextTID = jos.currentTID + 1
		ctx := &jea9LinuxContext{
			tid:      jos.currentTID,
			state:    jea9LinuxContextRunnable,
			snapshot: snapshotJea9LinuxCPU(cpu),
		}
		jos.attachActiveEcallTrap(ctx)
		jos.contexts[ctx.tid] = ctx
		jos.contextOrder = append(jos.contextOrder, ctx.tid)
		jos.loadedGuestContexts = 1
		return ctx
	}
	ctx := jos.contexts[jos.currentTID]
	if ctx == nil {
		ctx = &jea9LinuxContext{
			tid:      jos.currentTID,
			state:    jea9LinuxContextRunnable,
			snapshot: snapshotJea9LinuxCPU(cpu),
		}
		jos.attachActiveEcallTrap(ctx)
		jos.contexts[ctx.tid] = ctx
		jos.contextOrder = append(jos.contextOrder, ctx.tid)
	}
	if jos.futexWaiters == nil {
		jos.futexWaiters = make(map[uint64][]uint64)
	}
	if jos.nextTID <= jos.currentTID {
		jos.nextTID = jos.currentTID + 1
	}
	jos.loadedGuestContexts = 1
	jos.attachActiveEcallTrap(ctx)
	return ctx
}

func (jos *Jea9Linux) ensureSignalState() {
	if jos.signalActions == nil {
		jos.signalActions = make(map[uint64]jea9LinuxSignalAction)
	}
	if jos.signalFrames == nil {
		jos.signalFrames = make(map[jea9LinuxSignalFrameKey]jea9LinuxSignalFrame)
	}
}

func snapshotJea9LinuxCPU(cpu *CPU) jea9LinuxCPUSnapshot {
	return jea9LinuxCPUSnapshot{
		pc:      cpu.pc,
		x:       cpu.x,
		f:       cpu.f,
		fcsr:    cpu.fcsr,
		mtvec:   cpu.mtvec,
		mepc:    cpu.mepc,
		mcause:  cpu.mcause,
		mstatus: cpu.mstatus,
		mtval:   cpu.mtval,
	}
}

func restoreJea9LinuxCPU(cpu *CPU, snap jea9LinuxCPUSnapshot) {
	cpu.pc = snap.pc
	cpu.x = snap.x
	cpu.x[0] = 0
	cpu.f = snap.f
	cpu.fcsr = snap.fcsr
	cpu.mtvec = snap.mtvec
	cpu.mepc = snap.mepc
	cpu.mcause = snap.mcause
	cpu.mstatus = snap.mstatus
	cpu.mtval = snap.mtval
}

func jea9LinuxEcallTrapFromNote(n Note) jea9LinuxEcallTrapFrame {
	if !IsEcall(n) || n.InsnLen == 0 {
		return jea9LinuxEcallTrapFrame{}
	}
	return jea9LinuxEcallTrapFrame{
		active:   true,
		trapPC:   n.PC,
		resumePC: n.PC + uint64(n.InsnLen),
		cause:    n.Cause,
		insnLen:  n.InsnLen,
	}
}

func (jos *Jea9Linux) attachActiveEcallTrap(ctx *jea9LinuxContext) {
	if ctx == nil || !jos.activeEcallTrap.active {
		return
	}
	if ctx.tid != jos.currentTID {
		return
	}
	ctx.syscallTrap = jos.activeEcallTrap
}

func (jos *Jea9Linux) completeContextEcallTrap(ctx *jea9LinuxContext) {
	if ctx == nil || !ctx.syscallTrap.active {
		return
	}
	if ctx.snapshot.pc == ctx.syscallTrap.trapPC {
		ctx.snapshot.pc = ctx.syscallTrap.resumePC
	}
	ctx.syscallTrap = jea9LinuxEcallTrapFrame{}
}

func (jos *Jea9Linux) syscallReturnSnapshot(cpu *CPU, retval uint64) jea9LinuxCPUSnapshot {
	snap := snapshotJea9LinuxCPU(cpu)
	snap.x[10] = retval
	snap.x[0] = 0
	if jos.activeEcallTrap.active {
		snap.pc = jos.activeEcallTrap.resumePC
	}
	return snap
}

func (jos *Jea9Linux) finishHandledEcall(cpu *CPU, startTID uint64, trap jea9LinuxEcallTrapFrame, disp NoteDisposition, resume bool) {
	if !resume || !trap.active || disp != NoteHandled {
		return
	}
	var ctx *jea9LinuxContext
	if jos.contexts != nil && startTID != 0 {
		ctx = jos.contexts[startTID]
	}
	if ctx != nil && ctx.state == jea9LinuxContextRunnable && ctx.snapshot.pc == trap.trapPC {
		jos.completeContextEcallTrap(ctx)
	}
	if ctx == nil || (jos.currentTID == startTID && ctx.state == jea9LinuxContextRunnable) {
		if cpu.PC() == trap.trapPC {
			cpu.clearReservation()
			cpu.SetPC(trap.resumePC)
		} else if cpu.PC() == trap.resumePC {
			cpu.clearReservation()
		}
	}
}

func (jos *Jea9Linux) loadContext(cpu *CPU, tid uint64) bool {
	ctx := jos.contexts[tid]
	if ctx == nil || ctx.state != jea9LinuxContextRunnable {
		return false
	}
	jos.completeContextEcallTrap(ctx)
	restoreJea9LinuxCPU(cpu, ctx.snapshot)
	cpu.clearReservation()
	jos.currentTID = tid
	jos.tid = tid
	jos.loadedGuestContexts = 1
	return true
}

func (jos *Jea9Linux) nextRunnableByPolicyAfterCurrent() (uint64, bool) {
	jos.refreshChaosWindow()
	if len(jos.contextOrder) == 0 {
		return 0, false
	}
	start := 0
	for i, tid := range jos.contextOrder {
		if tid == jos.currentTID {
			start = i
			break
		}
	}
	for step := 1; step <= len(jos.contextOrder); step++ {
		tid := jos.contextOrder[(start+step)%len(jos.contextOrder)]
		if tid == jos.currentTID {
			continue
		}
		ctx := jos.contexts[tid]
		if ctx == nil || ctx.state != jea9LinuxContextRunnable {
			continue
		}
		if jos.chaosActive && ctx.schedPriority == jea9LinuxSchedLow {
			continue
		}
		return tid, true
	}
	return 0, false
}

func (jos *Jea9Linux) firstRunnableByPolicy() (uint64, bool) {
	jos.refreshChaosWindow()
	for _, tid := range jos.contextOrder {
		ctx := jos.contexts[tid]
		if ctx == nil || ctx.state != jea9LinuxContextRunnable {
			continue
		}
		if jos.chaosActive && ctx.schedPriority == jea9LinuxSchedLow {
			continue
		}
		return tid, true
	}
	return 0, false
}

func (jos *Jea9Linux) refreshChaosWindow() {
	if !jos.chaosActive || jos.monotonicNS < jos.chaosUntilNS {
		return
	}
	if jos.chaosUntilNS > jos.chaosStartNS {
		jos.chaosBlockedNS += jos.chaosUntilNS - jos.chaosStartNS
	}
	jos.chaosActive = false
	jos.chaosStartNS = 0
	jos.chaosUntilNS = 0
}

func (jos *Jea9Linux) remainingChaosBudgetNS() int64 {
	if jos.monotonicNS <= 0 {
		return -jos.chaosBlockedNS
	}
	denom := jos.schedulerConfig.ChaosBudgetDenominator
	if denom == 0 {
		denom = 5
	}
	numer := jos.schedulerConfig.ChaosBudgetNumerator
	if numer == 0 {
		numer = 1
	}
	allowed := int64((uint64(jos.monotonicNS) * numer) / denom)
	return allowed - jos.chaosBlockedNS
}

func (jos *Jea9Linux) startChaosWindow(cpu *CPU, durationNS int64) bool {
	if durationNS <= 0 || jos.schedulerConfig.Mode != Jea9SchedulerChaos {
		return false
	}
	jos.refreshChaosWindow()
	if jos.chaosActive {
		return false
	}
	remaining := jos.remainingChaosBudgetNS()
	if remaining <= 0 {
		return false
	}
	if durationNS > remaining {
		durationNS = remaining
	}
	jos.nextSchedulerEvent(cpu, "chaos-window")
	jos.chaosActive = true
	jos.chaosStartNS = jos.monotonicNS
	jos.chaosUntilNS = jos.monotonicNS + durationNS
	jos.clockPolicy = ClockPolicyFixed
	jos.clockFixedAdvanceNS = durationNS
	return true
}

func (jos *Jea9Linux) maybeStartChaosWindow(cpu *CPU) bool {
	jos.refreshChaosWindow()
	if jos.schedulerConfig.Mode != Jea9SchedulerChaos || jos.chaosActive {
		return false
	}
	denom := jos.schedulerConfig.ChaosWindowProbDenominator
	if denom == 0 {
		denom = 100
	}
	numer := jos.schedulerConfig.ChaosWindowProbNumerator
	if numer == 0 || numer > denom {
		return false
	}
	if jos.schedN(cpu, denom, "scheduler-chaos-window-prob") >= numer {
		return false
	}
	maxNS := jos.schedulerConfig.ChaosWindowMaxNS
	if maxNS <= 0 {
		maxNS = defaultJea9LinuxChaosWindowMaxNS
	}
	durationNS := int64(1 + jos.schedN(cpu, uint64(maxNS), "scheduler-chaos-window-duration"))
	return jos.startChaosWindow(cpu, durationNS)
}

func (jos *Jea9Linux) hasRunnableContext() bool {
	for _, ctx := range jos.contexts {
		if ctx.state == jea9LinuxContextRunnable {
			return true
		}
	}
	return false
}

func (jos *Jea9Linux) nextWaitDeadline() (int64, bool) {
	var deadline int64
	var ok bool
	for _, ctx := range jos.contexts {
		if ctx == nil || ctx.state != jea9LinuxContextWaiting || !ctx.waitHasDeadline {
			continue
		}
		if !ok || ctx.waitDeadlineNS < deadline {
			deadline = ctx.waitDeadlineNS
			ok = true
		}
	}
	return deadline, ok
}

func (jos *Jea9Linux) advanceIdleClockToNextDeadline() {
	if jos.clockMode != Jea9ClockIdleJump {
		return
	}
	jos.advanceVirtualClockForSchedulerEvent(nil, "idle-jump")
}

func (jos *Jea9Linux) advanceVirtualClockForSchedulerEvent(cpu *CPU, reason string) (int64, bool) {
	before := jos.monotonicNS
	switch jos.clockPolicy {
	case ClockPolicyOnlyDeadlockAdvances:
		if jos.hasRunnableContext() {
			return 0, false
		}
		deadline, ok := jos.nextWaitDeadline()
		if !ok {
			return 0, false
		}
		if deadline > jos.monotonicNS {
			jos.monotonicNS = deadline
		}
	case ClockPolicyPRNG:
		minNS := jos.clockPRNGMinNS
		maxNS := jos.clockPRNGMaxNS
		if minNS <= 0 {
			minNS = defaultJea9LinuxClockPRNGMinNS
		}
		if maxNS < minNS {
			maxNS = minNS
		}
		span := uint64(maxNS - minNS + 1)
		jos.monotonicNS += minNS + int64(jos.schedN(cpu, span, reason+":clock-prng"))
	case ClockPolicyFixed:
		jos.monotonicNS += jos.clockFixedAdvanceNS
	default:
		return 0, false
	}
	jos.refreshBlocked()
	jos.noteClockAdvance(reason, before)
	return jos.monotonicNS - before, true
}

func (jos *Jea9Linux) advanceVirtualClockWhenNoRunnableCandidate(cpu *CPU, reason string) (int64, bool) {
	before := jos.monotonicNS
	switch jos.clockPolicy {
	case ClockPolicyOnlyDeadlockAdvances:
		deadline, ok := jos.nextWaitDeadline()
		if jos.chaosActive && jos.chaosUntilNS > jos.monotonicNS && (!ok || jos.chaosUntilNS < deadline) {
			deadline = jos.chaosUntilNS
			ok = true
		}
		if !ok {
			return 0, false
		}
		delta, _ := jos.advanceVirtualClockTowardDeadline(cpu, reason, deadline)
		if delta == 0 {
			return 0, false
		}
	case ClockPolicyPRNG, ClockPolicyFixed:
		deadline, ok := jos.nextWaitDeadline()
		if jos.chaosActive && jos.chaosUntilNS > jos.monotonicNS && (!ok || jos.chaosUntilNS < deadline) {
			deadline = jos.chaosUntilNS
			ok = true
		}
		if !ok {
			return jos.advanceVirtualClockForSchedulerEvent(cpu, reason)
		}
		delta, _ := jos.advanceVirtualClockTowardDeadline(cpu, reason, deadline)
		if delta == 0 {
			return 0, false
		}
	default:
		return 0, false
	}
	jos.noteClockAdvance(reason, before)
	return jos.monotonicNS - before, true
}

func (jos *Jea9Linux) advanceVirtualClockTowardDeadline(cpu *CPU, reason string, deadline int64) (int64, bool) {
	before := jos.monotonicNS
	if deadline <= before {
		return 0, true
	}
	target := deadline
	if jos.chaosActive && jos.chaosUntilNS > before && jos.chaosUntilNS < target {
		target = jos.chaosUntilNS
	}
	switch jos.clockPolicy {
	case ClockPolicyOnlyDeadlockAdvances:
		jos.monotonicNS = target
	case ClockPolicyPRNG:
		minNS := jos.clockPRNGMinNS
		maxNS := jos.clockPRNGMaxNS
		if minNS <= 0 {
			minNS = defaultJea9LinuxClockPRNGMinNS
		}
		if maxNS < minNS {
			maxNS = minNS
		}
		span := uint64(maxNS - minNS + 1)
		delta := minNS + int64(jos.schedN(cpu, span, reason+":clock-prng"))
		if delta <= 0 {
			return 0, false
		}
		if delta > target-before {
			delta = target - before
		}
		jos.monotonicNS += delta
	case ClockPolicyFixed:
		delta := jos.clockFixedAdvanceNS
		if delta <= 0 {
			return 0, false
		}
		if delta > target-before {
			delta = target - before
		}
		jos.monotonicNS += delta
	default:
		return 0, false
	}
	jos.refreshChaosWindow()
	jos.refreshBlocked()
	return jos.monotonicNS - before, jos.monotonicNS >= deadline
}

func (jos *Jea9Linux) scheduleAfterCurrentBlocked(cpu *CPU) NoteDisposition {
	if next, ok := jos.nextRunnableByPolicyAfterCurrent(); ok {
		jos.loadContext(cpu, next)
		jos.blocked = false
		jos.blockedHasDeadline = false
		return NoteHandled
	}
	if _, ok := jos.nextWaitDeadline(); ok {
		jos.advanceVirtualClockWhenNoRunnableCandidate(cpu, "blocked")
		if next, ok := jos.firstRunnableByPolicy(); ok {
			jos.loadContext(cpu, next)
			jos.blocked = false
			jos.blockedHasDeadline = false
			return NoteHandled
		}
	}
	jos.blocked = true
	if deadline, ok := jos.nextWaitDeadline(); ok {
		jos.blockedUntil = deadline
		jos.blockedHasDeadline = true
	} else {
		jos.blockedUntil = 0
		jos.blockedHasDeadline = false
	}
	return NoteExit
}

func (jos *Jea9Linux) markRunnable(tid uint64, retval int64) {
	ctx := jos.contexts[tid]
	if ctx == nil || ctx.state == jea9LinuxContextExited {
		return
	}
	ctx.state = jea9LinuxContextRunnable
	ctx.snapshot.x[10] = uint64(retval)
	jos.completeContextEcallTrap(ctx)
	jos.clearContextWaitFields(ctx)
}

func (jos *Jea9Linux) removeFutexWaiter(addr, tid uint64) {
	waiters := jos.futexWaiters[addr]
	for i, waiter := range waiters {
		if waiter == tid {
			copy(waiters[i:], waiters[i+1:])
			waiters = waiters[:len(waiters)-1]
			break
		}
	}
	if len(waiters) == 0 {
		delete(jos.futexWaiters, addr)
		return
	}
	jos.futexWaiters[addr] = waiters
}

func (vm *jea9LinuxVM) CheckGuestAccess(addr, width uint64, kind FaultKind, size uint64) *MemFault {
	if width == 0 {
		return nil
	}
	end := addr + width
	if end < addr || end > size {
		return &MemFault{Addr: addr, Width: width, Kind: kind}
	}
	startPage := addr / GuestPageSize
	endPage := (end - 1) / GuestPageSize
	for page := startPage; page <= endPage; page++ {
		prot := jea9LinuxProtRead | jea9LinuxProtWrite | jea9LinuxProtExec
		if page == 0 {
			prot = 0
		} else if p, ok := vm.pages[page]; ok {
			if p&jea9LinuxPageMapped == 0 {
				prot = 0
			} else {
				prot = p & jea9LinuxProtMask
			}
		}
		if !jea9LinuxProtAllows(prot, kind) {
			return &MemFault{Addr: addr, Width: width, Kind: kind}
		}
	}
	return nil
}

func jea9LinuxProtAllows(prot uint64, kind FaultKind) bool {
	switch kind {
	case FaultStore:
		return prot&jea9LinuxProtWrite != 0
	case FaultFetch:
		return prot&jea9LinuxProtExec != 0
	default:
		return prot&jea9LinuxProtRead != 0
	}
}

func jea9LinuxAlignDown(v uint64) uint64 {
	return v &^ (GuestPageSize - 1)
}

func jea9LinuxAlignUp(v uint64) uint64 {
	return (v + GuestPageSize - 1) &^ (GuestPageSize - 1)
}

func jea9LinuxPageRange(addr, length, memSize uint64) (begin, end uint64, ok bool) {
	if length == 0 {
		return 0, 0, false
	}
	rawEnd := addr + length
	if rawEnd < addr || rawEnd > memSize {
		return 0, 0, false
	}
	begin = jea9LinuxAlignDown(addr)
	end = jea9LinuxAlignUp(rawEnd)
	if end < begin || end > memSize {
		return 0, 0, false
	}
	return begin, end, true
}

func (vm *jea9LinuxVM) mapRange(addr, length, prot uint64) {
	vm.mapRangeState(addr, length, prot, 0)
}

func (vm *jea9LinuxVM) mapRangeState(addr, length, prot, extra uint64) {
	if length == 0 {
		return
	}
	begin := jea9LinuxAlignDown(addr)
	end := jea9LinuxAlignUp(addr + length)
	for page := begin / GuestPageSize; page < end/GuestPageSize; page++ {
		vm.pages[page] = jea9LinuxPageMapped | (prot & jea9LinuxProtMask) | extra
	}
}

func (vm *jea9LinuxVM) protectRange(addr, length, prot uint64) {
	if length == 0 {
		return
	}
	begin := jea9LinuxAlignDown(addr)
	end := jea9LinuxAlignUp(addr + length)
	for page := begin / GuestPageSize; page < end/GuestPageSize; page++ {
		extra := vm.pages[page] & jea9LinuxPageNeedsZero
		if prot != 0 {
			extra = 0
		}
		vm.pages[page] = jea9LinuxPageMapped | (prot & jea9LinuxProtMask) | extra
	}
}

func (vm *jea9LinuxVM) zeroNeededRanges(mem *GuestMemory, addr, length uint64) *MemFault {
	if length == 0 {
		return nil
	}
	begin := jea9LinuxAlignDown(addr)
	end := jea9LinuxAlignUp(addr + length)
	var runStart uint64
	var runPages uint64
	flush := func() *MemFault {
		if runPages == 0 {
			return nil
		}
		f := mem.Zero(runStart*GuestPageSize, runPages*GuestPageSize)
		runPages = 0
		return f
	}
	for page := begin / GuestPageSize; page < end/GuestPageSize; page++ {
		if vm.pages[page]&jea9LinuxPageNeedsZero == 0 {
			if f := flush(); f != nil {
				return f
			}
			continue
		}
		if runPages == 0 {
			runStart = page
		}
		runPages++
		vm.pages[page] &^= jea9LinuxPageNeedsZero
	}
	return flush()
}

func (vm *jea9LinuxVM) unmapRange(addr, length uint64) {
	if length == 0 {
		return
	}
	begin := jea9LinuxAlignDown(addr)
	end := jea9LinuxAlignUp(addr + length)
	for page := begin / GuestPageSize; page < end/GuestPageSize; page++ {
		vm.pages[page] = 0
	}
}

func (vm *jea9LinuxVM) rangeUnmapped(addr, length uint64) bool {
	if length == 0 {
		return false
	}
	begin := jea9LinuxAlignDown(addr)
	end := jea9LinuxAlignUp(addr + length)
	for page := begin / GuestPageSize; page < end/GuestPageSize; page++ {
		if page == 0 {
			return true
		}
		if state, ok := vm.pages[page]; ok && state&jea9LinuxPageMapped == 0 {
			return true
		}
	}
	return false
}

func (vm *jea9LinuxVM) rangeFree(addr, length uint64) bool {
	begin := jea9LinuxAlignDown(addr)
	end := jea9LinuxAlignUp(addr + length)
	for page := begin / GuestPageSize; page < end/GuestPageSize; page++ {
		if state, ok := vm.pages[page]; ok && state&jea9LinuxPageMapped != 0 {
			return false
		}
	}
	return true
}

func (vm *jea9LinuxVM) allocRange(memSize, length uint64) (uint64, bool) {
	limit := jea9LinuxAlignDown(memSize - 2*GuestPageSize)
	addr := jea9LinuxAlignUp(vm.mmapNext)
	for addr+length >= addr && addr+length <= limit {
		if addr >= GuestPageSize && vm.rangeFree(addr, length) {
			vm.mmapNext = addr + length
			return addr, true
		}
		addr += GuestPageSize
	}
	return 0, false
}

func (vm *jea9LinuxVM) updateExecMetadata(mem *GuestMemory, addr, length, prot uint64) {
	begin := jea9LinuxAlignDown(addr)
	end := jea9LinuxAlignUp(addr + length)
	if prot&jea9LinuxProtExec != 0 {
		mem.AddExecRegion(begin, end, prot&jea9LinuxProtWrite != 0)
		return
	}
	mem.RemoveExecRegion(begin, end)
}

func newJea9LinuxStackBuilder(cpu *CPU, stackTop uint64) jea9LinuxStackBuilder {
	if stackTop == 0 {
		stackTop = cpu.mem.Size() - Size1MB
	}
	return jea9LinuxStackBuilder{cpu: cpu, sp: stackTop &^ 15}
}

func (b *jea9LinuxStackBuilder) pushBytes(data []byte) (uint64, error) {
	if uint64(len(data)) > b.sp {
		return 0, errors.New("jea9linux: initial stack overflow")
	}
	b.sp -= uint64(len(data))
	if f := b.cpu.mem.WriteBytes(b.sp, data); f != nil {
		return 0, f
	}
	return b.sp, nil
}

func (b *jea9LinuxStackBuilder) pushString(s string) (uint64, error) {
	return b.pushBytes(append([]byte(s), 0))
}

func (b *jea9LinuxStackBuilder) pushStrings(strings []string) ([]uint64, error) {
	ptrs := make([]uint64, len(strings))
	for i := len(strings) - 1; i >= 0; i-- {
		p, err := b.pushString(strings[i])
		if err != nil {
			return nil, err
		}
		ptrs[i] = p
	}
	return ptrs, nil
}

func (b *jea9LinuxStackBuilder) writeInitialVector(argPtrs, envPtrs []uint64, aux []jea9LinuxAuxEntry) (uint64, error) {
	words := 1 + len(argPtrs) + 1 + len(envPtrs) + 1 + len(aux)*2 + 2
	bytes := uint64(words * 8)
	if bytes > b.sp {
		return 0, errors.New("jea9linux: initial stack overflow")
	}
	vector := (b.sp - bytes) &^ 15
	w := vector
	store := func(v uint64) error {
		if f := b.cpu.mem.Store64(w, v); f != nil {
			return f
		}
		w += 8
		return nil
	}
	if err := store(uint64(len(argPtrs))); err != nil {
		return 0, err
	}
	for _, p := range argPtrs {
		if err := store(p); err != nil {
			return 0, err
		}
	}
	if err := store(0); err != nil {
		return 0, err
	}
	for _, p := range envPtrs {
		if err := store(p); err != nil {
			return 0, err
		}
	}
	if err := store(0); err != nil {
		return 0, err
	}
	for _, entry := range aux {
		if err := store(entry.tag); err != nil {
			return 0, err
		}
		if err := store(entry.val); err != nil {
			return 0, err
		}
	}
	if err := store(jea9LinuxATNull); err != nil {
		return 0, err
	}
	if err := store(0); err != nil {
		return 0, err
	}
	return vector, nil
}

func (jos *Jea9Linux) InitELFStack(cpu *CPU, ef *ELF, opts Jea9LinuxStartOptions) error {
	if ef == nil || ef.Header == nil {
		return errors.New("jea9linux: InitELFStack requires a loaded ELF with header")
	}
	vm := jos.ensureVM(cpu)
	if brk := elfProgramBreak(ef); brk > vm.brk {
		vm.brk = brk
		vm.minBrk = brk
	}
	args := append([]string(nil), opts.Args...)
	if len(args) == 0 {
		args = []string{""}
	}
	env := append([]string(nil), opts.Env...)
	execPath := opts.ExecPath
	if execPath == "" {
		execPath = args[0]
	}

	stackTop := opts.StackTop
	if stackTop == 0 {
		stackTop = cpu.mem.Size() - Size1MB
	}
	stackTop &^= 15
	stack := newJea9LinuxStackBuilder(cpu, stackTop)
	argPtrs, err := stack.pushStrings(args)
	if err != nil {
		return err
	}
	envPtrs, err := stack.pushStrings(env)
	if err != nil {
		return err
	}
	execfnPtr, err := stack.pushString(execPath)
	if err != nil {
		return err
	}
	platformPtr, err := stack.pushString(jea9LinuxPlatform)
	if err != nil {
		return err
	}
	auxRandom := jos.randomBlock("auxv-random-v1", 0)
	randomPtr, err := stack.pushBytes(auxRandom[:16])
	if err != nil {
		return err
	}

	aux := buildJea9LinuxAuxv(ef, randomPtr, platformPtr, execfnPtr)
	vector, err := stack.writeInitialVector(argPtrs, envPtrs, aux)
	if err != nil {
		return err
	}

	cpu.SetReg(2, vector)
	cpu.SetPC(ef.Entry)
	jos.reserveInitialStackMapping(cpu, vm, stackTop, vector)
	return nil
}

func (jos *Jea9Linux) reserveInitialStackMapping(cpu *CPU, vm *jea9LinuxVM, stackTop, vector uint64) {
	top := jea9LinuxAlignUp(stackTop)
	if top > cpu.mem.Size() {
		top = cpu.mem.Size()
	}
	bottom := uint64(GuestPageSize)
	if stackTop > defaultJea9LinuxStackReserve {
		bottom = stackTop - defaultJea9LinuxStackReserve
	}
	if vector < bottom {
		bottom = vector
	}
	bottom = jea9LinuxAlignDown(bottom)
	if bottom < GuestPageSize {
		bottom = GuestPageSize
	}
	if top <= bottom {
		return
	}
	vm.mapRange(bottom, top-bottom, jea9LinuxProtRead|jea9LinuxProtWrite)
}

func buildJea9LinuxAuxv(ef *ELF, randomPtr, platformPtr, execfnPtr uint64) []jea9LinuxAuxEntry {
	aux := []jea9LinuxAuxEntry{
		{jea9LinuxATPAGESZ, GuestPageSize},
		{jea9LinuxATPHENT, uint64(ef.Header.PhEntSize)},
		{jea9LinuxATPHNUM, uint64(ef.Header.PhNum)},
		{jea9LinuxATENTRY, ef.Entry},
		{jea9LinuxATUID, 0},
		{jea9LinuxATEUID, 0},
		{jea9LinuxATGID, 0},
		{jea9LinuxATEGID, 0},
		{jea9LinuxATPLAT, platformPtr},
		{jea9LinuxATHWCAP, 0},
		{jea9LinuxATCLKTCK, jea9LinuxClockTicksPerSecond},
		{jea9LinuxATSECURE, 0},
		{jea9LinuxATRANDOM, randomPtr},
		{jea9LinuxATHWCAP2, 0},
		{jea9LinuxATEXECFN, execfnPtr},
	}
	if phdr := elfProgramHeaderVA(ef); phdr != 0 {
		aux = append(aux, jea9LinuxAuxEntry{jea9LinuxATPHDR, phdr})
	}
	return aux
}

func elfProgramHeaderVA(ef *ELF) uint64 {
	if ef == nil || ef.Header == nil || ef.Data == nil {
		return 0
	}
	tableOff := ef.Header.PhOff
	tableEnd := tableOff + uint64(ef.Header.PhEntSize)*uint64(ef.Header.PhNum)
	for i := 0; i < int(ef.Header.PhNum); i++ {
		off := int(ef.Header.PhOff) + i*int(ef.Header.PhEntSize)
		if off+56 > len(ef.Data) {
			return 0
		}
		var ph Elf64Phdr
		if err := binary.Read(&byteReader{data: ef.Data[off:]}, binary.LittleEndian, &ph); err != nil {
			return 0
		}
		if ph.Type != ptLoad {
			continue
		}
		if tableOff >= ph.Offset && tableEnd <= ph.Offset+ph.FileSz {
			return ph.VAddr + (tableOff - ph.Offset)
		}
	}
	return 0
}

func elfProgramBreak(ef *ELF) uint64 {
	if ef == nil || ef.Header == nil || ef.Data == nil {
		return 0
	}
	var maxEnd uint64
	for i := 0; i < int(ef.Header.PhNum); i++ {
		off := int(ef.Header.PhOff) + i*int(ef.Header.PhEntSize)
		if off+56 > len(ef.Data) {
			return 0
		}
		var ph Elf64Phdr
		if err := binary.Read(&byteReader{data: ef.Data[off:]}, binary.LittleEndian, &ph); err != nil {
			return 0
		}
		if ph.Type != ptLoad {
			continue
		}
		end := ph.VAddr + ph.MemSz
		if end > maxEnd {
			maxEnd = end
		}
	}
	return jea9LinuxAlignUp(maxEnd)
}

func (jos *Jea9Linux) Run(cpu *CPU) error {
	budget := jos.instructionBudget
	var used uint64
	for {
		attemptRemaining := budget
		if budget != 0 {
			if used >= budget {
				return jos.expireBudget(cpu)
			}
			attemptRemaining = budget - used
		}
		var retiredRemaining uint64
		if schedRemaining, ok := jos.schedulerRemainingRetired(cpu); ok {
			if schedRemaining == 0 {
				return jos.expireSchedulerQuantum(cpu)
			}
			retiredRemaining = schedRemaining
		}

		attemptsBefore := cpu.RiscvInstrBegun()
		res, limit, err := RunDefaultDualBudget(cpu, &cpu.Notes, attemptRemaining, retiredRemaining)
		attemptDelta := cpu.RiscvInstrBegun() - attemptsBefore
		used += attemptDelta
		jos.accountInsAttempts(attemptDelta)
		if jos.Blocked() {
			return ErrJea9LinuxBlocked
		}
		if err != nil {
			return err
		}
		switch res {
		case RunBudgetExpired:
			if limit == RunBudgetLimitRetired {
				return jos.expireSchedulerQuantum(cpu)
			}
			if limit == RunBudgetLimitAttempt || (budget != 0 && used >= budget) {
				return jos.expireBudget(cpu)
			}
			if attemptDelta == 0 {
				return jos.expireBudget(cpu)
			}
			continue
		case RunBudgetExit:
			return nil
		default:
			return nil
		}
	}
}

func (jos *Jea9Linux) RunJIT(cpu *CPU, jit *JIT) error {
	if jit == nil {
		return errors.New("jea9linux: RunJIT requires a JIT")
	}
	budget := jos.instructionBudget
	var used uint64
	for {
		attemptRemaining := budget
		if budget != 0 {
			if used >= budget {
				return jos.expireBudget(cpu)
			}
			attemptRemaining = budget - used
		}
		var retiredRemaining uint64
		if schedRemaining, ok := jos.schedulerRemainingRetired(cpu); ok {
			if schedRemaining == 0 {
				return jos.expireSchedulerQuantum(cpu)
			}
			retiredRemaining = schedRemaining
		}

		attemptsBefore := cpu.RiscvInstrBegun()
		res, limit, err := jit.StepBlockDualBudget(cpu, attemptRemaining, retiredRemaining)
		attemptDelta := cpu.RiscvInstrBegun() - attemptsBefore
		used += attemptDelta
		jos.accountInsAttempts(attemptDelta)
		if jos.Blocked() {
			return ErrJea9LinuxBlocked
		}
		if cpu.watchAddr != 0 {
			if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
				return &ExitError{Code: tohostExitCode(v)}
			}
		}

		if err != nil {
			if _, ok := err.(*ExitError); ok {
				return err
			}
			n := noteFromCPUError(cpu, err)
			var disp NoteDisposition
			disp = cpu.Notes.Deliver(cpu, n)
			switch disp {
			case NoteHandled:
				if jos.Blocked() {
					return ErrJea9LinuxBlocked
				}
				continue
			case NoteExit:
				return &ExitError{Code: cpu.ExitCode}
			default:
				return err
			}
		}

		switch res {
		case RunBudgetExpired:
			if limit == RunBudgetLimitRetired {
				return jos.expireSchedulerQuantum(cpu)
			}
			if limit == RunBudgetLimitAttempt || (budget != 0 && used >= budget) {
				return jos.expireBudget(cpu)
			}
			if attemptDelta == 0 {
				return jos.expireBudget(cpu)
			}
			continue
		case RunBudgetExit:
			return nil
		}
	}
}

func (jos *Jea9Linux) expireSchedulerQuantum(cpu *CPU) error {
	jos.clearLastClockAdvance()
	from := jos.traceTID()
	to := from
	ctx := jos.ensureScheduler(cpu)
	ctx.state = jea9LinuxContextRunnable
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	fromPC := ctx.snapshot.pc
	jos.nextSchedulerEvent(cpu, "quantum")
	jos.maybeReshuffleSchedulerPriorities(cpu)
	jos.maybeStartChaosWindow(cpu)
	if next, ok := jos.nextRunnableByPolicyAfterCurrent(); ok {
		jos.loadContext(cpu, next)
		to = next
	} else if _, advanced := jos.advanceVirtualClockWhenNoRunnableCandidate(cpu, "quantum"); advanced {
		if next, ok := jos.nextRunnableByPolicyAfterCurrent(); ok {
			jos.loadContext(cpu, next)
			to = next
		}
	}
	jos.installNextSchedulerQuantum(cpu)
	jos.recordScheduleTracePC(cpu, "quantum", from, to, fromPC, cpu.PC())
	return ErrJea9LinuxBudget
}

func (jos *Jea9Linux) expireBudget(cpu *CPU) error {
	jos.budgetYields++
	if jos.contexts != nil {
		ctx := jos.ensureScheduler(cpu)
		ctx.state = jea9LinuxContextRunnable
		ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	}
	return ErrJea9LinuxBudget
}

func (jos *Jea9Linux) accountInsAttempts(attempts uint64) {
	if jos.clockMode != Jea9ClockICTick || attempts == 0 {
		return
	}
	jos.monotonicNS += int64(attempts) * jos.nsPerInstruction
}

func (jos *Jea9Linux) Handle(cpu *CPU, n Note) (disp NoteDisposition) {
	if !IsEcall(n) {
		if jea9LinuxNoteIsFault(n) {
			return jos.handleFaultSignal(cpu, n)
		}
		return NoteForward
	}
	trap := jea9LinuxEcallTrapFromNote(n)
	resumeEcall := true
	startTID := jos.currentTID
	if startTID == 0 {
		startTID = jos.tid
		if startTID == 0 {
			startTID = jos.pid
		}
	}
	prevTrap := jos.activeEcallTrap
	if trap.active {
		jos.activeEcallTrap = trap
		defer func() {
			jos.finishHandledEcall(cpu, startTID, trap, disp, resumeEcall)
			jos.activeEcallTrap = prevTrap
		}()
	}
	args := SyscallArgs{
		Num: cpu.Reg(17),
		A0:  cpu.Reg(10),
		A1:  cpu.Reg(11),
		A2:  cpu.Reg(12),
		A3:  cpu.Reg(13),
		A4:  cpu.Reg(14),
		A5:  cpu.Reg(15),
	}
	jos.syscallCount++
	if args.Num < uint64(len(jos.syscallCounts)) {
		jos.syscallCounts[args.Num]++
	} else {
		jos.syscallCountOutside++
	}
	if jos.syscallPCCounts == nil {
		jos.syscallPCCounts = make(map[uint64]uint64)
	}
	jos.syscallPCCounts[n.PC]++
	if jos.traceEnabled {
		defer func() {
			jos.recordSyscallTrace(cpu, n, args, disp)
		}()
	}
	switch args.Num {
	case jea9LinuxSysEventfd2:
		cpu.SetReg(10, uint64(jos.sysEventfd2(args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysEpollCreate1:
		cpu.SetReg(10, uint64(jos.sysEpollCreate1(args.A0)))
		return NoteHandled
	case jea9LinuxSysEpollCtl:
		cpu.SetReg(10, uint64(jos.sysEpollCtl(cpu, args.A0, args.A1, args.A2, args.A3)))
		return NoteHandled
	case jea9LinuxSysEpollPwait:
		return jos.sysEpollPwait(cpu, args.A0, args.A1, args.A2, args.A3, args.A4, args.A5)
	case jea9LinuxSysFcntl:
		cpu.SetReg(10, uint64(jos.sysFcntl(args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysIoctl:
		cpu.SetReg(10, uint64(jos.sysIoctl(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysOpenat:
		cpu.SetReg(10, uint64(jos.sysOpenat(cpu, args.A0, args.A1, args.A2, args.A3)))
		return NoteHandled
	case jea9LinuxSysClose:
		cpu.SetReg(10, uint64(jos.sysClose(args.A0)))
		return NoteHandled
	case jea9LinuxSysPipe2:
		cpu.SetReg(10, uint64(jos.sysPipe2(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysLseek:
		cpu.SetReg(10, uint64(jos.sysLseek(args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysRead:
		cpu.SetReg(10, uint64(jos.sysRead(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysWrite:
		cpu.SetReg(10, uint64(jos.sysWrite(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysPread64:
		cpu.SetReg(10, uint64(jos.sysPread64(cpu, args.A0, args.A1, args.A2, args.A3)))
		return NoteHandled
	case jea9LinuxSysPselect6:
		return jos.sysPselect6(cpu, args.A0, args.A1, args.A2, args.A3, args.A4, args.A5)
	case jea9LinuxSysSetTidAddress:
		cpu.SetReg(10, uint64(jos.sysSetTidAddress(cpu, args.A0)))
		return NoteHandled
	case jea9LinuxSysFutex, jea9LinuxSysFutexTime64:
		return jos.sysFutex(cpu, args.A0, args.A1, args.A2, args.A3, args.A5)
	case jea9LinuxSysSetRobustList:
		cpu.SetReg(10, uint64(jos.sysSetRobustList(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysSetitimer, jea9LinuxSysTimerCreate, jea9LinuxSysTimerSettime, jea9LinuxSysTimerDelete:
		cpu.SetReg(10, uint64(jos.sysTimerCompatibility(args.Num)))
		return NoteHandled
	case jea9LinuxSysKill:
		return jos.sysKill(cpu, args.A0, args.A1)
	case jea9LinuxSysTkill:
		return jos.sysTkill(cpu, args.A0, args.A1)
	case jea9LinuxSysTgkill:
		return jos.sysTgkill(cpu, args.A0, args.A1, args.A2)
	case jea9LinuxSysSigaltstack:
		cpu.SetReg(10, uint64(jos.sysSigaltstack(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysRtSigaction:
		cpu.SetReg(10, uint64(jos.sysRtSigaction(cpu, args.A0, args.A1, args.A2, args.A3)))
		return NoteHandled
	case jea9LinuxSysRtSigprocmask:
		return jos.sysRtSigprocmask(cpu, args.A0, args.A1, args.A2, args.A3)
	case jea9LinuxSysRtSigreturn:
		resumeEcall = false
		return jos.sysRtSigreturn(cpu)
	case jea9LinuxSysExit, jea9LinuxSysExitGroup:
		return jos.sysExit(cpu, args.A0, args.Num == jea9LinuxSysExitGroup)
	case jea9LinuxSysClockGettime:
		cpu.SetReg(10, uint64(jos.sysClockGettime(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysSchedGetAffinity:
		cpu.SetReg(10, uint64(jos.sysSchedGetAffinity(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysSchedYield:
		return jos.sysSchedYield(cpu)
	case jea9LinuxSysUname:
		cpu.SetReg(10, uint64(jos.sysUname(cpu, args.A0)))
		return NoteHandled
	case jea9LinuxSysGetrlimit:
		cpu.SetReg(10, uint64(jos.sysGetrlimit(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysPrctl:
		cpu.SetReg(10, uint64(jos.sysPrctl(cpu, args.A0, args.A1, args.A2, args.A3, args.A4)))
		return NoteHandled
	case jea9LinuxSysGettimeofday:
		cpu.SetReg(10, uint64(jos.sysGettimeofday(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysGetpid:
		cpu.SetReg(10, jos.pid)
		return NoteHandled
	case jea9LinuxSysGettid:
		jos.ensureScheduler(cpu)
		cpu.SetReg(10, jos.tid)
		return NoteHandled
	case jea9LinuxSysSysinfo:
		cpu.SetReg(10, uint64(jos.sysSysinfo(cpu, args.A0)))
		return NoteHandled
	case jea9LinuxSysBrk:
		cpu.SetReg(10, jos.sysBrk(cpu, args.A0))
		return NoteHandled
	case jea9LinuxSysClone:
		cpu.SetReg(10, uint64(jos.sysClone(cpu, args.A0, args.A1, args.A2, args.A3, args.A4)))
		return NoteHandled
	case jea9LinuxSysMunmap:
		cpu.SetReg(10, uint64(jos.sysMunmap(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysMmap:
		cpu.SetReg(10, uint64(jos.sysMmap(cpu, args.A0, args.A1, args.A2, args.A3, args.A4, args.A5)))
		return NoteHandled
	case jea9LinuxSysMprotect:
		cpu.SetReg(10, uint64(jos.sysMprotect(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysMincore:
		cpu.SetReg(10, uint64(jos.sysMincore(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysMadvise:
		cpu.SetReg(10, uint64(jos.sysMadvise(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysRiscvHwprobe:
		cpu.SetReg(10, uint64(jos.sysRiscvHwprobe(args.A0, args.A1, args.A2, args.A3, args.A4, args.A5)))
		return NoteHandled
	case jea9LinuxSysPrlimit64:
		cpu.SetReg(10, uint64(jos.sysPrlimit64(cpu, args.A0, args.A1, args.A2, args.A3)))
		return NoteHandled
	case jea9LinuxSysNanosleep:
		return jos.sysNanosleep(cpu, args.A0, args.A1)
	case jea9LinuxSysGetrandom:
		cpu.SetReg(10, uint64(jos.sysGetrandom(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	default:
		ret := jea9LinuxErrENOSYS
		cpu.SetReg(10, uint64(ret))
		return NoteHandled
	}
}

func (jos *Jea9Linux) allocFD(fd jea9LinuxFD) int {
	n := jos.nextFD
	jos.nextFD++
	jos.fds[n] = fd
	return n
}

func (jos *Jea9Linux) sysEventfd2(init, flags uint64) int64 {
	const supported = jea9LinuxEFDSemaphore | jea9LinuxFDNonblock | jea9LinuxFDCloexec
	if flags&^supported != 0 || init == ^uint64(0) {
		return jea9LinuxErrEINVAL
	}
	fd := jos.allocFD(jea9LinuxFD{
		kind:           jea9LinuxFDEventfd,
		flags:          flags,
		eventfdCounter: init,
	})
	return int64(fd)
}

func (jos *Jea9Linux) sysEpollCreate1(flags uint64) int64 {
	if flags&^jea9LinuxFDCloexec != 0 {
		return jea9LinuxErrEINVAL
	}
	fd := jos.allocFD(jea9LinuxFD{
		kind:  jea9LinuxFDEpoll,
		flags: flags,
		epoll: &jea9LinuxEpoll{
			registrations: make(map[int]jea9LinuxEpollRegistration),
		},
	})
	return int64(fd)
}

func (jos *Jea9Linux) sysEpollCtl(cpu *CPU, epfdRaw, op, fdRaw, eventAddr uint64) int64 {
	epfd := int(int64(epfdRaw))
	fd := int(int64(fdRaw))
	ep, ok := jos.fds[epfd]
	if !ok || ep.kind != jea9LinuxFDEpoll || ep.epoll == nil {
		return jea9LinuxErrEBADF
	}
	if _, ok := jos.fds[fd]; !ok {
		return jea9LinuxErrEBADF
	}
	switch op {
	case jea9LinuxEpollCtlAdd:
		if _, exists := ep.epoll.registrations[fd]; exists {
			return jea9LinuxErrEEXIST
		}
		reg, errno := loadJea9LinuxEpollEvent(cpu, eventAddr)
		if errno != 0 {
			return errno
		}
		ep.epoll.registrations[fd] = reg
		ep.epoll.order = append(ep.epoll.order, fd)
		return 0
	case jea9LinuxEpollCtlMod:
		if _, exists := ep.epoll.registrations[fd]; !exists {
			return jea9LinuxErrENOENT
		}
		reg, errno := loadJea9LinuxEpollEvent(cpu, eventAddr)
		if errno != 0 {
			return errno
		}
		ep.epoll.registrations[fd] = reg
		return 0
	case jea9LinuxEpollCtlDel:
		if _, exists := ep.epoll.registrations[fd]; !exists {
			return jea9LinuxErrENOENT
		}
		delete(ep.epoll.registrations, fd)
		for i, ordered := range ep.epoll.order {
			if ordered == fd {
				copy(ep.epoll.order[i:], ep.epoll.order[i+1:])
				ep.epoll.order = ep.epoll.order[:len(ep.epoll.order)-1]
				break
			}
		}
		return 0
	default:
		return jea9LinuxErrEINVAL
	}
}

func loadJea9LinuxEpollEvent(cpu *CPU, addr uint64) (jea9LinuxEpollRegistration, int64) {
	events, f := cpu.mem.Load32(addr)
	if f != nil {
		return jea9LinuxEpollRegistration{}, jea9LinuxErrEFAULT
	}
	var raw [8]byte
	if f := cpu.mem.ReadBytes(addr+4, raw[:]); f != nil {
		return jea9LinuxEpollRegistration{}, jea9LinuxErrEFAULT
	}
	data := binary.LittleEndian.Uint64(raw[:])
	return jea9LinuxEpollRegistration{events: events, data: data}, 0
}

func storeJea9LinuxEpollEvent(cpu *CPU, addr uint64, events uint32, data uint64) int64 {
	if f := cpu.mem.Store32(addr, events); f != nil {
		return jea9LinuxErrEFAULT
	}
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], data)
	if f := cpu.mem.WriteBytes(addr+4, raw[:]); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) sysEpollPwait(cpu *CPU, epfdRaw, eventsAddr, maxEvents, timeoutRaw, sigmask, sigsetSize uint64) NoteDisposition {
	_, _ = sigmask, sigsetSize
	ctx := jos.ensureScheduler(cpu)
	n, errno := jos.epollCollectReady(cpu, int(int64(epfdRaw)), eventsAddr, maxEvents)
	if errno != 0 {
		cpu.SetReg(10, uint64(errno))
		ctx.snapshot = snapshotJea9LinuxCPU(cpu)
		return NoteHandled
	}
	if n > 0 || int64(timeoutRaw) == 0 {
		cpu.SetReg(10, uint64(n))
		ctx.snapshot = snapshotJea9LinuxCPU(cpu)
		return NoteHandled
	}
	deadline, hasDeadline, timeoutErr := jos.epollDeadline(timeoutRaw)
	if timeoutErr != 0 {
		cpu.SetReg(10, uint64(timeoutErr))
		ctx.snapshot = snapshotJea9LinuxCPU(cpu)
		return NoteHandled
	}
	if hasDeadline {
		if _, ok := jos.nextRunnableByPolicyAfterCurrent(); !ok {
			if _, reached := jos.advanceVirtualClockTowardDeadline(cpu, "epoll-timeout", deadline); reached {
				cpu.SetReg(10, 0)
				ctx.snapshot = snapshotJea9LinuxCPU(cpu)
				return NoteHandled
			}
		}
	}
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	ctx.state = jea9LinuxContextWaiting
	jos.clearContextWaitFields(ctx)
	ctx.waitKind = jea9LinuxWaitEpoll
	ctx.waitFD = int(int64(epfdRaw))
	ctx.waitEventAddr = eventsAddr
	ctx.waitMaxEvents = maxEvents
	ctx.waitDeadlineNS = deadline
	ctx.waitHasDeadline = hasDeadline
	if hasDeadline {
		jos.timedEpollWaiters++
		jos.blockedUntil = deadline
		jos.blockedHasDeadline = true
	}
	if next, ok := jos.nextRunnableByPolicyAfterCurrent(); ok {
		jos.loadContext(cpu, next)
		jos.blocked = false
		jos.blockedHasDeadline = false
		return NoteHandled
	}
	jos.blocked = true
	return NoteExit
}

func (jos *Jea9Linux) epollDeadline(timeoutRaw uint64) (int64, bool, int64) {
	timeout := int64(timeoutRaw)
	if timeout < 0 {
		return 0, false, 0
	}
	return jos.monotonicNS + timeout*1_000_000, true, 0
}

func (jos *Jea9Linux) epollCollectReady(cpu *CPU, epfd int, eventsAddr, maxEvents uint64) (int64, int64) {
	if maxEvents == 0 {
		return 0, jea9LinuxErrEINVAL
	}
	ep, ok := jos.fds[epfd]
	if !ok || ep.kind != jea9LinuxFDEpoll || ep.epoll == nil {
		return 0, jea9LinuxErrEBADF
	}
	count := int64(0)
	for _, fd := range ep.epoll.order {
		if uint64(count) >= maxEvents {
			break
		}
		reg, ok := ep.epoll.registrations[fd]
		if !ok {
			continue
		}
		ready := jos.fdReadyEvents(fd) & reg.events
		if ready == 0 {
			continue
		}
		if errno := storeJea9LinuxEpollEvent(cpu, eventsAddr+uint64(count)*12, ready, reg.data); errno != 0 {
			return 0, errno
		}
		count++
	}
	return count, 0
}

func (jos *Jea9Linux) fdReadyEvents(fd int) uint32 {
	f, ok := jos.fds[fd]
	if !ok {
		return 0
	}
	switch f.kind {
	case jea9LinuxFDEventfd:
		events := jea9LinuxEpollOut
		if f.eventfdCounter != 0 {
			events |= jea9LinuxEpollIn
		}
		return events
	case jea9LinuxFDPipeRead:
		if f.pipe != nil && len(f.pipe.buf) > 0 {
			return jea9LinuxEpollIn
		}
	case jea9LinuxFDPipeWrite:
		if f.pipe != nil && len(f.pipe.buf) < cap(f.pipe.buf) {
			return jea9LinuxEpollOut
		}
	case jea9LinuxFDStdout, jea9LinuxFDStderr:
		return jea9LinuxEpollOut
	}
	return 0
}

func (jos *Jea9Linux) wakeEpollWaitersForFD(cpu *CPU, fd int) {
	for _, ctx := range jos.contexts {
		if ctx.state != jea9LinuxContextWaiting || ctx.waitKind != jea9LinuxWaitEpoll {
			continue
		}
		ep := jos.fds[ctx.waitFD]
		if ep.epoll == nil {
			continue
		}
		if _, ok := ep.epoll.registrations[fd]; !ok {
			continue
		}
		n, errno := jos.epollCollectReady(cpu, ctx.waitFD, ctx.waitEventAddr, ctx.waitMaxEvents)
		if errno != 0 {
			jos.markRunnable(ctx.tid, errno)
			continue
		}
		if n > 0 {
			jos.markRunnable(ctx.tid, n)
		}
	}
}

func (jos *Jea9Linux) sysPipe2(cpu *CPU, pipefdAddr, flags uint64) int64 {
	const supported = jea9LinuxFDNonblock | jea9LinuxFDCloexec
	if flags&^supported != 0 {
		return jea9LinuxErrEINVAL
	}
	pipe := &jea9LinuxPipe{buf: make([]byte, 0, 65536)}
	readFD := jos.allocFD(jea9LinuxFD{kind: jea9LinuxFDPipeRead, flags: flags, pipe: pipe})
	writeFD := jos.allocFD(jea9LinuxFD{kind: jea9LinuxFDPipeWrite, flags: flags, pipe: pipe})
	pipe.readFD = readFD
	pipe.writeFD = writeFD
	if f := cpu.mem.Store32(pipefdAddr, uint32(readFD)); f != nil {
		delete(jos.fds, readFD)
		delete(jos.fds, writeFD)
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store32(pipefdAddr+4, uint32(writeFD)); f != nil {
		delete(jos.fds, readFD)
		delete(jos.fds, writeFD)
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) sysPselect6(cpu *CPU, nfds, readfds, writefds, exceptfds, timeoutAddr, sigmask uint64) NoteDisposition {
	_, _, _, _, _ = nfds, readfds, writefds, exceptfds, sigmask
	if timeoutAddr == 0 {
		cpu.SetReg(10, 0)
		return NoteHandled
	}
	deadline, hasDeadline, errno := jos.timespecDeadline(cpu, timeoutAddr)
	if errno != 0 {
		cpu.SetReg(10, uint64(errno))
		return NoteHandled
	}
	if !hasDeadline || deadline <= jos.monotonicNS {
		cpu.SetReg(10, 0)
		return NoteHandled
	}
	ctx := jos.ensureScheduler(cpu)
	if _, ok := jos.nextRunnableByPolicyAfterCurrent(); !ok {
		if _, reached := jos.advanceVirtualClockTowardDeadline(cpu, "pselect-timeout", deadline); reached {
			cpu.SetReg(10, 0)
			ctx.snapshot = snapshotJea9LinuxCPU(cpu)
			return NoteHandled
		}
	}
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	ctx.state = jea9LinuxContextWaiting
	jos.clearContextWaitFields(ctx)
	ctx.waitKind = jea9LinuxWaitNanosleep
	ctx.waitDeadlineNS = deadline
	ctx.waitHasDeadline = true
	jos.timedNanosleepWaiters++
	return jos.scheduleAfterCurrentBlocked(cpu)
}

func (jos *Jea9Linux) timespecDeadline(cpu *CPU, timeoutAddr uint64) (int64, bool, int64) {
	secRaw, f := cpu.mem.Load64(timeoutAddr)
	if f != nil {
		return 0, false, jea9LinuxErrEFAULT
	}
	nsecRaw, f := cpu.mem.Load64(timeoutAddr + 8)
	if f != nil {
		return 0, false, jea9LinuxErrEFAULT
	}
	sec := int64(secRaw)
	nsec := int64(nsecRaw)
	if sec < 0 || nsec < 0 || nsec >= 1_000_000_000 {
		return 0, false, jea9LinuxErrEINVAL
	}
	return jos.monotonicNS + sec*1_000_000_000 + nsec, true, 0
}

func (jos *Jea9Linux) sysTimerCompatibility(num uint64) int64 {
	_ = num
	return jea9LinuxErrENOSYS
}

func (jos *Jea9Linux) sysRiscvHwprobe(pairs, pairCount, cpuCount, cpus, flags, reserved uint64) int64 {
	_, _, _, _, _, _ = pairs, pairCount, cpuCount, cpus, flags, reserved
	return jea9LinuxErrENOSYS
}

func setJea9LinuxReturn(cpu *CPU, ret int64) {
	cpu.SetReg(10, uint64(ret))
}

func jea9LinuxNoteIsFault(n Note) bool {
	if IsFault(n) {
		return true
	}
	switch n.Cause {
	case CauseInsnFault, CauseLoadFault, CauseStoreFault, CauseLoadMisalign, CauseMisalignStore:
		return true
	default:
		return false
	}
}

func jea9LinuxSignalBit(sig uint64) uint64 {
	if sig == 0 || sig > 64 {
		return 0
	}
	return uint64(1) << (sig - 1)
}

func (jos *Jea9Linux) handleFaultSignal(cpu *CPU, n Note) NoteDisposition {
	jos.ensureSignalState()
	action := jos.signalActions[jea9LinuxSIGSEGV]
	if action.handler == jea9LinuxSignalDefault || action.handler == jea9LinuxSignalIgnore {
		return NoteForward
	}
	ctx := jos.ensureScheduler(cpu)
	info := jea9LinuxSignalInfo{
		signo: jea9LinuxSIGSEGV,
		code:  jea9LinuxSignalCodeSEGVMapErr,
		pid:   jos.pid,
		addr:  n.Tval,
	}
	delivered, errno := jos.signalContext(cpu, ctx, jea9LinuxSIGSEGV, info)
	if errno != 0 || !delivered {
		return NoteForward
	}
	return NoteHandled
}

func (jos *Jea9Linux) sysRtSigaction(cpu *CPU, sig, actAddr, oldAddr, sigsetSize uint64) int64 {
	jos.ensureSignalState()
	if !jea9LinuxSignalValid(sig) || sigsetSize != 8 {
		return jea9LinuxErrEINVAL
	}
	if oldAddr != 0 {
		if errno := storeJea9LinuxSignalAction(cpu, oldAddr, jos.signalActions[sig]); errno != 0 {
			return errno
		}
	}
	if actAddr != 0 {
		action, errno := loadJea9LinuxSignalAction(cpu, actAddr)
		if errno != 0 {
			return errno
		}
		jos.signalActions[sig] = action
	}
	return 0
}

func loadJea9LinuxSignalAction(cpu *CPU, addr uint64) (jea9LinuxSignalAction, int64) {
	handler, f := cpu.mem.Load64(addr)
	if f != nil {
		return jea9LinuxSignalAction{}, jea9LinuxErrEFAULT
	}
	flags, f := cpu.mem.Load64(addr + 8)
	if f != nil {
		return jea9LinuxSignalAction{}, jea9LinuxErrEFAULT
	}
	third, f := cpu.mem.Load64(addr + 16)
	if f != nil {
		return jea9LinuxSignalAction{}, jea9LinuxErrEFAULT
	}
	fourth, f := cpu.mem.Load64(addr + 24)
	if f != nil {
		return jea9LinuxSignalAction{}, jea9LinuxErrEFAULT
	}
	restorer := third
	mask := fourth
	if flags&jea9LinuxSASiginfo != 0 && third != 0 && fourth == 0 {
		mask = third
		restorer = fourth
	}
	return jea9LinuxSignalAction{
		handler:  handler,
		flags:    flags,
		restorer: restorer,
		mask:     mask,
	}, 0
}

func storeJea9LinuxSignalAction(cpu *CPU, addr uint64, action jea9LinuxSignalAction) int64 {
	if f := cpu.mem.Store64(addr, action.handler); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+8, action.flags); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+16, action.restorer); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+24, action.mask); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) sysRtSigprocmask(cpu *CPU, how, setAddr, oldAddr, sigsetSize uint64) NoteDisposition {
	ctx := jos.ensureScheduler(cpu)
	if sigsetSize != 8 {
		setJea9LinuxReturn(cpu, jea9LinuxErrEINVAL)
		return NoteHandled
	}
	if oldAddr != 0 {
		if f := cpu.mem.Store64(oldAddr, ctx.signalMask); f != nil {
			setJea9LinuxReturn(cpu, jea9LinuxErrEFAULT)
			return NoteHandled
		}
	}
	if setAddr != 0 {
		mask, f := cpu.mem.Load64(setAddr)
		if f != nil {
			setJea9LinuxReturn(cpu, jea9LinuxErrEFAULT)
			return NoteHandled
		}
		switch how {
		case jea9LinuxSIGBlock:
			ctx.signalMask |= mask
		case jea9LinuxSIGUnblock:
			ctx.signalMask &^= mask
		case jea9LinuxSIGSetmask:
			ctx.signalMask = mask
		default:
			setJea9LinuxReturn(cpu, jea9LinuxErrEINVAL)
			return NoteHandled
		}
	}
	cpu.SetReg(10, 0)
	if delivered, errno := jos.deliverPendingSignals(cpu, ctx); errno != 0 {
		cpu.SetReg(10, uint64(errno))
	} else if delivered {
		return NoteHandled
	}
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	return NoteHandled
}

func (jos *Jea9Linux) sysSigaltstack(cpu *CPU, newAddr, oldAddr uint64) int64 {
	ctx := jos.ensureScheduler(cpu)
	if oldAddr != 0 {
		if errno := storeJea9LinuxSigaltstack(cpu, oldAddr, ctx.sigaltSP, ctx.sigaltFlags, ctx.sigaltSize); errno != 0 {
			return errno
		}
	}
	if newAddr != 0 {
		sp, flags, size, errno := loadJea9LinuxSigaltstack(cpu, newAddr)
		if errno != 0 {
			return errno
		}
		if flags&^jea9LinuxSSDisable != 0 {
			return jea9LinuxErrEINVAL
		}
		ctx.sigaltSP = sp
		ctx.sigaltFlags = flags
		ctx.sigaltSize = size
	}
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	return 0
}

func loadJea9LinuxSigaltstack(cpu *CPU, addr uint64) (sp, flags, size uint64, errno int64) {
	sp, f := cpu.mem.Load64(addr)
	if f != nil {
		return 0, 0, 0, jea9LinuxErrEFAULT
	}
	flags, f = cpu.mem.Load64(addr + 8)
	if f != nil {
		return 0, 0, 0, jea9LinuxErrEFAULT
	}
	size, f = cpu.mem.Load64(addr + 16)
	if f != nil {
		return 0, 0, 0, jea9LinuxErrEFAULT
	}
	return sp, flags, size, 0
}

func storeJea9LinuxSigaltstack(cpu *CPU, addr, sp, flags, size uint64) int64 {
	if f := cpu.mem.Store64(addr, sp); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+8, flags); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+16, size); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) sysKill(cpu *CPU, pidRaw, sig uint64) NoteDisposition {
	if sig == 0 {
		cpu.SetReg(10, 0)
		return NoteHandled
	}
	if pidRaw != jos.pid {
		setJea9LinuxReturn(cpu, jea9LinuxErrESRCH)
		return NoteHandled
	}
	return jos.signalTIDSyscall(cpu, jos.currentTID, sig)
}

func (jos *Jea9Linux) sysTkill(cpu *CPU, tid, sig uint64) NoteDisposition {
	if sig == 0 {
		cpu.SetReg(10, 0)
		return NoteHandled
	}
	return jos.signalTIDSyscall(cpu, tid, sig)
}

func (jos *Jea9Linux) sysTgkill(cpu *CPU, pidRaw, tid, sig uint64) NoteDisposition {
	if sig == 0 {
		cpu.SetReg(10, 0)
		return NoteHandled
	}
	if pidRaw != jos.pid {
		setJea9LinuxReturn(cpu, jea9LinuxErrESRCH)
		return NoteHandled
	}
	return jos.signalTIDSyscall(cpu, tid, sig)
}

func (jos *Jea9Linux) signalTIDSyscall(cpu *CPU, tid, sig uint64) NoteDisposition {
	if !jea9LinuxSignalValid(sig) {
		setJea9LinuxReturn(cpu, jea9LinuxErrEINVAL)
		return NoteHandled
	}
	ctx := jos.ensureScheduler(cpu)
	if tid != ctx.tid {
		ctx = jos.contexts[tid]
	}
	if ctx == nil || ctx.state == jea9LinuxContextExited {
		setJea9LinuxReturn(cpu, jea9LinuxErrESRCH)
		return NoteHandled
	}
	info := jea9LinuxSignalInfo{
		signo: sig,
		code:  jea9LinuxSignalCodeUser,
		pid:   jos.pid,
		uid:   0,
	}
	delivered, errno := jos.signalContext(cpu, ctx, sig, info)
	if errno != 0 {
		cpu.SetReg(10, uint64(errno))
		return NoteHandled
	}
	if delivered && ctx.tid == jos.currentTID {
		return NoteHandled
	}
	cpu.SetReg(10, 0)
	current := jos.contexts[jos.currentTID]
	if current != nil {
		current.snapshot = snapshotJea9LinuxCPU(cpu)
	}
	return NoteHandled
}

func (jos *Jea9Linux) sysRtSigreturn(cpu *CPU) NoteDisposition {
	ctx := jos.ensureScheduler(cpu)
	key, frame, ok := jos.findSignalFrame(ctx.tid, cpu.Reg(2))
	if !ok {
		setJea9LinuxReturn(cpu, jea9LinuxErrEINVAL)
		return NoteHandled
	}
	snap, mask, errno := loadJea9LinuxSignalUContext(cpu, key.sp+jea9LinuxSignalFrameUctxOff, frame.snapshot, frame.signalMask)
	if errno != 0 {
		setJea9LinuxReturn(cpu, errno)
		return NoteHandled
	}
	delete(jos.signalFrames, key)
	ctx.signalMask = mask
	restoreJea9LinuxCPU(cpu, snap)
	ctx.syscallTrap = jea9LinuxEcallTrapFrame{}
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	return NoteHandled
}

func (jos *Jea9Linux) findSignalFrame(tid, currentSP uint64) (jea9LinuxSignalFrameKey, jea9LinuxSignalFrame, bool) {
	exact := jea9LinuxSignalFrameKey{tid: tid, sp: currentSP}
	if frame, ok := jos.signalFrames[exact]; ok {
		return exact, frame, true
	}
	var bestKey jea9LinuxSignalFrameKey
	var bestFrame jea9LinuxSignalFrame
	var bestSet bool
	for key, frame := range jos.signalFrames {
		if key.tid != tid || key.sp < currentSP {
			continue
		}
		if !bestSet || key.sp < bestKey.sp {
			bestKey = key
			bestFrame = frame
			bestSet = true
		}
	}
	return bestKey, bestFrame, bestSet
}

func jea9LinuxSignalValid(sig uint64) bool {
	return sig > 0 && sig <= 64
}

func (jos *Jea9Linux) signalContext(cpu *CPU, ctx *jea9LinuxContext, sig uint64, info jea9LinuxSignalInfo) (bool, int64) {
	bit := jea9LinuxSignalBit(sig)
	if bit == 0 {
		return false, jea9LinuxErrEINVAL
	}
	if ctx.signalMask&bit != 0 {
		ctx.pendingSignals = append(ctx.pendingSignals, jea9LinuxPendingSignal{sig: sig, info: info})
		return false, 0
	}
	action := jos.signalActions[sig]
	if action.handler == jea9LinuxSignalDefault || action.handler == jea9LinuxSignalIgnore {
		return false, 0
	}
	return jos.deliverSignal(cpu, ctx, sig, info, action)
}

func (jos *Jea9Linux) deliverPendingSignals(cpu *CPU, ctx *jea9LinuxContext) (bool, int64) {
	for i := 0; i < len(ctx.pendingSignals); i++ {
		pending := ctx.pendingSignals[i]
		if ctx.signalMask&jea9LinuxSignalBit(pending.sig) != 0 {
			continue
		}
		copy(ctx.pendingSignals[i:], ctx.pendingSignals[i+1:])
		ctx.pendingSignals = ctx.pendingSignals[:len(ctx.pendingSignals)-1]
		action := jos.signalActions[pending.sig]
		if action.handler == jea9LinuxSignalDefault || action.handler == jea9LinuxSignalIgnore {
			i--
			continue
		}
		return jos.deliverSignal(cpu, ctx, pending.sig, pending.info, action)
	}
	return false, 0
}

func (jos *Jea9Linux) deliverSignal(cpu *CPU, ctx *jea9LinuxContext, sig uint64, info jea9LinuxSignalInfo, action jea9LinuxSignalAction) (bool, int64) {
	jos.ensureSignalState()
	current := ctx.tid == jos.currentTID
	snap := ctx.snapshot
	if current {
		snap = snapshotJea9LinuxCPU(cpu)
		if jos.activeEcallTrap.active && snap.pc == jos.activeEcallTrap.trapPC {
			snap.pc = jos.activeEcallTrap.resumePC
		}
	} else if ctx.syscallTrap.active {
		if snap.pc == ctx.syscallTrap.trapPC {
			snap.pc = ctx.syscallTrap.resumePC
		}
	}
	frameSP, siginfoAddr, ucontextAddr, errno := jos.writeSignalFrame(cpu, ctx, snap, info)
	if errno != 0 {
		return false, errno
	}
	jos.signalFrames[jea9LinuxSignalFrameKey{tid: ctx.tid, sp: frameSP}] = jea9LinuxSignalFrame{
		snapshot:   snap,
		signalMask: ctx.signalMask,
	}

	restorer := action.restorer
	if restorer == 0 {
		var errno int64
		restorer, errno = jos.ensureSignalRestorer(cpu)
		if errno != 0 {
			return false, errno
		}
	}
	snap.pc = action.handler
	snap.x[1] = restorer
	snap.x[2] = frameSP
	snap.x[10] = sig
	snap.x[11] = siginfoAddr
	snap.x[12] = ucontextAddr
	snap.x[0] = 0
	ctx.signalMask |= action.mask | jea9LinuxSignalBit(sig)
	jos.cancelContextWait(ctx)
	ctx.state = jea9LinuxContextRunnable
	ctx.syscallTrap = jea9LinuxEcallTrapFrame{}
	ctx.snapshot = snap
	if current {
		restoreJea9LinuxCPU(cpu, snap)
	}
	return true, 0
}

func (jos *Jea9Linux) ensureSignalRestorer(cpu *CPU) (uint64, int64) {
	if jos.signalRestorer != 0 {
		return jos.signalRestorer, 0
	}
	vm := jos.ensureVM(cpu)
	addr, ok := vm.allocRange(cpu.mem.Size(), GuestPageSize)
	if !ok {
		return 0, jea9LinuxErrENOMEM
	}
	if f := cpu.mem.Zero(addr, GuestPageSize); f != nil {
		return 0, jea9LinuxErrENOMEM
	}
	if f := cpu.mem.Store32(addr, encodeJea9LinuxADDI(17, 0, int32(jea9LinuxSysRtSigreturn))); f != nil {
		return 0, jea9LinuxErrENOMEM
	}
	if f := cpu.mem.Store32(addr+4, 0x00000073); f != nil {
		return 0, jea9LinuxErrENOMEM
	}
	vm.mapRange(addr, GuestPageSize, jea9LinuxProtRead|jea9LinuxProtExec)
	vm.updateExecMetadata(&cpu.mem, addr, GuestPageSize, jea9LinuxProtRead|jea9LinuxProtExec)
	jos.signalRestorer = addr
	return addr, 0
}

func encodeJea9LinuxADDI(rd, rs1 uint32, imm int32) uint32 {
	return ((uint32(imm) & 0xfff) << 20) |
		(rs1 << 15) |
		(rd << 7) |
		0x13
}

func (jos *Jea9Linux) writeSignalFrame(cpu *CPU, ctx *jea9LinuxContext, snap jea9LinuxCPUSnapshot, info jea9LinuxSignalInfo) (frameSP, siginfoAddr, ucontextAddr uint64, errno int64) {
	stackTop := snap.x[2]
	if ctx.sigaltFlags&jea9LinuxSSDisable == 0 &&
		ctx.sigaltSP != 0 &&
		ctx.sigaltSize >= jea9LinuxSignalFrameSize &&
		jos.signalActions[info.signo].flags&jea9LinuxSAOnstack != 0 {
		stackTop = ctx.sigaltSP + ctx.sigaltSize
	}
	if stackTop < jea9LinuxSignalFrameSize {
		stackTop = defaultJea9LinuxSignalStackTop(cpu)
	}
	if stackTop < jea9LinuxSignalFrameSize {
		return 0, 0, 0, jea9LinuxErrEFAULT
	}
	frameSP = (stackTop - jea9LinuxSignalFrameSize) &^ uint64(15)
	siginfoAddr = frameSP + jea9LinuxSignalFrameSiginfoOff
	ucontextAddr = frameSP + jea9LinuxSignalFrameUctxOff
	if f := cpu.mem.Zero(frameSP, jea9LinuxSignalFrameSize); f != nil {
		return 0, 0, 0, jea9LinuxErrEFAULT
	}
	if errno := storeJea9LinuxSignalInfo(cpu, siginfoAddr, info); errno != 0 {
		return 0, 0, 0, errno
	}
	if errno := storeJea9LinuxSignalUContext(cpu, ucontextAddr, snap, ctx.signalMask); errno != 0 {
		return 0, 0, 0, errno
	}
	return frameSP, siginfoAddr, ucontextAddr, 0
}

func defaultJea9LinuxSignalStackTop(cpu *CPU) uint64 {
	if cpu.mem.Size() <= Size1MB+GuestPageSize {
		return cpu.mem.Size()
	}
	return cpu.mem.Size() - Size1MB
}

func storeJea9LinuxSignalUContext(cpu *CPU, addr uint64, snap jea9LinuxCPUSnapshot, mask uint64) int64 {
	if f := cpu.mem.Store64(addr+jea9LinuxUContextSigmaskOff, mask); f != nil {
		return jea9LinuxErrEFAULT
	}
	regs := jea9LinuxSignalRegs(snap)
	base := addr + jea9LinuxUContextMContextOff
	for i, reg := range regs {
		if f := cpu.mem.Store64(base+uint64(i)*8, reg); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	return 0
}

func loadJea9LinuxSignalUContext(cpu *CPU, addr uint64, fallback jea9LinuxCPUSnapshot, fallbackMask uint64) (jea9LinuxCPUSnapshot, uint64, int64) {
	snap := fallback
	mask := fallbackMask
	if v, f := cpu.mem.Load64(addr + jea9LinuxUContextSigmaskOff); f == nil {
		mask = v
	} else {
		return snap, mask, jea9LinuxErrEFAULT
	}
	base := addr + jea9LinuxUContextMContextOff
	var regs [32]uint64
	for i := range regs {
		v, f := cpu.mem.Load64(base + uint64(i)*8)
		if f != nil {
			return snap, mask, jea9LinuxErrEFAULT
		}
		regs[i] = v
	}
	snap.pc = regs[0]
	copy(snap.x[1:], regs[1:])
	snap.x[0] = 0
	return snap, mask, 0
}

func jea9LinuxSignalRegs(snap jea9LinuxCPUSnapshot) [32]uint64 {
	return [32]uint64{
		snap.pc,
		snap.x[1],
		snap.x[2],
		snap.x[3],
		snap.x[4],
		snap.x[5],
		snap.x[6],
		snap.x[7],
		snap.x[8],
		snap.x[9],
		snap.x[10],
		snap.x[11],
		snap.x[12],
		snap.x[13],
		snap.x[14],
		snap.x[15],
		snap.x[16],
		snap.x[17],
		snap.x[18],
		snap.x[19],
		snap.x[20],
		snap.x[21],
		snap.x[22],
		snap.x[23],
		snap.x[24],
		snap.x[25],
		snap.x[26],
		snap.x[27],
		snap.x[28],
		snap.x[29],
		snap.x[30],
		snap.x[31],
	}
}

func storeJea9LinuxSignalInfo(cpu *CPU, addr uint64, info jea9LinuxSignalInfo) int64 {
	if f := cpu.mem.Store32(addr, uint32(info.signo)); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store32(addr+4, 0); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store32(addr+8, uint32(info.code)); f != nil {
		return jea9LinuxErrEFAULT
	}
	if info.addr != 0 {
		if f := cpu.mem.Store64(addr+16, info.addr); f != nil {
			return jea9LinuxErrEFAULT
		}
	} else {
		if f := cpu.mem.Store32(addr+16, uint32(info.pid)); f != nil {
			return jea9LinuxErrEFAULT
		}
		if f := cpu.mem.Store32(addr+20, uint32(info.uid)); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	if f := cpu.mem.Store64(addr+24, info.addr); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) cancelContextWait(ctx *jea9LinuxContext) {
	if ctx.waitKind == jea9LinuxWaitFutex {
		jos.removeFutexWaiter(ctx.waitAddr, ctx.tid)
	}
	jos.clearContextWaitFields(ctx)
}

func (jos *Jea9Linux) clearContextWaitFields(ctx *jea9LinuxContext) {
	if ctx.waitHasDeadline {
		switch ctx.waitKind {
		case jea9LinuxWaitFutex:
			if jos.timedFutexWaiters > 0 {
				jos.timedFutexWaiters--
			}
		case jea9LinuxWaitEpoll:
			if jos.timedEpollWaiters > 0 {
				jos.timedEpollWaiters--
			}
		case jea9LinuxWaitNanosleep:
			if jos.timedNanosleepWaiters > 0 {
				jos.timedNanosleepWaiters--
			}
		}
	}
	ctx.waitKind = jea9LinuxWaitNone
	ctx.waitAddr = 0
	ctx.waitDeadlineNS = 0
	ctx.waitHasDeadline = false
	ctx.waitFD = 0
	ctx.waitEventAddr = 0
	ctx.waitMaxEvents = 0
}

func (jos *Jea9Linux) sysClone(cpu *CPU, flags, childStack, parentTIDAddr, tls, childTIDAddr uint64) int64 {
	parent := jos.ensureScheduler(cpu)
	if !jea9LinuxCloneFlagsSupported(flags) {
		return jea9LinuxErrEINVAL
	}
	tid := jos.nextTID
	for tid == 0 || jos.contexts[tid] != nil {
		tid++
	}
	jos.nextTID = tid + 1

	if flags&jea9LinuxCloneParentSetTID != 0 {
		if parentTIDAddr == 0 {
			return jea9LinuxErrEFAULT
		}
		if f := cpu.mem.Store32(parentTIDAddr, uint32(tid)); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	if flags&jea9LinuxCloneChildSetTID != 0 {
		if childTIDAddr == 0 {
			return jea9LinuxErrEFAULT
		}
		if f := cpu.mem.Store32(childTIDAddr, uint32(tid)); f != nil {
			return jea9LinuxErrEFAULT
		}
	}

	childSnap := jos.syscallReturnSnapshot(cpu, 0)
	if childStack != 0 {
		childSnap.x[2] = childStack
	}
	if flags&jea9LinuxCloneSetTLS != 0 {
		childSnap.x[4] = tls
	}
	childSnap.x[0] = 0

	child := &jea9LinuxContext{
		tid:        tid,
		state:      jea9LinuxContextRunnable,
		snapshot:   childSnap,
		signalMask: parent.signalMask,
	}
	if flags&jea9LinuxCloneChildClearTID != 0 {
		child.clearChildTID = childTIDAddr
	}
	jos.contexts[tid] = child
	jos.contextOrder = append(jos.contextOrder, tid)

	cpu.SetReg(10, tid)
	parent.state = jea9LinuxContextRunnable
	parent.snapshot = jos.syscallReturnSnapshot(cpu, tid)
	return int64(tid)
}

func jea9LinuxCloneFlagsSupported(flags uint64) bool {
	const required = jea9LinuxCloneVM |
		jea9LinuxCloneFS |
		jea9LinuxCloneFiles |
		jea9LinuxCloneSighand |
		jea9LinuxCloneThread |
		jea9LinuxCloneSysvsem
	const supported = required |
		jea9LinuxCloneSetTLS |
		jea9LinuxCloneParentSetTID |
		jea9LinuxCloneChildClearTID |
		jea9LinuxCloneChildSetTID
	if flags&^supported != 0 {
		return false
	}
	return flags&required == required
}

func (jos *Jea9Linux) sysSchedYield(cpu *CPU) NoteDisposition {
	ctx := jos.ensureScheduler(cpu)
	from := jos.traceTID()
	to := from
	cpu.SetReg(10, 0)
	ctx.state = jea9LinuxContextRunnable
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	fromPC := ctx.snapshot.pc
	jos.advanceIdleClockToNextDeadline()
	if next, ok := jos.nextRunnableByPolicyAfterCurrent(); ok {
		jos.loadContext(cpu, next)
		to = next
	}
	jos.recordScheduleTracePC(cpu, "yield", from, to, fromPC, cpu.PC())
	return NoteHandled
}

func (jos *Jea9Linux) sysSchedGetAffinity(cpu *CPU, pid, cpuSetSize, maskAddr uint64) int64 {
	_ = pid
	if cpuSetSize == 0 {
		return jea9LinuxErrEINVAL
	}
	for i := uint64(0); i < cpuSetSize; i++ {
		v := uint8(0)
		if i == 0 {
			v = 1
		}
		if f := cpu.mem.Store8(maskAddr+i, v); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	return 0
}

func (jos *Jea9Linux) sysSetTidAddress(cpu *CPU, addr uint64) int64 {
	ctx := jos.ensureScheduler(cpu)
	ctx.clearChildTID = addr
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	return int64(jos.currentTID)
}

func (jos *Jea9Linux) sysSetRobustList(cpu *CPU, addr, length uint64) int64 {
	ctx := jos.ensureScheduler(cpu)
	ctx.robustList = addr
	ctx.robustListLen = length
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	return 0
}

func (jos *Jea9Linux) sysExit(cpu *CPU, code uint64, exitGroup bool) NoteDisposition {
	ctx := jos.ensureScheduler(cpu)
	if exitGroup || ctx.tid == jos.pid {
		cpu.ExitCode = int(int32(code))
		return NoteExit
	}
	jos.exitCurrentThread(cpu)
	if next, ok := jos.nextRunnableByPolicyAfterCurrent(); ok {
		jos.loadContext(cpu, next)
		return NoteHandled
	}
	cpu.ExitCode = int(int32(code))
	return NoteExit
}

func (jos *Jea9Linux) exitCurrentThread(cpu *CPU) {
	ctx := jos.ensureScheduler(cpu)
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	jos.cancelContextWait(ctx)
	ctx.state = jea9LinuxContextExited
	if ctx.clearChildTID != 0 {
		if f := cpu.mem.Store32(ctx.clearChildTID, 0); f == nil {
			jos.wakeFutex(ctx.clearChildTID, 1)
		}
	}
}

func (jos *Jea9Linux) sysFutex(cpu *CPU, addr, op, val, timeoutAddr, val3 uint64) NoteDisposition {
	_ = val3
	ctx := jos.ensureScheduler(cpu)
	cmd := op & 0xf
	switch cmd {
	case jea9LinuxFutexWait, jea9LinuxFutexWaitBitset:
		ret, blocked := jos.futexWait(cpu, ctx, addr, uint32(val), timeoutAddr)
		if blocked {
			if next, ok := jos.nextRunnableByPolicyAfterCurrent(); ok {
				jos.loadContext(cpu, next)
				jos.blocked = false
				jos.blockedHasDeadline = false
				return NoteHandled
			}
			jos.blocked = true
			return NoteExit
		}
		cpu.SetReg(10, uint64(ret))
		ctx.snapshot = snapshotJea9LinuxCPU(cpu)
		return NoteHandled
	case jea9LinuxFutexWake, jea9LinuxFutexWakeBitset:
		woken := jos.wakeFutex(addr, int(val))
		cpu.SetReg(10, uint64(woken))
		ctx.snapshot = snapshotJea9LinuxCPU(cpu)
		return NoteHandled
	default:
		ret := jea9LinuxErrEINVAL
		cpu.SetReg(10, uint64(ret))
		ctx.snapshot = snapshotJea9LinuxCPU(cpu)
		return NoteHandled
	}
}

func (jos *Jea9Linux) futexWait(cpu *CPU, ctx *jea9LinuxContext, addr uint64, expected uint32, timeoutAddr uint64) (int64, bool) {
	if addr%4 != 0 {
		return jea9LinuxErrEINVAL, false
	}
	got, f := cpu.mem.Load32(addr)
	if f != nil {
		return jea9LinuxErrEFAULT, false
	}
	if got != expected {
		return jea9LinuxErrEAGAIN, false
	}
	deadline, hasDeadline, errno := jos.futexDeadline(cpu, timeoutAddr)
	if errno != 0 {
		return errno, false
	}
	if hasDeadline && deadline <= jos.monotonicNS {
		return jea9LinuxErrETIMEDOUT, false
	}
	if hasDeadline {
		if _, ok := jos.nextRunnableByPolicyAfterCurrent(); !ok {
			if _, reached := jos.advanceVirtualClockTowardDeadline(cpu, "futex-timeout", deadline); reached {
				return jea9LinuxErrETIMEDOUT, false
			}
		}
	}
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	ctx.state = jea9LinuxContextWaiting
	jos.clearContextWaitFields(ctx)
	ctx.waitKind = jea9LinuxWaitFutex
	ctx.waitAddr = addr
	ctx.waitDeadlineNS = deadline
	ctx.waitHasDeadline = hasDeadline
	jos.futexWaiters[addr] = append(jos.futexWaiters[addr], ctx.tid)
	if hasDeadline {
		jos.timedFutexWaiters++
		jos.blockedUntil = deadline
		jos.blockedHasDeadline = true
	}
	return 0, true
}

func (jos *Jea9Linux) futexDeadline(cpu *CPU, timeoutAddr uint64) (int64, bool, int64) {
	if timeoutAddr == 0 {
		return 0, false, 0
	}
	return jos.timespecDeadline(cpu, timeoutAddr)
}

func (jos *Jea9Linux) wakeFutex(addr uint64, limit int) int {
	if limit <= 0 {
		return 0
	}
	waiters := jos.futexWaiters[addr]
	woken := 0
	kept := waiters[:0]
	for _, tid := range waiters {
		if woken < limit {
			jos.markRunnable(tid, 0)
			woken++
			continue
		}
		kept = append(kept, tid)
	}
	if len(kept) == 0 {
		delete(jos.futexWaiters, addr)
	} else {
		jos.futexWaiters[addr] = kept
	}
	if woken > 0 && jos.hasRunnableContext() {
		jos.blocked = false
		jos.blockedHasDeadline = false
	}
	return woken
}

func (jos *Jea9Linux) sysGetrandom(cpu *CPU, bufAddr, n, flags uint64) int64 {
	const supportedFlags = uint64(1 | 2) // GRND_NONBLOCK | GRND_RANDOM
	if flags&^supportedFlags != 0 {
		return jea9LinuxErrEINVAL
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	buf := make([]byte, int(n))
	jos.fillRandom(buf)
	jos.recordRandomTrace("getrandom", n, flags, buf)
	if f := cpu.mem.WriteBytes(bufAddr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return int64(n)
}

func (jos *Jea9Linux) sysOpenat(cpu *CPU, dirfd, pathAddr, flags, mode uint64) int64 {
	_ = dirfd
	path, errno := readLinuxCString(cpu, pathAddr, 4096)
	if errno != 0 {
		return errno
	}
	switch path {
	case "/dev/urandom", "/dev/random":
		if flags&jea9LinuxOAccmode != 0 {
			return jea9LinuxErrEACCES
		}
		fd := jos.allocFD(jea9LinuxFD{kind: jea9LinuxFDRandom})
		return int64(fd)
	default:
		if data, ok := jos.files[path]; ok {
			if flags&jea9LinuxOAccmode != 0 {
				return jea9LinuxErrEACCES
			}
			fd := jos.allocFD(jea9LinuxFD{kind: jea9LinuxFDFile, data: data, flags: flags})
			return int64(fd)
		}
		if jos.allowAllHostFiles {
			return jos.sysOpenHostFile(path, flags, mode)
		}
		return jea9LinuxErrENOENT
	}
}

func (jos *Jea9Linux) sysOpenHostFile(path string, flags, mode uint64) int64 {
	hostFlags, errno := jea9LinuxHostOpenFlags(flags)
	if errno != 0 {
		return errno
	}
	perm := os.FileMode(mode & 0o777)
	file, err := os.OpenFile(path, hostFlags, perm)
	if err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	fd := jos.allocFD(jea9LinuxFD{
		kind:     jea9LinuxFDHostFile,
		hostFile: file,
		flags:    flags,
	})
	return int64(fd)
}

func jea9LinuxHostOpenFlags(flags uint64) (int, int64) {
	var hostFlags int
	switch flags & jea9LinuxOAccmode {
	case 0:
		hostFlags = os.O_RDONLY
	case jea9LinuxOWronly:
		hostFlags = os.O_WRONLY
	case jea9LinuxORdwr:
		hostFlags = os.O_RDWR
	default:
		return 0, jea9LinuxErrEINVAL
	}
	if flags&jea9LinuxOCreat != 0 {
		hostFlags |= os.O_CREATE
	}
	if flags&jea9LinuxOExcl != 0 {
		hostFlags |= os.O_EXCL
	}
	if flags&jea9LinuxOTrunc != 0 {
		hostFlags |= os.O_TRUNC
	}
	if flags&jea9LinuxOAppend != 0 {
		hostFlags |= os.O_APPEND
	}
	return hostFlags, 0
}

func jea9LinuxErrnoFromHost(err error) int64 {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, syscall.EBADF), errors.Is(err, os.ErrClosed):
		return jea9LinuxErrEBADF
	case errors.Is(err, syscall.EFAULT):
		return jea9LinuxErrEFAULT
	case errors.Is(err, syscall.EINVAL), errors.Is(err, os.ErrInvalid):
		return jea9LinuxErrEINVAL
	case errors.Is(err, syscall.ENOENT), errors.Is(err, os.ErrNotExist):
		return jea9LinuxErrENOENT
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM), errors.Is(err, os.ErrPermission):
		return jea9LinuxErrEACCES
	case errors.Is(err, syscall.EEXIST), errors.Is(err, os.ErrExist):
		return jea9LinuxErrEEXIST
	case errors.Is(err, syscall.ENOTDIR):
		return jea9LinuxErrENOTDIR
	case errors.Is(err, syscall.EISDIR):
		return jea9LinuxErrEISDIR
	case errors.Is(err, syscall.ENAMETOOLONG):
		return jea9LinuxErrENAMETOOLONG
	case errors.Is(err, syscall.ESPIPE):
		return jea9LinuxErrESPIPE
	case errors.Is(err, syscall.EAGAIN):
		return jea9LinuxErrEAGAIN
	default:
		return jea9LinuxErrEIO
	}
}

func (jos *Jea9Linux) sysRead(cpu *CPU, fdRaw, bufAddr, n uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	switch f.kind {
	case jea9LinuxFDStdin:
		if jos.stdin == nil {
			return 0
		}
		buf := make([]byte, int(n))
		nread, err := jos.stdin.Read(buf)
		if nread < 0 {
			return jea9LinuxErrEIO
		}
		if nread > len(buf) {
			nread = len(buf)
		}
		if nread > 0 {
			if fault := cpu.mem.WriteBytes(bufAddr, buf[:nread]); fault != nil {
				return jea9LinuxErrEFAULT
			}
			return int64(nread)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return jea9LinuxErrEIO
		}
		return 0
	case jea9LinuxFDRandom:
		buf := make([]byte, int(n))
		jos.fillRandom(buf)
		jos.recordRandomTrace("random-device", n, 0, buf)
		if fault := cpu.mem.WriteBytes(bufAddr, buf); fault != nil {
			return jea9LinuxErrEFAULT
		}
		return int64(n)
	case jea9LinuxFDFile:
		count := readJea9LinuxFileRange(cpu, f, bufAddr, n, f.off)
		if count <= 0 {
			return count
		}
		f.off += count
		jos.fds[fd] = f
		return count
	case jea9LinuxFDHostFile:
		return readJea9LinuxHostFile(cpu, f, bufAddr, n)
	case jea9LinuxFDEventfd:
		return jos.sysEventfdRead(cpu, fd, f, bufAddr, n)
	case jea9LinuxFDPipeRead:
		return jos.sysPipeRead(cpu, fd, f, bufAddr, n)
	default:
		return jea9LinuxErrEBADF
	}
}

func (jos *Jea9Linux) sysWrite(cpu *CPU, fdRaw, bufAddr, n uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	buf := make([]byte, int(n))
	if fault := cpu.mem.ReadBytes(bufAddr, buf); fault != nil {
		return jea9LinuxErrEFAULT
	}
	var w io.Writer
	switch f.kind {
	case jea9LinuxFDStdout:
		w = jos.stdout
	case jea9LinuxFDStderr:
		w = jos.stderr
	case jea9LinuxFDHostFile:
		if f.hostFile == nil {
			return jea9LinuxErrEBADF
		}
		written, err := f.hostFile.Write(buf)
		if written < 0 {
			return jea9LinuxErrEIO
		}
		if written > len(buf) {
			written = len(buf)
		}
		if written == 0 && err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
		return int64(written)
	case jea9LinuxFDEventfd:
		return jos.sysEventfdWrite(cpu, fd, f, bufAddr, n)
	case jea9LinuxFDPipeWrite:
		return jos.sysPipeWrite(cpu, fd, f, bufAddr, n)
	default:
		return jea9LinuxErrEBADF
	}
	written, err := w.Write(buf)
	if written < 0 {
		return jea9LinuxErrEIO
	}
	if written > len(buf) {
		written = len(buf)
	}
	if written == 0 && err != nil {
		return jea9LinuxErrEIO
	}
	return int64(written)
}

func (jos *Jea9Linux) sysEventfdRead(cpu *CPU, fd int, f jea9LinuxFD, bufAddr, n uint64) int64 {
	if n < 8 {
		return jea9LinuxErrEINVAL
	}
	if f.eventfdCounter == 0 {
		return jea9LinuxErrEAGAIN
	}
	value := f.eventfdCounter
	if f.flags&jea9LinuxEFDSemaphore != 0 {
		value = 1
		f.eventfdCounter--
	} else {
		f.eventfdCounter = 0
	}
	if fault := cpu.mem.Store64(bufAddr, value); fault != nil {
		return jea9LinuxErrEFAULT
	}
	jos.fds[fd] = f
	return 8
}

func (jos *Jea9Linux) sysEventfdWrite(cpu *CPU, fd int, f jea9LinuxFD, bufAddr, n uint64) int64 {
	if n < 8 {
		return jea9LinuxErrEINVAL
	}
	value, fault := cpu.mem.Load64(bufAddr)
	if fault != nil {
		return jea9LinuxErrEFAULT
	}
	if value == ^uint64(0) || f.eventfdCounter > (^uint64(0)-1)-value {
		return jea9LinuxErrEAGAIN
	}
	f.eventfdCounter += value
	jos.fds[fd] = f
	jos.wakeEpollWaitersForFD(cpu, fd)
	return 8
}

func (jos *Jea9Linux) sysPipeRead(cpu *CPU, fd int, f jea9LinuxFD, bufAddr, n uint64) int64 {
	_ = fd
	if f.pipe == nil {
		return jea9LinuxErrEBADF
	}
	if len(f.pipe.buf) == 0 {
		return jea9LinuxErrEAGAIN
	}
	count := int(n)
	if count > len(f.pipe.buf) {
		count = len(f.pipe.buf)
	}
	if fault := cpu.mem.WriteBytes(bufAddr, f.pipe.buf[:count]); fault != nil {
		return jea9LinuxErrEFAULT
	}
	copy(f.pipe.buf, f.pipe.buf[count:])
	f.pipe.buf = f.pipe.buf[:len(f.pipe.buf)-count]
	return int64(count)
}

func (jos *Jea9Linux) sysPipeWrite(cpu *CPU, fd int, f jea9LinuxFD, bufAddr, n uint64) int64 {
	_ = fd
	if f.pipe == nil {
		return jea9LinuxErrEBADF
	}
	space := cap(f.pipe.buf) - len(f.pipe.buf)
	if space == 0 {
		return jea9LinuxErrEAGAIN
	}
	count := int(n)
	if count > space {
		count = space
	}
	buf := make([]byte, count)
	if fault := cpu.mem.ReadBytes(bufAddr, buf); fault != nil {
		return jea9LinuxErrEFAULT
	}
	f.pipe.buf = append(f.pipe.buf, buf...)
	jos.wakeEpollWaitersForFD(cpu, f.pipe.readFD)
	return int64(count)
}

func (jos *Jea9Linux) sysClose(fdRaw uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	delete(jos.fds, fd)
	if f.kind == jea9LinuxFDHostFile && f.hostFile != nil {
		if err := f.hostFile.Close(); err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
	}
	return 0
}

func (jos *Jea9Linux) sysFcntl(fdRaw, cmd, arg uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	switch cmd {
	case jea9LinuxFGetFD:
		return 0
	case jea9LinuxFSetFD:
		return 0
	case jea9LinuxFGetFL:
		return int64(f.flags)
	case jea9LinuxFSetFL:
		f.flags = arg
		jos.fds[fd] = f
		return 0
	default:
		return jea9LinuxErrEINVAL
	}
}

func (jos *Jea9Linux) sysIoctl(cpu *CPU, fdRaw, req, arg uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if !f.kind.isTerminal() {
		return jea9LinuxErrENOTTY
	}
	switch req {
	case jea9LinuxTCGETS:
		t := f.termios
		if !f.termiosSet {
			t = defaultJea9LinuxTermios()
		}
		if fault := cpu.mem.WriteBytes(arg, t[:]); fault != nil {
			return jea9LinuxErrEFAULT
		}
		return 0
	case jea9LinuxTCSETS, jea9LinuxTCSETSW, jea9LinuxTCSETSF:
		var t [jea9LinuxTermiosSize]byte
		if fault := cpu.mem.ReadBytes(arg, t[:]); fault != nil {
			return jea9LinuxErrEFAULT
		}
		f.termios = t
		f.termiosSet = true
		jos.fds[fd] = f
		return 0
	case jea9LinuxTIOCGWINSZ:
		w := f.winsize
		if !f.winsizeSet {
			w = defaultJea9LinuxWinsize()
		}
		if fault := cpu.mem.WriteBytes(arg, w[:]); fault != nil {
			return jea9LinuxErrEFAULT
		}
		return 0
	case jea9LinuxTIOCSWINSZ:
		var w [jea9LinuxWinsizeSize]byte
		if fault := cpu.mem.ReadBytes(arg, w[:]); fault != nil {
			return jea9LinuxErrEFAULT
		}
		f.winsize = w
		f.winsizeSet = true
		jos.fds[fd] = f
		return 0
	default:
		return jea9LinuxErrENOTTY
	}
}

func (k jea9LinuxFDKind) isTerminal() bool {
	return k == jea9LinuxFDStdin || k == jea9LinuxFDStdout || k == jea9LinuxFDStderr
}

func defaultJea9LinuxTermios() [jea9LinuxTermiosSize]byte {
	const (
		iflag = uint32(0x2 | 0x100 | 0x400)                    // BRKINT | ICRNL | IXON
		oflag = uint32(0x1 | 0x4)                              // OPOST | ONLCR
		cflag = uint32(0xf | 0x30 | 0x80 | 0x400)              // B38400 | CS8 | CREAD | HUPCL
		lflag = uint32(0x1 | 0x2 | 0x8 | 0x10 | 0x20 | 0x8000) // ISIG | ICANON | ECHO | ECHOE | ECHOK | IEXTEN
	)
	var t [jea9LinuxTermiosSize]byte
	binary.LittleEndian.PutUint32(t[0:], iflag)
	binary.LittleEndian.PutUint32(t[4:], oflag)
	binary.LittleEndian.PutUint32(t[8:], cflag)
	binary.LittleEndian.PutUint32(t[12:], lflag)
	cc := t[17:36]
	cc[0] = 3                                  // VINTR
	cc[1] = 28                                 // VQUIT
	cc[2] = 127                                // VERASE
	cc[3] = 21                                 // VKILL
	cc[4] = 4                                  // VEOF
	cc[6] = 1                                  // VMIN
	cc[8] = 17                                 // VSTART
	cc[9] = 19                                 // VSTOP
	cc[10] = 26                                // VSUSP
	cc[12] = 18                                // VREPRINT
	cc[13] = 15                                // VDISCARD
	cc[14] = 23                                // VWERASE
	cc[15] = 22                                // VLNEXT
	binary.LittleEndian.PutUint32(t[36:], 0xf) // input speed: B38400
	binary.LittleEndian.PutUint32(t[40:], 0xf) // output speed: B38400
	return t
}

func defaultJea9LinuxWinsize() [jea9LinuxWinsizeSize]byte {
	var w [jea9LinuxWinsizeSize]byte
	binary.LittleEndian.PutUint16(w[0:], 24)
	binary.LittleEndian.PutUint16(w[2:], 80)
	return w
}

func (jos *Jea9Linux) sysLseek(fdRaw, offRaw, whence uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if f.kind != jea9LinuxFDFile && f.kind != jea9LinuxFDHostFile {
		return jea9LinuxErrESPIPE
	}
	off := int64(offRaw)
	if f.kind == jea9LinuxFDHostFile {
		if f.hostFile == nil {
			return jea9LinuxErrEBADF
		}
		if whence != jea9LinuxSeekSet && whence != jea9LinuxSeekCur && whence != jea9LinuxSeekEnd {
			return jea9LinuxErrEINVAL
		}
		next, err := f.hostFile.Seek(off, int(whence))
		if err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
		if next < 0 {
			return jea9LinuxErrEINVAL
		}
		return next
	}
	var next int64
	switch whence {
	case jea9LinuxSeekSet:
		next = off
	case jea9LinuxSeekCur:
		next = f.off + off
	case jea9LinuxSeekEnd:
		next = int64(len(f.data)) + off
	default:
		return jea9LinuxErrEINVAL
	}
	if next < 0 {
		return jea9LinuxErrEINVAL
	}
	f.off = next
	jos.fds[fd] = f
	return next
}

func (jos *Jea9Linux) sysPread64(cpu *CPU, fdRaw, bufAddr, n, offRaw uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if f.kind != jea9LinuxFDFile && f.kind != jea9LinuxFDHostFile {
		return jea9LinuxErrESPIPE
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	off := int64(offRaw)
	if off < 0 {
		return jea9LinuxErrEINVAL
	}
	if f.kind == jea9LinuxFDHostFile {
		return preadJea9LinuxHostFile(cpu, f, bufAddr, n, off)
	}
	return readJea9LinuxFileRange(cpu, f, bufAddr, n, off)
}

func readJea9LinuxHostFile(cpu *CPU, f jea9LinuxFD, bufAddr, n uint64) int64 {
	if f.hostFile == nil {
		return jea9LinuxErrEBADF
	}
	buf := make([]byte, int(n))
	nread, err := f.hostFile.Read(buf)
	if nread < 0 {
		return jea9LinuxErrEIO
	}
	if nread > len(buf) {
		nread = len(buf)
	}
	if nread > 0 {
		if fault := cpu.mem.WriteBytes(bufAddr, buf[:nread]); fault != nil {
			return jea9LinuxErrEFAULT
		}
		return int64(nread)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return jea9LinuxErrnoFromHost(err)
	}
	return 0
}

func preadJea9LinuxHostFile(cpu *CPU, f jea9LinuxFD, bufAddr, n uint64, off int64) int64 {
	if f.hostFile == nil {
		return jea9LinuxErrEBADF
	}
	buf := make([]byte, int(n))
	nread, err := f.hostFile.ReadAt(buf, off)
	if nread < 0 {
		return jea9LinuxErrEIO
	}
	if nread > len(buf) {
		nread = len(buf)
	}
	if nread > 0 {
		if fault := cpu.mem.WriteBytes(bufAddr, buf[:nread]); fault != nil {
			return jea9LinuxErrEFAULT
		}
		return int64(nread)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return jea9LinuxErrnoFromHost(err)
	}
	return 0
}

func readJea9LinuxFileRange(cpu *CPU, f jea9LinuxFD, bufAddr, n uint64, off int64) int64 {
	if off < 0 {
		return jea9LinuxErrEINVAL
	}
	if off >= int64(len(f.data)) {
		return 0
	}
	remaining := int64(len(f.data)) - off
	count := int64(n)
	if count > remaining {
		count = remaining
	}
	if fault := cpu.mem.WriteBytes(bufAddr, f.data[off:off+count]); fault != nil {
		return jea9LinuxErrEFAULT
	}
	return count
}

func readLinuxCString(cpu *CPU, addr uint64, max int) (string, int64) {
	buf := make([]byte, 0, 64)
	for i := 0; i < max; i++ {
		b, f := cpu.mem.Load8(addr + uint64(i))
		if f != nil {
			return "", jea9LinuxErrEFAULT
		}
		if b == 0 {
			return string(buf), 0
		}
		buf = append(buf, b)
	}
	return "", jea9LinuxErrEINVAL
}

func (jos *Jea9Linux) sysUname(cpu *CPU, addr uint64) int64 {
	const fieldLen = 65
	buf := make([]byte, fieldLen*6)
	storeFixedLinuxString(buf[0*fieldLen:], fieldLen, "Linux")
	storeFixedLinuxString(buf[1*fieldLen:], fieldLen, "jea9linux")
	storeFixedLinuxString(buf[2*fieldLen:], fieldLen, "6.14.0-jea9linux")
	storeFixedLinuxString(buf[3*fieldLen:], fieldLen, "#1 deterministic")
	storeFixedLinuxString(buf[4*fieldLen:], fieldLen, "riscv64")
	storeFixedLinuxString(buf[5*fieldLen:], fieldLen, "(none)")
	if f := cpu.mem.WriteBytes(addr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func storeFixedLinuxString(dst []byte, fieldLen int, s string) {
	if len(dst) > fieldLen {
		dst = dst[:fieldLen]
	}
	copy(dst, s)
}

func (jos *Jea9Linux) sysGetrlimit(cpu *CPU, resource, addr uint64) int64 {
	cur, max, ok := jea9LinuxRlimit(resource)
	if !ok {
		return jea9LinuxErrEINVAL
	}
	return storeLinuxRlimit(cpu, addr, cur, max)
}

func (jos *Jea9Linux) sysPrlimit64(cpu *CPU, pid, resource, newLimitAddr, oldLimitAddr uint64) int64 {
	if pid != 0 && pid != jos.pid {
		return jea9LinuxErrESRCH
	}
	if newLimitAddr != 0 {
		return jea9LinuxErrEPERM
	}
	if oldLimitAddr != 0 {
		cur, max, ok := jea9LinuxRlimit(resource)
		if !ok {
			return jea9LinuxErrEINVAL
		}
		return storeLinuxRlimit(cpu, oldLimitAddr, cur, max)
	}
	_, _, ok := jea9LinuxRlimit(resource)
	if !ok {
		return jea9LinuxErrEINVAL
	}
	return 0
}

func jea9LinuxRlimit(resource uint64) (cur, max uint64, ok bool) {
	switch resource {
	case jea9LinuxRLimitStack:
		return 8 * 1024 * 1024, 8 * 1024 * 1024, true
	case jea9LinuxRLimitNOFile:
		return 1024, 1024, true
	case jea9LinuxRLimitAS:
		return ^uint64(0), ^uint64(0), true
	default:
		if resource < 16 {
			return ^uint64(0), ^uint64(0), true
		}
		return 0, 0, false
	}
}

func storeLinuxRlimit(cpu *CPU, addr, cur, max uint64) int64 {
	if f := cpu.mem.Store64(addr, cur); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+8, max); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) sysSysinfo(cpu *CPU, addr uint64) int64 {
	buf := make([]byte, 112)
	uptime := jos.monotonicNS / 1_000_000_000
	if uptime < 0 {
		uptime = 0
	}
	binary.LittleEndian.PutUint64(buf[0:], uint64(uptime))
	binary.LittleEndian.PutUint64(buf[32:], cpu.mem.Size())
	binary.LittleEndian.PutUint64(buf[40:], cpu.mem.Size())
	binary.LittleEndian.PutUint16(buf[80:], 1)
	binary.LittleEndian.PutUint32(buf[104:], 1)
	if f := cpu.mem.WriteBytes(addr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) sysPrctl(cpu *CPU, option, arg2, arg3, arg4, arg5 uint64) int64 {
	_, _, _ = arg3, arg4, arg5
	switch option {
	case jea9LinuxPRSetName:
		name, errno := readLinuxThreadName(cpu, arg2)
		if errno != 0 {
			return errno
		}
		jos.threadName = name
		return 0
	case jea9LinuxPRGetName:
		var buf [16]byte
		copy(buf[:], jos.threadName)
		if f := cpu.mem.WriteBytes(arg2, buf[:]); f != nil {
			return jea9LinuxErrEFAULT
		}
		return 0
	case jea9LinuxPRSetVMA:
		return 0
	default:
		return jea9LinuxErrEINVAL
	}
}

func readLinuxThreadName(cpu *CPU, addr uint64) (string, int64) {
	var raw [16]byte
	for i := range raw {
		b, f := cpu.mem.Load8(addr + uint64(i))
		if f != nil {
			return "", jea9LinuxErrEFAULT
		}
		if b == 0 {
			return string(raw[:i]), 0
		}
		raw[i] = b
	}
	return string(raw[:15]), 0
}

func (jos *Jea9Linux) sysBrk(cpu *CPU, addr uint64) uint64 {
	vm := jos.ensureVM(cpu)
	if addr == 0 {
		return vm.brk
	}
	if addr < vm.minBrk || addr >= cpu.mem.Size()-2*GuestPageSize {
		return vm.brk
	}
	old := vm.brk
	if addr > old {
		begin := jea9LinuxAlignUp(old)
		end := jea9LinuxAlignUp(addr)
		if end > begin {
			if f := cpu.mem.Zero(begin, end-begin); f != nil {
				return vm.brk
			}
			vm.mapRange(begin, end-begin, jea9LinuxProtRead|jea9LinuxProtWrite)
		}
	} else if addr < old {
		begin := jea9LinuxAlignUp(addr)
		end := jea9LinuxAlignUp(old)
		if end > begin {
			vm.unmapRange(begin, end-begin)
			cpu.mem.RemoveExecRegion(begin, end)
		}
	}
	vm.brk = addr
	return addr
}

func (jos *Jea9Linux) sysMmap(cpu *CPU, addr, length, prot, flags, fd, off uint64) int64 {
	_, _ = fd, off
	vm := jos.ensureVM(cpu)
	if length == 0 || prot&^(jea9LinuxProtRead|jea9LinuxProtWrite|jea9LinuxProtExec) != 0 {
		return jea9LinuxErrEINVAL
	}
	length = jea9LinuxAlignUp(length)
	if flags&jea9LinuxMapAnonymous == 0 {
		return jea9LinuxErrEINVAL
	}
	var chosen uint64
	if flags&jea9LinuxMapFixed != 0 {
		if addr == 0 || addr%GuestPageSize != 0 {
			return jea9LinuxErrEINVAL
		}
		if _, _, ok := jea9LinuxPageRange(addr, length, cpu.mem.Size()); !ok {
			return jea9LinuxErrEINVAL
		}
		chosen = addr
	} else {
		var ok bool
		chosen, ok = vm.allocRange(cpu.mem.Size(), length)
		if !ok {
			return jea9LinuxErrENOMEM
		}
	}
	extra := uint64(0)
	if prot == 0 {
		extra = jea9LinuxPageNeedsZero
	} else if f := cpu.mem.Zero(chosen, length); f != nil {
		return jea9LinuxErrENOMEM
	}
	vm.mapRangeState(chosen, length, prot, extra)
	vm.updateExecMetadata(&cpu.mem, chosen, length, prot)
	return int64(chosen)
}

func (jos *Jea9Linux) sysMunmap(cpu *CPU, addr, length uint64) int64 {
	vm := jos.ensureVM(cpu)
	if addr%GuestPageSize != 0 {
		return jea9LinuxErrEINVAL
	}
	begin, end, ok := jea9LinuxPageRange(addr, length, cpu.mem.Size())
	if !ok {
		return jea9LinuxErrEINVAL
	}
	vm.unmapRange(begin, end-begin)
	cpu.mem.RemoveExecRegion(begin, end)
	return 0
}

func (jos *Jea9Linux) sysMprotect(cpu *CPU, addr, length, prot uint64) int64 {
	vm := jos.ensureVM(cpu)
	if addr%GuestPageSize != 0 || prot&^(jea9LinuxProtRead|jea9LinuxProtWrite|jea9LinuxProtExec) != 0 {
		return jea9LinuxErrEINVAL
	}
	begin, end, ok := jea9LinuxPageRange(addr, length, cpu.mem.Size())
	if !ok {
		return jea9LinuxErrEINVAL
	}
	if vm.rangeUnmapped(begin, end-begin) {
		return jea9LinuxErrENOMEM
	}
	if prot != 0 {
		if f := vm.zeroNeededRanges(&cpu.mem, begin, end-begin); f != nil {
			return jea9LinuxErrENOMEM
		}
	}
	vm.protectRange(begin, end-begin, prot)
	vm.updateExecMetadata(&cpu.mem, begin, end-begin, prot)
	return 0
}

func (jos *Jea9Linux) sysMincore(cpu *CPU, addr, length, vecAddr uint64) int64 {
	vm := jos.ensureVM(cpu)
	if addr%GuestPageSize != 0 {
		return jea9LinuxErrEINVAL
	}
	begin, end, ok := jea9LinuxPageRange(addr, length, cpu.mem.Size())
	if !ok {
		return jea9LinuxErrEINVAL
	}
	if vm.rangeUnmapped(begin, end-begin) {
		return jea9LinuxErrENOMEM
	}
	pages := int((end - begin) / GuestPageSize)
	vec := make([]byte, pages)
	for i := range vec {
		vec[i] = 1
	}
	if f := cpu.mem.WriteBytes(vecAddr, vec); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) sysMadvise(cpu *CPU, addr, length, advice uint64) int64 {
	_, _ = jos.ensureVM(cpu), advice
	if length == 0 {
		return 0
	}
	if _, _, ok := jea9LinuxPageRange(addr, length, cpu.mem.Size()); !ok {
		return jea9LinuxErrEINVAL
	}
	return 0
}

func (jos *Jea9Linux) sysClockGettime(cpu *CPU, clockID, tsAddr uint64) int64 {
	ns, ok := jos.clockNow(clockID)
	if !ok {
		return jea9LinuxErrEINVAL
	}
	jos.recordClockTrace("clock_gettime", clockID, ns)
	return storeLinuxTimespec(cpu, tsAddr, ns)
}

func (jos *Jea9Linux) sysGettimeofday(cpu *CPU, tvAddr, tzAddr uint64) int64 {
	if tvAddr != 0 {
		ns := jos.monotonicNS + jos.realtimeOffsetNS
		jos.recordClockTrace("gettimeofday", jea9LinuxClockRealtime, ns)
		sec, nsec := splitLinuxNS(ns)
		if f := cpu.mem.Store64(tvAddr, uint64(sec)); f != nil {
			return jea9LinuxErrEFAULT
		}
		if f := cpu.mem.Store64(tvAddr+8, uint64(nsec/1000)); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	if tzAddr != 0 {
		if f := cpu.mem.Store64(tzAddr, 0); f != nil {
			return jea9LinuxErrEFAULT
		}
		if f := cpu.mem.Store64(tzAddr+8, 0); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	return 0
}

func (jos *Jea9Linux) sysNanosleep(cpu *CPU, reqAddr, remAddr uint64) NoteDisposition {
	_ = remAddr
	secRaw, f := cpu.mem.Load64(reqAddr)
	if f != nil {
		setJea9LinuxReturn(cpu, jea9LinuxErrEFAULT)
		return NoteHandled
	}
	nsecRaw, f := cpu.mem.Load64(reqAddr + 8)
	if f != nil {
		setJea9LinuxReturn(cpu, jea9LinuxErrEFAULT)
		return NoteHandled
	}
	sec := int64(secRaw)
	nsec := int64(nsecRaw)
	if sec < 0 || nsec < 0 || nsec >= 1_000_000_000 {
		setJea9LinuxReturn(cpu, jea9LinuxErrEINVAL)
		return NoteHandled
	}
	delta := sec*1_000_000_000 + nsec
	if delta >= 0 {
		deltaU := uint64(delta)
		jos.nanosleepCount++
		jos.nanosleepTotalNS += deltaU
		if deltaU > jos.nanosleepMaxNS {
			jos.nanosleepMaxNS = deltaU
		}
	}
	advance := delta
	if jos.nanosleepMode == Jea9NanosleepAdvanceFixed {
		advance = jos.nanosleepFixedNS
		if advance < 0 {
			advance = 0
		}
	}
	if advance <= 0 {
		cpu.SetReg(10, 0)
		return NoteHandled
	}

	ctx := jos.ensureScheduler(cpu)
	ctx.snapshot = snapshotJea9LinuxCPU(cpu)
	ctx.state = jea9LinuxContextWaiting
	jos.clearContextWaitFields(ctx)
	ctx.waitKind = jea9LinuxWaitNanosleep
	ctx.waitDeadlineNS = jos.monotonicNS + advance
	ctx.waitHasDeadline = true
	jos.timedNanosleepWaiters++
	return jos.scheduleAfterCurrentBlocked(cpu)
}

func (jos *Jea9Linux) refreshBlocked() {
	if jos.timedFutexWaiters > 0 {
		jos.refreshFutexTimeouts()
	}
	if jos.timedEpollWaiters > 0 {
		jos.refreshEpollTimeouts()
	}
	if jos.timedNanosleepWaiters > 0 {
		jos.refreshNanosleepTimeouts()
	}
	if jos.blocked && jos.hasRunnableContext() {
		jos.blocked = false
		jos.blockedHasDeadline = false
		return
	}
	if jos.blocked && jos.blockedHasDeadline && jos.monotonicNS >= jos.blockedUntil {
		jos.blocked = false
		jos.blockedHasDeadline = false
	}
}

func (jos *Jea9Linux) refreshFutexTimeouts() {
	if len(jos.contexts) == 0 {
		return
	}
	for _, ctx := range jos.contexts {
		if ctx.state != jea9LinuxContextWaiting || ctx.waitKind != jea9LinuxWaitFutex || !ctx.waitHasDeadline {
			continue
		}
		if jos.monotonicNS < ctx.waitDeadlineNS {
			continue
		}
		addr := ctx.waitAddr
		jos.removeFutexWaiter(addr, ctx.tid)
		jos.markRunnable(ctx.tid, jea9LinuxErrETIMEDOUT)
	}
}

func (jos *Jea9Linux) refreshEpollTimeouts() {
	if len(jos.contexts) == 0 {
		return
	}
	for _, ctx := range jos.contexts {
		if ctx.state != jea9LinuxContextWaiting || ctx.waitKind != jea9LinuxWaitEpoll || !ctx.waitHasDeadline {
			continue
		}
		if jos.monotonicNS < ctx.waitDeadlineNS {
			continue
		}
		jos.markRunnable(ctx.tid, 0)
	}
}

func (jos *Jea9Linux) refreshNanosleepTimeouts() {
	if len(jos.contexts) == 0 {
		return
	}
	for _, ctx := range jos.contexts {
		if ctx.state != jea9LinuxContextWaiting || ctx.waitKind != jea9LinuxWaitNanosleep || !ctx.waitHasDeadline {
			continue
		}
		if jos.monotonicNS < ctx.waitDeadlineNS {
			continue
		}
		jos.markRunnable(ctx.tid, 0)
	}
}

func (jos *Jea9Linux) clockNow(clockID uint64) (int64, bool) {
	switch clockID {
	case jea9LinuxClockRealtime, jea9LinuxClockRealtimeCoarse:
		return jos.monotonicNS + jos.realtimeOffsetNS, true
	case jea9LinuxClockMonotonic, jea9LinuxClockMonotonicCoarse:
		return jos.monotonicNS, true
	default:
		return 0, false
	}
}

func storeLinuxTimespec(cpu *CPU, addr uint64, ns int64) int64 {
	sec, nsec := splitLinuxNS(ns)
	if f := cpu.mem.Store64(addr, uint64(sec)); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+8, uint64(nsec)); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func splitLinuxNS(ns int64) (sec int64, nsec int64) {
	sec = ns / 1_000_000_000
	nsec = ns % 1_000_000_000
	if nsec < 0 {
		sec--
		nsec += 1_000_000_000
	}
	return sec, nsec
}

func InstallJea9Linux(cpu *CPU, jos *Jea9Linux) func() {

	cleanupCore := installJea9LinuxCore(cpu, jos)
	return func() {
		cleanupCore()
	}
}

func InstallJea9LinuxJIT(cpu *CPU, jit *JIT, jos *Jea9Linux) func() {
	cleanupCore := installJea9LinuxCore(cpu, jos)

	jit.faultPageZero = true
	return func() {
		cleanupCore()
	}
}

func installJea9LinuxCore(cpu *CPU, jos *Jea9Linux) func() {
	vm := jos.ensureVM(cpu)
	jos.ensureScheduler(cpu)
	cpu.Notes.Push(jos.Handle)
	return func() {
		cpu.Notes.Pop()
		(&cpu.mem).clearAccessOverlay(vm)
	}
}

func RunWithJea9Linux(cpu *CPU, jos *Jea9Linux) (exitCode int, err error) {
	cleanup := InstallJea9Linux(cpu, jos)
	defer cleanup()
	for {
		err = jos.Run(cpu)
		if errors.Is(err, ErrJea9LinuxBudget) {
			continue
		}
		if err != nil {
			if ex, ok := err.(*ExitError); ok {
				return ex.Code, nil
			}
			return 0, err
		}
		return cpu.ExitCode, nil
	}
}

func RunWithJea9LinuxJIT(cpu *CPU, jit *JIT, jos *Jea9Linux) (exitCode int, err error) {
	cleanup := InstallJea9LinuxJIT(cpu, jit, jos)
	defer cleanup()
	for {
		err = jos.RunJIT(cpu, jit)
		if errors.Is(err, ErrJea9LinuxBudget) {
			continue
		}
		if err != nil {
			if ex, ok := err.(*ExitError); ok {
				return ex.Code, nil
			}
			return 0, err
		}
		return cpu.ExitCode, nil
	}
}
