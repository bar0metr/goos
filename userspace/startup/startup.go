package startup

const Size = 128

type Data struct {
	Command        []byte
	CWD, Arguments []byte
}

func Encode(command, cwd, arguments []byte, target *[Size]byte) ([]byte, bool) {
	if target == nil || len(command) == 0 || len(command) > 32 || len(cwd) == 0 || len(cwd) > 96 || len(arguments) > 120 || 4+len(command)+len(cwd)+len(arguments) > Size {
		return nil, false
	}
	for index := range target {
		target[index] = 0
	}
	target[0], target[1], target[2] = byte(len(command)), byte(len(cwd)), byte(len(arguments))
	copy(target[4:], command)
	copy(target[4+len(command):], cwd)
	copy(target[4+len(command)+len(cwd):], arguments)
	return target[:4+len(command)+len(cwd)+len(arguments)], true
}

func Decode(source []byte) (Data, bool) {
	if len(source) != Size || source[0] == 0 || source[3] != 0 {
		return Data{}, false
	}
	commandSize, cwdSize, argumentSize := int(source[0]), int(source[1]), int(source[2])
	if commandSize > 32 || cwdSize == 0 || 4+commandSize+cwdSize+argumentSize > len(source) {
		return Data{}, false
	}
	cwdStart := 4 + commandSize
	argumentsStart := cwdStart + cwdSize
	return Data{Command: source[4:cwdStart], CWD: source[cwdStart:argumentsStart], Arguments: source[argumentsStart : argumentsStart+argumentSize]}, true
}
