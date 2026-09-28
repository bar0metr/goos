// Package commands describes the executables in /system/bin. Each one declares
// a Manifest in its own source; the build collects them into one index in the
// system image, which PID 1 reads to decide what a launched command is granted
// and the shell reads to answer help. No list of commands is kept by hand.
package commands

// Grant names an authority a command may ask PID 1 for, beyond the standard
// streams every command receives. It is a request: PID 1 grants only the roles
// it knows, and only to executables in the immutable system image.
type Grant uint8

const (
	// GrantVFS asks for a private session with the filesystem service.
	GrantVFS Grant = 1 << iota
	// GrantBlockCheck asks PID 1 for an exclusive, read-only storage slice.
	// PID 1 grants it only after a clean VFS unmount.
	GrantBlockCheck
	// GrantTerminal asks the shell to put the terminal in raw mode while the
	// command runs in the foreground, for a full-screen program (ADR 0124).
	// It carries no handle: the command still reads its standard input and
	// writes its standard output, and gets no console authority.
	GrantTerminal

	knownGrants = GrantVFS | GrantBlockCheck | GrantTerminal
)

// Unbounded is the MaxArgs of a command that takes any number of arguments.
const Unbounded = -1

// Manifest documents one command. MinArgs and MaxArgs count the arguments
// after the command name.
type Manifest struct {
	Usage, Summary   string
	MinArgs, MaxArgs int
	Grants           Grant
}

// Accepts reports whether count arguments after the name are allowed.
func (manifest Manifest) Accepts(count int) bool {
	return count >= manifest.MinArgs && (manifest.MaxArgs == Unbounded || count <= manifest.MaxArgs)
}

const (
	// IndexPath is where the build places the index in the system image.
	IndexPath = "/system/commands"
	// MaxIndex bounds the index a reader has to hold.
	MaxIndex = 4096
	// MaxName, MaxUsage and MaxSummary bound one entry's fields.
	MaxName    = 32
	MaxUsage   = 96
	MaxSummary = 96
	// MaxArgs is the largest bounded argument count an entry may declare.
	MaxArgs = 99
)

// Entry is one decoded index line. Its byte slices alias the index.
type Entry struct {
	Name, Usage, Summary []byte
	MinArgs, MaxArgs     int
	Grants               Grant
}

// Accepts reports whether count arguments after the name are allowed.
func (entry Entry) Accepts(count int) bool {
	return Manifest{MinArgs: entry.MinArgs, MaxArgs: entry.MaxArgs}.Accepts(count)
}

// Next decodes the entry starting at offset and returns the offset after it.
// It returns false at the end of the index and on any malformed line, so a
// reader stops rather than trusting what follows.
//
// A line is NAME TAB GRANTS TAB MIN TAB MAX TAB USAGE TAB SUMMARY LF, with
// GRANTS, MIN and MAX in decimal and MAX written as "*" when unbounded.
func Next(index []byte, offset int) (Entry, int, bool) {
	if offset < 0 || offset >= len(index) {
		return Entry{}, offset, false
	}
	end := offset
	for end < len(index) && index[end] != '\n' {
		end++
	}
	if end == len(index) {
		return Entry{}, offset, false
	}
	var fields [6][]byte
	count, start := 0, offset
	for position := offset; position <= end; position++ {
		if position < end && index[position] != '\t' {
			continue
		}
		if count == len(fields) {
			return Entry{}, offset, false
		}
		fields[count] = index[start:position]
		count++
		start = position + 1
	}
	if count != len(fields) {
		return Entry{}, offset, false
	}
	grants, grantsOK := decimal(fields[1], uint64(knownGrants))
	minimum, minimumOK := decimal(fields[2], MaxArgs)
	maximum, maximumOK := int(Unbounded), true
	if !(len(fields[3]) == 1 && fields[3][0] == '*') {
		var value uint64
		value, maximumOK = decimal(fields[3], MaxArgs)
		maximum = int(value)
	}
	entry := Entry{Name: fields[0], Usage: fields[4], Summary: fields[5], MinArgs: int(minimum), MaxArgs: maximum, Grants: Grant(grants)}
	if !grantsOK || !minimumOK || !maximumOK || maximum != Unbounded && maximum < entry.MinArgs ||
		!ValidName(entry.Name) || !validText(entry.Usage, MaxUsage) || !validText(entry.Summary, MaxSummary) {
		return Entry{}, offset, false
	}
	return entry, end + 1, true
}

// Lookup finds the entry for name. A malformed index answers not found.
func Lookup(index []byte, name []byte) (Entry, bool) {
	for offset := 0; ; {
		entry, next, ok := Next(index, offset)
		if !ok {
			return Entry{}, false
		}
		if equal(entry.Name, name) {
			return entry, true
		}
		offset = next
	}
}

// Append encodes one entry after target. It refuses a manifest the reader
// would reject, so the build fails instead of the index being cut short.
func Append(target []byte, name string, manifest Manifest) ([]byte, bool) {
	if !ValidName([]byte(name)) || !validText([]byte(manifest.Usage), MaxUsage) || !validText([]byte(manifest.Summary), MaxSummary) ||
		manifest.Grants&^knownGrants != 0 || manifest.MinArgs < 0 || manifest.MinArgs > MaxArgs ||
		manifest.MaxArgs != Unbounded && (manifest.MaxArgs < manifest.MinArgs || manifest.MaxArgs > MaxArgs) {
		return target, false
	}
	target = append(target, name...)
	target = append(target, '\t')
	target = appendDecimal(target, uint64(manifest.Grants))
	target = append(target, '\t')
	target = appendDecimal(target, uint64(manifest.MinArgs))
	target = append(target, '\t')
	if manifest.MaxArgs == Unbounded {
		target = append(target, '*')
	} else {
		target = appendDecimal(target, uint64(manifest.MaxArgs))
	}
	target = append(target, '\t')
	target = append(target, manifest.Usage...)
	target = append(target, '\t')
	target = append(target, manifest.Summary...)
	return append(target, '\n'), true
}

// ValidName accepts the names a command directory may have: lower-case
// letters, digits and '-', starting with a letter.
func ValidName(name []byte) bool {
	if len(name) == 0 || len(name) > MaxName || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, value := range name {
		if (value < 'a' || value > 'z') && (value < '0' || value > '9') && value != '-' {
			return false
		}
	}
	return true
}

// validText accepts printable ASCII without tabs, so a field cannot split a
// line or carry terminal control sequences to the console.
func validText(text []byte, limit int) bool {
	if len(text) == 0 || len(text) > limit {
		return false
	}
	for _, value := range text {
		if value < ' ' || value > '~' {
			return false
		}
	}
	return true
}

func decimal(text []byte, limit uint64) (uint64, bool) {
	if len(text) == 0 || len(text) > 3 {
		return 0, false
	}
	value := uint64(0)
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		value = value*10 + uint64(digit-'0')
	}
	return value, value <= limit
}

func appendDecimal(target []byte, value uint64) []byte {
	if value >= 10 {
		target = appendDecimal(target, value/10)
	}
	return append(target, byte('0'+value%10))
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
