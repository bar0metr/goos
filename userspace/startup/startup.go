package startup

import (
	"encoding/binary"
	"goos/kernel/handle"
)

const (
	Size       = 128
	Version    = 2
	MaxHandles = 7
	MaxName    = 15
	MaxArgs    = 8
	MaxEnv     = 4
	headerSize = 12
)

var magic = [4]byte{'G', 'S', 'T', '2'}

type Handle struct {
	Name []byte
	ID   handle.ID
}

type Data struct {
	Command     []byte
	CWD         []byte
	Arguments   []byte
	Environment []byte
	Handles     [MaxHandles]Handle
	HandleCount uint8
}

// Encode serializes the versioned native process startup ABI. A zero handle ID
// is permitted only in a launch template and must be resolved by BindHandles
// before the child can read the block.
func Encode(command, cwd, arguments, environment []byte, handles []Handle, target *[Size]byte) ([]byte, bool) {
	if target == nil || len(command) == 0 || len(command) > 32 || len(cwd) == 0 || len(cwd) > 96 || len(arguments) > 255 || len(environment) > 255 || len(handles) > MaxHandles {
		return nil, false
	}
	if _, _, ok := DecodeVectors(arguments, environment); !ok {
		return nil, false
	}
	size := headerSize + len(command) + len(cwd) + len(arguments) + len(environment)
	for index, binding := range handles {
		if len(binding.Name) == 0 || len(binding.Name) > MaxName || duplicateName(handles[:index], binding.Name) {
			return nil, false
		}
		size += 9 + len(binding.Name)
	}
	if size > Size {
		return nil, false
	}
	for index := range target {
		target[index] = 0
	}
	copy(target[:4], magic[:])
	target[4], target[5] = Version, byte(len(handles))
	target[6], target[7], target[8], target[9] = byte(len(command)), byte(len(cwd)), byte(len(arguments)), byte(len(environment))
	binary.LittleEndian.PutUint16(target[10:12], uint16(size))
	offset := headerSize
	copy(target[offset:], command)
	offset += len(command)
	copy(target[offset:], cwd)
	offset += len(cwd)
	copy(target[offset:], arguments)
	offset += len(arguments)
	copy(target[offset:], environment)
	offset += len(environment)
	for _, binding := range handles {
		target[offset] = byte(len(binding.Name))
		binary.LittleEndian.PutUint64(target[offset+1:offset+9], uint64(binding.ID))
		copy(target[offset+9:], binding.Name)
		offset += 9 + len(binding.Name)
	}
	return target[:size], true
}

// BindHandles replaces the zero IDs in a launch template with the handles
// installed by the kernel, preserving the capability-manifest order.
func BindHandles(template []byte, installed []handle.ID, target *[Size]byte) ([]byte, bool) {
	data, size, ok := decode(template, true)
	if !ok || int(data.HandleCount) != len(installed) {
		return nil, false
	}
	bindings := data.Handles[:data.HandleCount]
	for index := range bindings {
		if bindings[index].ID != 0 || installed[index] == 0 {
			return nil, false
		}
		bindings[index].ID = installed[index]
	}
	encoded, ok := Encode(data.Command, data.CWD, data.Arguments, data.Environment, bindings, target)
	if !ok || len(encoded) != size {
		return nil, false
	}
	return encoded, true
}

func Decode(source []byte) (Data, bool) {
	data, _, ok := decode(source, false)
	return data, ok
}

func (data Data) Handle(name string) (handle.ID, bool) {
	for index := 0; index < int(data.HandleCount); index++ {
		if equal(data.Handles[index].Name, []byte(name)) {
			return data.Handles[index].ID, true
		}
	}
	return 0, false
}

func decode(source []byte, allowUnbound bool) (Data, int, bool) {
	if len(source) < headerSize || !equal(source[:4], magic[:]) || source[4] != Version {
		return Data{}, 0, false
	}
	handleCount := int(source[5])
	commandSize, cwdSize, argumentSize, environmentSize := int(source[6]), int(source[7]), int(source[8]), int(source[9])
	size := int(binary.LittleEndian.Uint16(source[10:12]))
	if handleCount > MaxHandles || commandSize == 0 || commandSize > 32 || cwdSize == 0 || size < headerSize+commandSize+cwdSize+argumentSize+environmentSize || size > len(source) || size > Size {
		return Data{}, 0, false
	}
	offset := headerSize
	data := Data{Command: source[offset : offset+commandSize], HandleCount: uint8(handleCount)}
	offset += commandSize
	data.CWD = source[offset : offset+cwdSize]
	offset += cwdSize
	data.Arguments = source[offset : offset+argumentSize]
	offset += argumentSize
	data.Environment = source[offset : offset+environmentSize]
	offset += environmentSize
	if _, _, ok := DecodeVectors(data.Arguments, data.Environment); !ok {
		return Data{}, 0, false
	}
	for index := 0; index < handleCount; index++ {
		if offset+9 > size {
			return Data{}, 0, false
		}
		nameSize := int(source[offset])
		id := handle.ID(binary.LittleEndian.Uint64(source[offset+1 : offset+9]))
		if nameSize == 0 || nameSize > MaxName || offset+9+nameSize > size || id == 0 && !allowUnbound {
			return Data{}, 0, false
		}
		name := source[offset+9 : offset+9+nameSize]
		for previous := 0; previous < index; previous++ {
			if equal(data.Handles[previous].Name, name) {
				return Data{}, 0, false
			}
		}
		data.Handles[index] = Handle{Name: name, ID: id}
		offset += 9 + nameSize
	}
	if offset != size {
		return Data{}, 0, false
	}
	return data, size, true
}

func duplicateName(handles []Handle, name []byte) bool {
	for _, binding := range handles {
		if equal(binding.Name, name) {
			return true
		}
	}
	return false
}

func equal(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
