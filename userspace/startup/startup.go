// Package startup defines GST4, the native process startup object (ADR 0109).
//
// A process starts with one read-only object that names the program, its
// working directory, its arguments, its environment, and the handles it was
// given. The object is variable-length and bounded by bytes, not by counts:
//
//	offset  size  field
//	0       4     magic "GST4"
//	4       2     version (4)
//	6       2     handle count
//	8       4     total size of the object
//	12      4     argument count (at least 1: argv[0])
//	16      4     environment count
//	20      4     offset of the string area
//	24      8     reserved, zero
//	32            string table: (2 + arguments + environment) entries of
//	              {u32 offset, u32 length} — the command, the working
//	              directory, each argument, each "NAME=VALUE" variable
//	              handle table: entries of {u32 name offset, u32 name length,
//	              u64 handle ID}
//	              string area, up to the total size
//
// All fields are little-endian. The string area starts right after the handle
// table, every string lies inside it, and no string contains a zero byte, so
// a future execve can pass them on as C strings.
package startup

import (
	"encoding/binary"
	"goos/internal/limits"
	"goos/kernel/handle"
	"goos/kernel/kcall"
)

const (
	Version = 5
	// MaxSize bounds a whole startup object. It is the kernel's limit; see
	// kcall.MaxStartupSize for why it has this value.
	MaxSize = kcall.MaxStartupSize
	// MaxString bounds one argument or one variable.
	MaxString = MaxSize / 2
	// MaxHandles bounds the handle table.
	MaxHandles = 64
	// MaxHandleName bounds the name a handle is bound to.
	MaxHandleName = 63
	HeaderSize    = 32
	entrySize     = 8
	handleSize    = 16

	// flagCredentials, in reserved byte 24, marks an object whose trailing
	// block carries the Linux credential snapshot (ADR 0234).
	flagCredentials = 1
)

// CredentialsSize is the fixed encoded size of a Credentials block.
const CredentialsSize = 4 + 4 + 8 + 16 + 1 + limits.SupplementaryGroupsMax*4

// Credentials is the bounded Linux credential snapshot a launch carries: the
// real/effective/saved UID/GID (equal at launch), the supplementary GIDs, the
// stable account ID and the identity database epoch (ADR 0234). A zero value
// is the identity-free legacy launch, which the kernel reports as UID/GID 0
// for getuid and so on. It grants no authority.
type Credentials struct {
	UID        uint32
	GID        uint32
	AccountID  uint64
	Epoch      [16]byte
	Groups     [limits.SupplementaryGroupsMax]uint32
	GroupCount uint8
}

// Valid reports whether the credential is well formed. A zero credential is
// valid and means an identity-free launch.
func (credentials Credentials) Valid() bool {
	if credentials.GroupCount > limits.SupplementaryGroupsMax {
		return false
	}
	for index := 1; index < int(credentials.GroupCount); index++ {
		if credentials.Groups[index-1] >= credentials.Groups[index] {
			return false
		}
	}
	if credentials.AccountID == 0 {
		return credentials.UID == 0 && credentials.GID == 0 && credentials.Epoch == [16]byte{}
	}
	return credentials.UID != 0 && credentials.GID != 0 && credentials.Epoch != [16]byte{}
}

func encodeCredentials(target []byte, credentials Credentials) {
	binary.LittleEndian.PutUint32(target[0:], credentials.UID)
	binary.LittleEndian.PutUint32(target[4:], credentials.GID)
	binary.LittleEndian.PutUint64(target[8:], credentials.AccountID)
	copy(target[16:32], credentials.Epoch[:])
	target[32] = credentials.GroupCount
	offset := 33
	for index := 0; index < limits.SupplementaryGroupsMax; index++ {
		binary.LittleEndian.PutUint32(target[offset:], credentials.Groups[index])
		offset += 4
	}
}

func decodeCredentials(source []byte) (Credentials, bool) {
	var credentials Credentials
	if len(source) != CredentialsSize {
		return credentials, false
	}
	credentials.UID = binary.LittleEndian.Uint32(source[0:])
	credentials.GID = binary.LittleEndian.Uint32(source[4:])
	credentials.AccountID = binary.LittleEndian.Uint64(source[8:])
	copy(credentials.Epoch[:], source[16:32])
	credentials.GroupCount = source[32]
	offset := 33
	for index := 0; index < limits.SupplementaryGroupsMax; index++ {
		credentials.Groups[index] = binary.LittleEndian.Uint32(source[offset:])
		offset += 4
	}
	if !credentials.Valid() {
		return Credentials{}, false
	}
	return credentials, true
}

