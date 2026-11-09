package ext4

// Kind classifies a failure so a service can map it onto its protocol status
// without parsing text.
type Kind uint8

const (
	KindIO Kind = iota + 1
	KindCorrupt
	KindUnsupported
	KindNeedsRecovery
	KindNotFound
	KindNotDirectory
	KindIsDirectory
	KindNotSymlink
	KindInvalid
	KindNameTooLong
	KindStale
	KindReadOnly
)

var kindNames = [...]string{
	KindIO:            "i/o error",
	KindCorrupt:       "corrupt metadata",
	KindUnsupported:   "unsupported",
	KindNeedsRecovery: "needs recovery",
	KindNotFound:      "not found",
	KindNotDirectory:  "not a directory",
	KindIsDirectory:   "is a directory",
	KindNotSymlink:    "not a symbolic link",
	KindInvalid:       "invalid argument",
	KindNameTooLong:   "name too long",
	KindStale:         "stale inode",
	KindReadOnly:      "read-only filesystem",
}

func (kind Kind) String() string {
	if int(kind) < len(kindNames) && kindNames[kind] != "" {
		return kindNames[kind]
	}
	return "unknown error"
}

// Error is the one error type the package returns. Reason is a fixed,
// bounded description of the first check that failed; it never embeds bytes
// read from the volume.
type Error struct {
	Kind   Kind
	Reason string
}

func (err *Error) Error() string {
	if err.Reason == "" {
		return "ext4: " + err.Kind.String()
	}
	return "ext4: " + err.Kind.String() + ": " + err.Reason
}

func fail(kind Kind, reason string) *Error { return &Error{Kind: kind, Reason: reason} }

func corrupt(reason string) *Error { return fail(KindCorrupt, reason) }

// errIO is a value, not a pointer made by a package initializer: GoOS
// services do not run initializers that need the heap.
var errIO = &ioError

var ioError = Error{Kind: KindIO, Reason: "device read failed"}

// KindOf reports the kind of err, or zero when err is nil or foreign.
func KindOf(err error) Kind {
	if typed, ok := err.(*Error); ok && typed != nil {
		return typed.Kind
	}
	return 0
}
