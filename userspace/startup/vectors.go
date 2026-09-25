package startup

type Variable struct {
	Name  []byte
	Value []byte
}

// VectorSize bounds one encoded argv plus environment pair; each vector is at
// most MaxVector bytes, the width of its GST3 length field.
const (
	VectorSize = 256
	MaxVector  = 255
)

// RestartVariable is set in the environment of a service PID 1 starts again
// in the same boot (ADR 0100).
const RestartVariable = "RESTART"

// EncodeVectors serializes bounded argv and environment vectors independently
// so the shell-to-init protocol can carry the same representation as GST3.
func EncodeVectors(arguments [][]byte, environment []Variable, target *[VectorSize]byte) ([]byte, []byte, bool) {
	if target == nil || len(arguments) == 0 || len(arguments) > MaxArgs || len(environment) > MaxEnv {
		return nil, nil, false
	}
	offset := 1
	target[0] = byte(len(arguments))
	for _, argument := range arguments {
		if len(argument) == 0 || offset+1+len(argument) > MaxVector {
			return nil, nil, false
		}
		target[offset] = byte(len(argument))
		copy(target[offset+1:], argument)
		offset += 1 + len(argument)
	}
	argumentEnd := offset
	if offset >= len(target)-1 {
		return nil, nil, false
	}
	target[offset] = byte(len(environment))
	offset++
	for index, variable := range environment {
		if len(variable.Name) == 0 || len(variable.Name) > 31 || len(variable.Value) > 63 || duplicateVariable(environment[:index], variable.Name) || offset+2+len(variable.Name)+len(variable.Value) > len(target) || offset+2+len(variable.Name)+len(variable.Value)-argumentEnd > MaxVector {
			return nil, nil, false
		}
		for _, value := range variable.Name {
			if value != '_' && (value < 'A' || value > 'Z') && (value < '0' || value > '9') {
				return nil, nil, false
			}
		}
		target[offset], target[offset+1] = byte(len(variable.Name)), byte(len(variable.Value))
		copy(target[offset+2:], variable.Name)
		copy(target[offset+2+len(variable.Name):], variable.Value)
		offset += 2 + len(variable.Name) + len(variable.Value)
	}
	return target[:argumentEnd], target[argumentEnd:offset], true
}

func DecodeVectors(arguments, environment []byte) ([MaxArgs][]byte, [MaxEnv]Variable, bool) {
	var args [MaxArgs][]byte
	var env [MaxEnv]Variable
	if len(arguments) < 1 || arguments[0] == 0 || int(arguments[0]) > MaxArgs {
		return args, env, false
	}
	offset := 1
	for index := 0; index < int(arguments[0]); index++ {
		if offset >= len(arguments) || arguments[offset] == 0 || offset+1+int(arguments[offset]) > len(arguments) {
			return args, env, false
		}
		size := int(arguments[offset])
		args[index] = arguments[offset+1 : offset+1+size]
		offset += 1 + size
	}
	if offset != len(arguments) || len(environment) < 1 || int(environment[0]) > MaxEnv {
		return args, env, false
	}
	offset = 1
	for index := 0; index < int(environment[0]); index++ {
		if offset+2 > len(environment) {
			return args, env, false
		}
		nameSize, valueSize := int(environment[offset]), int(environment[offset+1])
		if nameSize == 0 || offset+2+nameSize+valueSize > len(environment) {
			return args, env, false
		}
		name := environment[offset+2 : offset+2+nameSize]
		for _, value := range name {
			if value != '_' && (value < 'A' || value > 'Z') && (value < '0' || value > '9') {
				return args, env, false
			}
		}
		if duplicateVariable(env[:index], name) {
			return args, env, false
		}
		env[index] = Variable{Name: name, Value: environment[offset+2+nameSize : offset+2+nameSize+valueSize]}
		offset += 2 + nameSize + valueSize
	}
	return args, env, offset == len(environment)
}

// ArgCount is the number of arguments after the command name. Decode has
// validated the vector, so its count byte is read without decoding it again.
func (data Data) ArgCount() int {
	if len(data.Arguments) == 0 || data.Arguments[0] == 0 {
		return 0
	}
	return int(data.Arguments[0]) - 1
}

func (data Data) Arg(index int) ([]byte, bool) {
	arguments, _, ok := DecodeVectors(data.Arguments, data.Environment)
	if !ok || index < 0 || index >= int(data.Arguments[0]) {
		return nil, false
	}
	return arguments[index], true
}

func (data Data) Env(name string) ([]byte, bool) {
	_, environment, ok := DecodeVectors(data.Arguments, data.Environment)
	if !ok {
		return nil, false
	}
	for index := 0; index < int(data.Environment[0]); index++ {
		if equal(environment[index].Name, []byte(name)) {
			return environment[index].Value, true
		}
	}
	return nil, false
}

func duplicateVariable(environment []Variable, name []byte) bool {
	for _, variable := range environment {
		if equal(variable.Name, name) {
			return true
		}
	}
	return false
}
