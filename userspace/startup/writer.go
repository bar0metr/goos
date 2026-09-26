package startup

import (
	"encoding/binary"
	"goos/kernel/handle"
)

// Writer builds a startup object in place, without allocating. The counts are
// fixed when it is made, because they place the string area; the command, the
// working directory, and every argument, variable, and handle are then added
// in order and Finish validates the result.
type Writer struct {
	target      []byte
	arguments   int
	environment int
	handles     int
	added       [3]int
	cursor      int
	failed      bool
	command     bool
	cwd         bool
}

// NewWriter starts an object with the given counts in target.
func NewWriter(target []byte, arguments, environment, handles int) Writer {
	writer := Writer{target: target, arguments: arguments, environment: environment, handles: handles}
	if arguments <= 0 || environment < 0 || handles < 0 || handles > MaxHandles || arguments > MaxSize || environment > MaxSize {
		writer.failed = true
		return writer
	}
	writer.cursor = HeaderSize + (2+arguments+environment)*entrySize + handles*handleSize
	if len(target) > MaxSize {
		writer.target = target[:MaxSize]
	}
	if writer.cursor > len(writer.target) {
		writer.failed = true
		return writer
	}
	clear(writer.target[:writer.cursor])
	copy(writer.target[:4], magic[:])
	binary.LittleEndian.PutUint16(writer.target[4:6], Version)
	binary.LittleEndian.PutUint16(writer.target[6:8], uint16(handles))
	binary.LittleEndian.PutUint32(writer.target[12:16], uint32(arguments))
	binary.LittleEndian.PutUint32(writer.target[16:20], uint32(environment))
	binary.LittleEndian.PutUint32(writer.target[20:24], uint32(writer.cursor))
	return writer
}

// Command sets the name or path the program is started by.
func (writer *Writer) Command(command []byte) {
	writer.command = writer.put(0, command, nil)
}

// CWD sets the working directory.
func (writer *Writer) CWD(cwd []byte) {
	writer.cwd = writer.put(1, cwd, nil)
}

// Argument adds the next argument; the first is argv[0].
func (writer *Writer) Argument(argument []byte) {
	if writer.added[0] == writer.arguments {
		writer.failed = true
		return
	}
	writer.put(2+writer.added[0], argument, nil)
	writer.added[0]++
}

// Variable adds the next environment variable.
func (writer *Writer) Variable(name, value []byte) {
	if writer.added[1] == writer.environment || len(name) == 0 {
		writer.failed = true
		return
	}
	for _, character := range name {
		if character == '=' {
			writer.failed = true
			return
		}
	}
	writer.put(2+writer.arguments+writer.added[1], name, value)
	writer.added[1]++
}

// Entry adds the next environment variable given as "NAME=VALUE".
func (writer *Writer) Entry(variable []byte) {
	name, value, ok := split(variable)
	if !ok {
		writer.failed = true
		return
	}
	writer.Variable(name, value)
}

// Handle adds the next handle binding; id is zero in a launch template.
func (writer *Writer) Handle(name []byte, id handle.ID) {
	if writer.failed || writer.added[2] == writer.handles {
		writer.failed = true
		return
	}
	offset := writer.cursor
	if !writer.append(name) {
		return
	}
	at := HeaderSize + (2+writer.arguments+writer.environment)*entrySize + writer.added[2]*handleSize
	binary.LittleEndian.PutUint32(writer.target[at:at+4], uint32(offset))
	binary.LittleEndian.PutUint32(writer.target[at+4:at+8], uint32(len(name)))
	binary.LittleEndian.PutUint64(writer.target[at+8:at+16], uint64(id))
	writer.added[2]++
}

// Finish completes the object and validates it as a template.
func (writer *Writer) Finish() ([]byte, bool) {
	if writer.failed || !writer.command || !writer.cwd || writer.added[0] != writer.arguments || writer.added[1] != writer.environment || writer.added[2] != writer.handles {
		return nil, false
	}
	binary.LittleEndian.PutUint32(writer.target[8:12], uint32(writer.cursor))
	object := writer.target[:writer.cursor]
	if _, ok := DecodeTemplate(object); !ok {
		return nil, false
	}
	return object, true
}

// put writes string index as first, followed by '=' and second when second
// is not nil.
func (writer *Writer) put(index int, first, second []byte) bool {
	if writer.failed {
		return false
	}
	offset := writer.cursor
	if !writer.append(first) {
		return false
	}
	if second != nil && (!writer.append(equals[:]) || !writer.append(second)) {
		return false
	}
	at := HeaderSize + index*entrySize
	binary.LittleEndian.PutUint32(writer.target[at:at+4], uint32(offset))
	binary.LittleEndian.PutUint32(writer.target[at+4:at+8], uint32(writer.cursor-offset))
	return true
}

var equals = [1]byte{'='}

func (writer *Writer) append(value []byte) bool {
	if len(value) > len(writer.target)-writer.cursor {
		writer.failed = true
		return false
	}
	writer.cursor += copy(writer.target[writer.cursor:], value)
	return true
}
