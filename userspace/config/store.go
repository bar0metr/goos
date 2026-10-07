// Package config implements the typed configuration store. Configuration is
// not a directory of text files that every component parses its own way: keys
// are declared with a type, values are validated against it, and a client
// reaches the store through a session whose scope and rights decide what it may
// see and change.
package config

const (
	// MaxKey bounds a dotted key. It matches a VFS path component so a key can
	// also be a name in the exported form.
	MaxKey = 63
	// MaxText bounds a text value.
	MaxText = 64
	// MaxEntries bounds the store. Configuration is small and system-wide;
	// anything that needs to grow without bound is data, not configuration.
	MaxEntries = 48
)

type Kind uint8

const (
	KindBool Kind = iota + 1
	KindUint
	KindText
)

type Status uint8

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusNotFound
	StatusPermissionDenied
	StatusTypeMismatch
	StatusResourceExhausted
	StatusReadOnly
)

// Rights are what a session may do within its scope. They are separate from the
// IPC rights on the channel it arrives over: those say whether a message can be
// sent, these say what the message may ask for.
type Rights uint8

const (
	Read Rights = 1 << iota
	Write
	// HostsRead and HostsWrite govern the hosts table (GHST), which is not
	// part of the key namespace and so is not covered by Read and Write or
	// limited by a scope.
	HostsRead
	HostsWrite

	allRights = Read | Write | HostsRead | HostsWrite
)

// Scope is the namespace prefix a session is confined to. An empty scope sees
// every namespace; "user." sees only the user namespace. A scope is matched on
// whole components, so "user." never matches "username.x".
type Scope struct {
	prefix [MaxKey]byte
	size   uint8
}

func MakeScope(prefix string) (Scope, bool) {
	var scope Scope
	if len(prefix) > MaxKey {
		return scope, false
	}
	if len(prefix) != 0 && !validScope(prefix) {
		return scope, false
	}
	scope.size = uint8(copy(scope.prefix[:], prefix))
	return scope, true
}

func (scope Scope) covers(key []byte) bool {
	if scope.size == 0 {
		return true
	}
	if len(key) <= int(scope.size) {
		return false
	}
	for index := 0; index < int(scope.size); index++ {
		if key[index] != scope.prefix[index] {
			return false
		}
	}
	return true
}

// Value is a typed configuration value. Bool and Uint share the numeric field;
// a bool is zero or one and nothing else.
type Value struct {
	Kind Kind
	Uint uint64
	Text [MaxText]byte
	Size uint8
}

func Bool(value bool) Value {
	number := uint64(0)
	if value {
		number = 1
	}
	return Value{Kind: KindBool, Uint: number}
}

func Uint(value uint64) Value { return Value{Kind: KindUint, Uint: value} }

func Text(value []byte) (Value, bool) {
	if len(value) > MaxText || !printable(value) {
		return Value{}, false
	}
	result := Value{Kind: KindText, Size: uint8(len(value))}
	copy(result.Text[:], value)
	return result, true
}

func (value Value) Bytes() []byte { return value.Text[:value.Size] }

// valid rejects a value whose contents do not match its own kind, which is what
// stops a decoded message from introducing a value the store never accepts.
func (value Value) valid() bool {
	switch value.Kind {
	case KindBool:
		return value.Uint <= 1 && value.Size == 0
	case KindUint:
		return value.Size == 0
	case KindText:
		return value.Uint == 0 && int(value.Size) <= MaxText && printable(value.Text[:value.Size])
	default:
		return false
	}
}

type entry struct {
	key      [MaxKey]byte
	size     uint8
	value    Value
	readOnly bool
	used     bool
}

// Store holds the declared keys. A key is declared with its type before it can
// be set, so a write is checked against a schema rather than against whatever
// happened to be there.
type Store struct {
	entries [MaxEntries]entry
}

