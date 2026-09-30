package abi

import (
	"encoding/binary"

	"goos/kernel/compat/linux/elf"
	"goos/kernel/mm/vm"
)

const (
	MaxInitialStackSize = 64 << 10
	MaxArguments        = 256
	MaxEnvironment      = 256

	SyntheticUID = uint64(1000)
	SyntheticGID = uint64(1000)
	ClockTicks   = uint64(100)

	auxNull      = uint64(0)
	auxPHDR      = uint64(3)
	auxPHENT     = uint64(4)
	auxPHNUM     = uint64(5)
	auxPageSize  = uint64(6)
	auxEntry     = uint64(9)
	auxUID       = uint64(11)
	auxEUID      = uint64(12)
	auxGID       = uint64(13)
	auxEGID      = uint64(14)
	auxPlatform  = uint64(15)
	auxClockTick = uint64(17)
	auxSecure    = uint64(23)
	auxRandom    = uint64(25)
	auxExecFN    = uint64(31)

	// auxiliaryCount is the number of auxv entries, the terminator included,
	// of a stack without AT_RANDOM.
	auxiliaryCount = 14

	// RandomSize is the number of bytes AT_RANDOM points at.
	RandomSize = 16
)

var platform = [...]byte{'x', '8', '6', '_', '6', '4'}

// Startup is the stable, validated launch view used to build Linux argv and
// environment. userspace/startup.Data implements it without exposing GST4 to
// the Linux process.
type Startup interface {
	Command() []byte
	Args() int
	Arg(index int) ([]byte, bool)
	EnvCount() int
	EnvAt(index int) ([]byte, []byte)
}

type rawEnvironment interface {
	Environment(index int) ([]byte, bool)
}

// InitialStack is a contiguous image of the occupied top of a Linux stack.
// Data aliases the caller's target buffer and covers [Pointer, Top).
type InitialStack struct {
	Pointer uint64
	Top     uint64
	Data    []byte
}

// StackError is a bounded failure reason for initial-stack construction.
type StackError uint8

const (
	StackOK StackError = iota
	StackInvalidInput
	StackTooManyArguments
	StackTooManyEnvironment
	StackStringInvalid
	StackTooLarge
)

