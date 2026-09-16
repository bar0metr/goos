package startup

const Size = 128

type Data struct {
	Command        byte
	CWD, Arguments []byte
}

func Encode(command byte, cwd, arguments []byte, target *[Size]byte) ([]byte, bool) {
	if target == nil || command == 0 || len(cwd) == 0 || len(cwd) > 96 || len(arguments) > 120 || 4+len(cwd)+len(arguments) > Size {
		return nil, false
	}
	for index := range target {
		target[index] = 0
	}
	target[0], target[1] = byte(len(cwd)), byte(len(arguments))
	target[2] = command
	copy(target[4:], cwd)
	copy(target[4+len(cwd):], arguments)
	return target[:4+len(cwd)+len(arguments)], true
}

func Decode(source []byte) (Data, bool) {
	if len(source) != Size || source[2] == 0 || source[3] != 0 {
		return Data{}, false
	}
	cwdSize, argumentSize := int(source[0]), int(source[1])
	if cwdSize == 0 || 4+cwdSize+argumentSize > len(source) {
		return Data{}, false
	}
	return Data{Command: source[2], CWD: source[4 : 4+cwdSize], Arguments: source[4+cwdSize : 4+cwdSize+argumentSize]}, true
}
