package supervisor

import execproto "goos/userspace/exec"

// Mode is how PID 1 runs the system (ADR 0175). It starts the bootstrap
// services from the boot archive while starting; normal runs the installed
// system from the volume, and recovery runs the archive's own programs
// without it.
type Mode uint8

const (
	ModeStarting Mode = iota
	ModeNormal
	ModeRecovery
)

func (mode Mode) String() string {
	switch mode {
	case ModeNormal:
		return "normal"
	case ModeRecovery:
		return "recovery"
	}
	return "starting"
}

// Stage names the step of the boot, or of the running system, that sent it
// into recovery.
type Stage uint8

const (
	StageNone Stage = iota
	// StageForced is recovery the boot media asked for.
	StageForced
	StageStorage
	StageRecord
	StageServices
	StageIndex
	StagePublish
	StageLaunch
	// StageRuntime is storage lost after a normal boot.
	StageRuntime
)

func (stage Stage) String() string {
	switch stage {
	case StageForced:
		return "forced"
	case StageStorage:
		return "storage"
	case StageRecord:
		return "record"
	case StageServices:
		return "services"
	case StageIndex:
		return "index"
	case StagePublish:
		return "publish"
	case StageLaunch:
		return "launch"
	case StageRuntime:
		return "runtime"
	}
	return "none"
}

// MaxBootText bounds the path and reason a recovery records.
const MaxBootText = 64

// BootState is PID 1's mode and, in recovery, why. It changes only forward:
// starting to normal or recovery, and normal to recovery. The first reason
// for recovery is the one kept.
type BootState struct {
	mode                 Mode
	stage                Stage
	path, reason         [MaxBootText]byte
	pathSize, reasonSize uint8
}

func (state *BootState) Mode() Mode   { return state.mode }
func (state *BootState) Stage() Stage { return state.stage }
func (state *BootState) Path() []byte { return state.path[:state.pathSize] }
func (state *BootState) Reason() []byte {
	return state.reason[:state.reasonSize]
}

// Normal enters normal mode from starting, and reports whether it did.
func (state *BootState) Normal() bool {
	if state.mode != ModeStarting {
		return false
	}
	state.mode = ModeNormal
	return true
}

// Recover enters recovery and records where and why, and reports whether
// the mode changed. In recovery already, nothing changes.
func (state *BootState) Recover(stage Stage, path, reason string) bool {
	if state.mode == ModeRecovery {
		return false
	}
	state.mode, state.stage = ModeRecovery, stage
	state.pathSize = uint8(copy(state.path[:], path))
	state.reasonSize = uint8(copy(state.reason[:], reason))
	return true
}

// Line is the one console line that announces the mode.
func (state *BootState) Line() string {
	if state.mode != ModeRecovery {
		return "[MODE] " + state.mode.String() + "\n"
	}
	line := "[MODE] recovery: stage=" + state.stage.String()
	if state.pathSize != 0 {
		line += " path=" + string(state.Path())
	}
	if state.reasonSize != 0 {
		line += " reason=" + string(state.Reason())
	}
	return line + "\n"
}

// IndexKind names which command index may grant a launched program
// authority beyond its standard streams.
type IndexKind uint8

const (
	IndexNone IndexKind = iota
	// IndexBootstrap is the archive's recovery index, for the archive's
	// commands.
	IndexBootstrap
	// IndexSystem is the installed index, for the installed commands, and
	// only while the installed system is published in normal mode.
	IndexSystem
)

// TrustedIndex returns the index that describes the program at path. A
// program elsewhere, such as a copy of a system command in a user directory,
// is described by none, whatever its name.
func TrustedIndex(path []byte, mode Mode, published bool) IndexKind {
	if _, found := execproto.CommandFromBootstrapPath(path); found {
		return IndexBootstrap
	}
	if _, found := execproto.CommandFromSystemPath(path); found && mode == ModeNormal && published {
		return IndexSystem
	}
	return IndexNone
}

// ServicePath is where the executable of a session or storage-independent
// service lives in mode: on the volume in normal mode, in the archive
// otherwise.
func ServicePath(name string, mode Mode) string {
	if mode == ModeNormal {
		return "/system/services/" + name
	}
	return execproto.BootstrapRoot + name
}

// SearchPath is the native shell's search path in mode.
func SearchPath(mode Mode) string {
	if mode == ModeNormal {
		return execproto.NormalPath
	}
	return execproto.RecoveryPath
}