func zero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

var magic = [4]byte{'G', 'S', 'T', '4'}

// RestartVariable is set in the environment of a service PID 1 starts again
// in the same boot (ADR 0100).
const RestartVariable = "RESTART"

// Data is a validated startup object. It aliases the bytes it was decoded
// from.
type Data struct {
	source         []byte
	arguments      int
	environment    int
	handles        int
	handleTable    int
	stringArea     int
	unboundAllowed bool
	credentials    bool
}

// Size reads the total size a startup object's header declares, or reports
// false if the header is not a GST4 header.
func Size(header []byte) (int, bool) {
	if len(header) < HeaderSize || !equal(header[:4], magic[:]) || binary.LittleEndian.Uint16(header[4:6]) != Version {
		return 0, false
	}
	size := binary.LittleEndian.Uint32(header[8:12])
	if size < HeaderSize || size > uint32(MaxSize) {
		return 0, false
	}
	return int(size), true
}

// Decode validates a bound startup object: every handle names a handle.
func Decode(source []byte) (Data, bool) { return decode(source, false) }

// DecodeTemplate validates a launch template, whose handle IDs may still be
// zero until the kernel binds them.
func DecodeTemplate(source []byte) (Data, bool) { return decode(source, true) }

func decode(source []byte, allowUnbound bool) (Data, bool) {
	size, ok := Size(source)
	withCredentials := source[24] == flagCredentials
	if !ok || size > len(source) || source[24] > flagCredentials || !zero(source[25:32]) {
		return Data{}, false
	}
	source = source[:size]
	arguments := uint64(binary.LittleEndian.Uint32(source[12:16]))
	environment := uint64(binary.LittleEndian.Uint32(source[16:20]))
	handles := uint64(binary.LittleEndian.Uint16(source[6:8]))
	if arguments == 0 || handles > MaxHandles {
		return Data{}, false
	}
	// The counts are bounded by the size before anything is multiplied, so
	// the table sizes below cannot overflow.
	if arguments > uint64(size) || environment > uint64(size) {
		return Data{}, false
	}
	handleTable := HeaderSize + (2+arguments+environment)*entrySize
	stringArea := handleTable + handles*handleSize
	if uint64(binary.LittleEndian.Uint32(source[20:24])) != stringArea || stringArea > uint64(size) {
		return Data{}, false
	}
	// A credential block, when present, trails the string area.
	if withCredentials {
		if size-CredentialsSize < int(stringArea) {
			return Data{}, false
		}
		if _, ok := decodeCredentials(source[size-CredentialsSize:]); !ok {
			return Data{}, false
		}
	}
	data := Data{source: source, arguments: int(arguments), environment: int(environment), handles: int(handles), handleTable: int(handleTable), stringArea: int(stringArea), unboundAllowed: allowUnbound, credentials: withCredentials}
	command, ok := data.entry(0)
	if !ok || len(command) == 0 || len(command) > limits.PathMax {
		return Data{}, false
	}
	cwd, ok := data.entry(1)
	if !ok || !absolute(cwd) {
		return Data{}, false
	}
	for index := 0; index < data.arguments; index++ {
		argument, ok := data.entry(2 + index)
		if !ok || len(argument) > MaxString || index == 0 && len(argument) == 0 {
			return Data{}, false
		}
	}
	for index := 0; index < data.environment; index++ {
		variable, ok := data.entry(2 + data.arguments + index)
		if !ok || len(variable) > MaxString {
			return Data{}, false
		}
		name, _, ok := split(variable)
		if !ok {
			return Data{}, false
		}
		for previous := 0; previous < index; previous++ {
			if other, _ := data.EnvAt(previous); equal(other, name) {
				return Data{}, false
			}
		}
	}
	for index := 0; index < data.handles; index++ {
		name, id, ok := data.handleEntry(index)
		if !ok || len(name) == 0 || len(name) > MaxHandleName || id == 0 && !allowUnbound {
			return Data{}, false
		}
		for previous := 0; previous < index; previous++ {
			if other, _ := data.HandleAt(previous); equal(other, name) {
				return Data{}, false
			}
		}
	}
	return data, true
}

// entry returns string index of the string table, checked against the string
// area and for zero bytes.
func (data Data) entry(index int) ([]byte, bool) {
	at := HeaderSize + index*entrySize
	return data.span(binary.LittleEndian.Uint32(data.source[at:at+4]), binary.LittleEndian.Uint32(data.source[at+4:at+8]))
}

