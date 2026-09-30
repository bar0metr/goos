package linux

import "goos/kernel/compat/linux/abi"

const (
	MaxExecArgs        = 256
	MaxExecEnv         = 256
	MaxExecStringBytes = 32 << 10
	MaxExecTotalBytes  = 64 << 10
)

type execSpan struct {
	offset uint32
	size   uint32
}

// ExecSnapshot owns the path, arguments, and environment copied from the old
// address space before image preparation starts.
type ExecSnapshot struct {
	data        [MaxExecTotalBytes]byte
	arguments   [MaxExecArgs]execSpan
	environment [MaxExecEnv]execSpan
	command     execSpan
	used        uint32
	argCount    uint16
	envCount    uint16
}

func (snapshot *ExecSnapshot) Reset() {
	if snapshot == nil {
		return
	}
	clear(snapshot.data[:snapshot.used])
	snapshot.command = execSpan{}
	snapshot.used, snapshot.argCount, snapshot.envCount = 0, 0, 0
}

func (snapshot *ExecSnapshot) SetCommand(command []byte) abi.Errno {
	if snapshot == nil || len(command) == 0 {
		return abi.ENOENT
	}
	span, errno := snapshot.append(command)
	if errno == 0 {
		snapshot.command = span
	}
	return errno
}

func (snapshot *ExecSnapshot) AddArgument(argument []byte) abi.Errno {
	if snapshot == nil || int(snapshot.argCount) == len(snapshot.arguments) {
		return abi.E2BIG
	}
	span, errno := snapshot.append(argument)
	if errno == 0 {
		snapshot.arguments[snapshot.argCount] = span
		snapshot.argCount++
	}
	return errno
}

func (snapshot *ExecSnapshot) AddEnvironment(variable []byte) abi.Errno {
	if snapshot == nil || int(snapshot.envCount) == len(snapshot.environment) {
		return abi.E2BIG
	}
	span, errno := snapshot.append(variable)
	if errno == 0 {
		snapshot.environment[snapshot.envCount] = span
		snapshot.envCount++
	}
	return errno
}

func (snapshot *ExecSnapshot) append(value []byte) (execSpan, abi.Errno) {
	if len(value) > MaxExecStringBytes || len(value)+1 > MaxExecTotalBytes-int(snapshot.used) {
		return execSpan{}, abi.E2BIG
	}
	start := snapshot.used
	copy(snapshot.data[start:], value)
	snapshot.used += uint32(len(value) + 1)
	return execSpan{offset: start, size: uint32(len(value))}, 0
}

func (snapshot *ExecSnapshot) value(span execSpan) []byte {
	end := uint64(span.offset) + uint64(span.size)
	if snapshot == nil || end > uint64(snapshot.used) {
		return nil
	}
	return snapshot.data[span.offset:end]
}

func (snapshot *ExecSnapshot) Command() []byte { return snapshot.value(snapshot.command) }
func (snapshot *ExecSnapshot) Args() int       { return int(snapshot.argCount) }

func (snapshot *ExecSnapshot) Arg(index int) ([]byte, bool) {
	if snapshot == nil || index < 0 || index >= int(snapshot.argCount) {
		return nil, false
	}
	return snapshot.value(snapshot.arguments[index]), true
}

func (snapshot *ExecSnapshot) EnvCount() int { return int(snapshot.envCount) }

// Environment returns one raw envp string. Linux does not require an equals
// sign, so exec snapshots preserve the byte sequence exactly.
func (snapshot *ExecSnapshot) Environment(index int) ([]byte, bool) {
	if snapshot == nil || index < 0 || index >= int(snapshot.envCount) {
		return nil, false
	}
	return snapshot.value(snapshot.environment[index]), true
}

func (snapshot *ExecSnapshot) EnvAt(index int) ([]byte, []byte) {
	if snapshot == nil || index < 0 || index >= int(snapshot.envCount) {
		return nil, nil
	}
	variable := snapshot.value(snapshot.environment[index])
	for at, character := range variable {
		if character == '=' {
			return variable[:at], variable[at+1:]
		}
	}
	return nil, nil
}

const (
	ExecStageStat uint8 = iota + 1
	ExecStageOpen
	ExecStagePrepare
	ExecStageSeek
	ExecStageFill
	ExecStageClose
	ExecStageCloseDescriptors
)