// BuildInitialStack writes a Linux x86_64 process-entry stack into target.
// It includes argc, argv, envp and the required static-ELF auxv entries.
//
// random is 16 bytes from a hardware generator, which the stack carries
// behind AT_RANDOM. A caller without such a source passes nil and the entry
// is left out: a constant or a guessable value there would be read as a
// secret. The C library then derives its stack-protector canary from
// addresses, as it does on a Linux kernel too old to supply the entry.
func BuildInitialStack(target []byte, top uint64, image *elf.Image, startup Startup, random *[RandomSize]byte) (InitialStack, StackError) {
	if startup == nil || image == nil || top == 0 || top%16 != 0 || image.Entry == 0 || image.ProgramHeaderAddress == 0 ||
		image.ProgramHeaderCount == 0 || image.ProgramHeaderCount > elf.MaxProgramHeaders {
		return InitialStack{}, StackInvalidInput
	}
	arguments, environment := startup.Args(), startup.EnvCount()
	if arguments < 0 || arguments > MaxArguments {
		return InitialStack{}, StackTooManyArguments
	}
	if environment < 0 || environment > MaxEnvironment {
		return InitialStack{}, StackTooManyEnvironment
	}
	command := startup.Command()
	if !validString(command, true) {
		return InitialStack{}, StackStringInvalid
	}
	strings := 0
	if !addStringSize(&strings, command) || !addStringSize(&strings, platform[:]) {
		return InitialStack{}, StackTooLarge
	}
	for index := 0; index < arguments; index++ {
		argument, ok := startup.Arg(index)
		if !ok || !validString(argument, false) {
			return InitialStack{}, StackStringInvalid
		}
		if !addStringSize(&strings, argument) {
			return InitialStack{}, StackTooLarge
		}
	}
	for index := 0; index < environment; index++ {
		if raw, ok := startup.(rawEnvironment); ok {
			variable, found := raw.Environment(index)
			if !found || !validString(variable, false) {
				return InitialStack{}, StackStringInvalid
			}
			if !addStringSize(&strings, variable) {
				return InitialStack{}, StackTooLarge
			}
			continue
		}
		name, value := startup.EnvAt(index)
		if !validEnvironment(name, value) {
			return InitialStack{}, StackStringInvalid
		}
		if !addEnvironmentSize(&strings, name, value) {
			return InitialStack{}, StackTooLarge
		}
	}
	entries := auxiliaryCount
	if random != nil {
		entries++
		if !addSize(&strings, RandomSize) {
			return InitialStack{}, StackTooLarge
		}
	}
	words := 1 + arguments + 1 + environment + 1
	vectorBytes := words*8 + entries*16
	if vectorBytes > MaxInitialStackSize || strings > MaxInitialStackSize-vectorBytes {
		return InitialStack{}, StackTooLarge
	}
	needed := vectorBytes + strings
	stackSize := (needed + 15) &^ 15
	if stackSize > MaxInitialStackSize || stackSize > len(target) || uint64(stackSize) > top {
		return InitialStack{}, StackTooLarge
	}
	pointer := top - uint64(stackSize)
	data := target[len(target)-stackSize:]
	clear(data)
	binary.LittleEndian.PutUint64(data[0:8], uint64(arguments))
	argvAt := 8
	envpAt := argvAt + (arguments+1)*8
	auxvAt := envpAt + (environment+1)*8
	cursor := vectorBytes
	for index := 0; index < arguments; index++ {
		argument, ok := startup.Arg(index)
		if !ok || !validString(argument, false) {
			return InitialStack{}, StackInvalidInput
		}
		binary.LittleEndian.PutUint64(data[argvAt+index*8:], pointer+uint64(cursor))
		if cursor, ok = putString(data, cursor, argument); !ok {
			return InitialStack{}, StackInvalidInput
		}
	}
	for index := 0; index < environment; index++ {
		if raw, exists := startup.(rawEnvironment); exists {
			variable, found := raw.Environment(index)
			if !found || !validString(variable, false) {
				return InitialStack{}, StackInvalidInput
			}
			binary.LittleEndian.PutUint64(data[envpAt+index*8:], pointer+uint64(cursor))
			var ok bool
			if cursor, ok = putString(data, cursor, variable); !ok {
				return InitialStack{}, StackInvalidInput
			}
			continue
		}
		name, value := startup.EnvAt(index)
		if !validEnvironment(name, value) {
			return InitialStack{}, StackInvalidInput
		}
		binary.LittleEndian.PutUint64(data[envpAt+index*8:], pointer+uint64(cursor))
		var ok bool
		if cursor, ok = putEnvironment(data, cursor, name, value); !ok {
			return InitialStack{}, StackInvalidInput
		}
	}
	execAddress := pointer + uint64(cursor)
	var ok bool
	if cursor, ok = putString(data, cursor, command); !ok {
		return InitialStack{}, StackInvalidInput
	}
	platformAddress := pointer + uint64(cursor)
	if cursor, ok = putString(data, cursor, platform[:]); !ok {
		return InitialStack{}, StackInvalidInput
	}
	// The random bytes follow the strings. With no source the entry that
	// would name them is the terminator, and the pair after it is not written.
	randomType, randomAddress := auxNull, uint64(0)
	if random != nil {
		if cursor < 0 || RandomSize > len(data)-cursor {
			return InitialStack{}, StackInvalidInput
		}
		randomType, randomAddress = auxRandom, pointer+uint64(cursor)
		copy(data[cursor:], random[:])
	}
	auxiliary := [...]uint64{
		auxPHDR, image.ProgramHeaderAddress,
		auxPHENT, elf.ProgramHeaderSize,
		auxPHNUM, uint64(image.ProgramHeaderCount),
		auxPageSize, vm.PageSize,
		auxEntry, image.Entry,
		auxUID, SyntheticUID,
		auxEUID, SyntheticUID,
		auxGID, SyntheticGID,
		auxEGID, SyntheticGID,
		auxSecure, 0,
		auxClockTick, ClockTicks,
		auxPlatform, platformAddress,
		auxExecFN, execAddress,
		randomType, randomAddress,
		auxNull, 0,
	}
	for index := 0; index < entries*2; index++ {
		binary.LittleEndian.PutUint64(data[auxvAt+index*8:], auxiliary[index])
	}
	return InitialStack{Pointer: pointer, Top: top, Data: data}, StackOK
}

func validString(value []byte, nonempty bool) bool {
	if nonempty && len(value) == 0 {
		return false
	}
	for _, character := range value {
		if character == 0 {
			return false
		}
	}
	return true
}

func validEnvironment(name, value []byte) bool {
	if !validString(name, true) || !validString(value, false) {
		return false
	}
	for _, character := range name {
		if character == '=' {
			return false
		}
	}
	return true
}

func addSize(total *int, amount int) bool {
	if amount < 0 || *total > MaxInitialStackSize-amount {
		return false
	}
	*total += amount
	return true
}

func addStringSize(total *int, value []byte) bool {
	return len(value) < MaxInitialStackSize && addSize(total, len(value)+1)
}

func addEnvironmentSize(total *int, name, value []byte) bool {
	return len(name) < MaxInitialStackSize && len(value) < MaxInitialStackSize &&
		addSize(total, len(name)+1) && addSize(total, len(value)+1)
}

func putString(target []byte, cursor int, value []byte) (int, bool) {
	if cursor < 0 || cursor > len(target) || len(value) >= len(target)-cursor {
		return cursor, false
	}
	cursor += copy(target[cursor:], value)
	target[cursor] = 0
	return cursor + 1, true
}

func putEnvironment(target []byte, cursor int, name, value []byte) (int, bool) {
	if cursor < 0 || cursor > len(target) || len(name) >= len(target)-cursor {
		return cursor, false
	}
	cursor += copy(target[cursor:], name)
	target[cursor] = '='
	cursor++
	return putString(target, cursor, value)
}