func (data Data) handleEntry(index int) ([]byte, handle.ID, bool) {
	at := data.handleTable + index*handleSize
	name, ok := data.span(binary.LittleEndian.Uint32(data.source[at:at+4]), binary.LittleEndian.Uint32(data.source[at+4:at+8]))
	return name, handle.ID(binary.LittleEndian.Uint64(data.source[at+8 : at+16])), ok
}

func (data Data) span(offset, length uint32) ([]byte, bool) {
	start, end := uint64(offset), uint64(offset)+uint64(length)
	if start < uint64(data.stringArea) || end > uint64(len(data.source)) {
		return nil, false
	}
	value := data.source[start:end]
	for _, character := range value {
		if character == 0 {
			return nil, false
		}
	}
	return value, true
}

// Bytes returns the whole object.
func (data Data) Bytes() []byte { return data.source }

// Credentials returns the Linux credential snapshot the object carries, and
// whether one is present. An object without one is an identity-free launch.
func (data Data) Credentials() (Credentials, bool) {
	if !data.credentials || len(data.source) < CredentialsSize {
		return Credentials{}, false
	}
	return decodeCredentials(data.source[len(data.source)-CredentialsSize:])
}

// Command is the name or path the program was started by.
func (data Data) Command() []byte { value, _ := data.at(0); return value }

// CWD is the working directory, an absolute path.
func (data Data) CWD() []byte { value, _ := data.at(1); return value }

// Args is the number of arguments including argv[0].
func (data Data) Args() int { return data.arguments }

// ArgCount is the number of arguments after argv[0].
func (data Data) ArgCount() int {
	if data.arguments == 0 {
		return 0
	}
	return data.arguments - 1
}

// Arg returns argument index; argument 0 is argv[0].
func (data Data) Arg(index int) ([]byte, bool) {
	if index < 0 || index >= data.arguments {
		return nil, false
	}
	return data.at(2 + index)
}

// EnvCount is the number of environment variables.
func (data Data) EnvCount() int { return data.environment }

// EnvAt returns variable index's name and value.
func (data Data) EnvAt(index int) ([]byte, []byte) {
	if index < 0 || index >= data.environment {
		return nil, nil
	}
	variable, _ := data.at(2 + data.arguments + index)
	name, value, _ := split(variable)
	return name, value
}

// Env returns the value of the variable named name.
func (data Data) Env(name string) ([]byte, bool) {
	for index := 0; index < data.environment; index++ {
		if key, value := data.EnvAt(index); equalString(key, name) {
			return value, true
		}
	}
	return nil, false
}

// HandleCount is the number of handles in the object.
func (data Data) HandleCount() int { return data.handles }

// HandleAt returns handle index's name and ID.
func (data Data) HandleAt(index int) ([]byte, handle.ID) {
	if index < 0 || index >= data.handles {
		return nil, 0
	}
	name, id, _ := data.handleEntry(index)
	return name, id
}

// Handle returns the handle bound to name.
func (data Data) Handle(name string) (handle.ID, bool) {
	for index := 0; index < data.handles; index++ {
		if key, id := data.HandleAt(index); equalString(key, name) {
			return id, true
		}
	}
	return 0, false
}

func (data Data) at(index int) ([]byte, bool) {
	if len(data.source) == 0 {
		return nil, false
	}
	return data.entry(index)
}

// BindHandles fills in place the zero handle IDs of a launch template with
// the handles the kernel installed, in the template's order. The object is
// then a bound startup object.
func BindHandles(template []byte, installed []handle.ID) bool {
	data, ok := DecodeTemplate(template)
	if !ok || data.handles != len(installed) {
		return false
	}
	for index := range installed {
		if _, id := data.HandleAt(index); id != 0 || installed[index] == 0 {
			return false
		}
	}
	for index, id := range installed {
		at := data.handleTable + index*handleSize
		binary.LittleEndian.PutUint64(data.source[at+8:at+16], uint64(id))
	}
	return true
}

// split divides "NAME=VALUE" at its first '='. The name is not empty.
func split(variable []byte) ([]byte, []byte, bool) {
	for index, character := range variable {
		if character == '=' {
			return variable[:index], variable[index+1:], index != 0
		}
	}
	return nil, nil, false
}

func absolute(path []byte) bool {
	return len(path) != 0 && len(path) <= limits.PathMax && path[0] == '/'
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

func equalString(left []byte, right string) bool {
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