// Declare adds a key with its type and initial value. A read-only key is one
// the system publishes and nothing may change: the running version, the machine
// it was built for. Declaring an existing key replaces its declaration, which
// is how a seed is applied over defaults.
func (store *Store) Declare(key string, value Value, readOnly bool) Status {
	name := []byte(key)
	if !validKey(name) || !value.valid() {
		return StatusInvalidArgument
	}
	if slot := store.find(name); slot != nil {
		slot.value, slot.readOnly = value, readOnly
		return StatusOK
	}
	for index := range store.entries {
		slot := &store.entries[index]
		if slot.used {
			continue
		}
		slot.size = uint8(copy(slot.key[:], name))
		slot.value, slot.readOnly, slot.used = value, readOnly, true
		return StatusOK
	}
	return StatusResourceExhausted
}

// Get returns a value if the session's scope covers the key and it may read.
// A key outside the scope is reported as not found rather than as denied: a
// session confined to a namespace learns nothing about what exists elsewhere.
func (store *Store) Get(scope Scope, rights Rights, key []byte) (Value, Status) {
	if rights&Read == 0 {
		return Value{}, StatusPermissionDenied
	}
	if !validKey(key) {
		return Value{}, StatusInvalidArgument
	}
	if !scope.covers(key) {
		return Value{}, StatusNotFound
	}
	slot := store.find(key)
	if slot == nil {
		return Value{}, StatusNotFound
	}
	return slot.value, StatusOK
}

// Set replaces a declared value with one of the same type. An undeclared key is
// not found: configuration is a schema, not a bag, and a typo must not quietly
// become a new setting that nothing reads.
func (store *Store) Set(scope Scope, rights Rights, key []byte, value Value) Status {
	if rights&Write == 0 {
		return StatusPermissionDenied
	}
	if !validKey(key) || !value.valid() {
		return StatusInvalidArgument
	}
	if !scope.covers(key) {
		return StatusNotFound
	}
	slot := store.find(key)
	if slot == nil {
		return StatusNotFound
	}
	if slot.readOnly {
		return StatusReadOnly
	}
	if slot.value.Kind != value.Kind {
		return StatusTypeMismatch
	}
	slot.value = value
	return StatusOK
}

// At returns the cursor-th key the session can see, in declaration order, so a
// client can list a namespace without being told what is outside it.
func (store *Store) At(scope Scope, rights Rights, cursor int) ([]byte, Value, bool, int, Status) {
	if rights&Read == 0 {
		return nil, Value{}, false, cursor, StatusPermissionDenied
	}
	if cursor < 0 {
		return nil, Value{}, false, cursor, StatusInvalidArgument
	}
	for index := cursor; index < len(store.entries); index++ {
		slot := &store.entries[index]
		if !slot.used || !scope.covers(slot.key[:slot.size]) {
			continue
		}
		return slot.key[:slot.size], slot.value, slot.readOnly, index + 1, StatusOK
	}
	return nil, Value{}, false, len(store.entries), StatusNotFound
}

func (store *Store) find(key []byte) *entry {
	for index := range store.entries {
		slot := &store.entries[index]
		if slot.used && equal(slot.key[:slot.size], key) {
			return slot
		}
	}
	return nil
}

// validKey requires at least two components, so every key names a namespace and
// a setting within it, and rejects anything that would not survive the exported
// text form.
func validKey(key []byte) bool {
	if len(key) == 0 || len(key) > MaxKey || key[0] == '.' || key[len(key)-1] == '.' {
		return false
	}
	components := 1
	previousDot := false
	for _, character := range key {
		switch {
		case character == '.':
			if previousDot {
				return false
			}
			components++
			previousDot = true
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '-':
			previousDot = false
		default:
			return false
		}
	}
	return components >= 2
}

// validScope accepts a prefix ending in a separator, which is what makes a
// scope match whole components.
func validScope(prefix string) bool {
	if prefix[len(prefix)-1] != '.' {
		return false
	}
	previousDot := true
	for index := 0; index < len(prefix); index++ {
		character := prefix[index]
		switch {
		case character == '.':
			if previousDot && index != len(prefix)-1 {
				return false
			}
			if previousDot && index == len(prefix)-1 && index == 0 {
				return false
			}
			previousDot = true
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '-':
			previousDot = false
		default:
			return false
		}
	}
	return true
}

func printable(value []byte) bool {
	for _, character := range value {
		if character < 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}

func equal(left []byte, right []byte) bool {
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
