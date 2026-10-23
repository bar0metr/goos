package initramfs

const (
	blockSize = 512
	// USTAR type flags. A zero flag also names a regular file.
	typeFile      = byte('0')
	typeLink      = byte('1')
	typeDirectory = byte('5')
	// maxLinkDepth bounds hard-link resolution so a cyclic archive cannot make
	// a lookup run forever.
	maxLinkDepth = uint8(4)
)

// Find returns the contents of the member named path. The kernel uses it once,
// to locate the bootstrap program; every other launch names its image by
// extent (Extent) and the kernel resolves no path for it (ADR 0093).
func Find(archive []byte, path []byte) ([]byte, bool) {
	entry, ok := find(archive, memberName{head: path}, 0)
	return entry.data, ok
}

// Extent reports where the contents of the member named path lie in the
// archive: the byte offset of its data and its length. Hard links resolve to
// the member they name, so every path leading to the same bytes reports the
// same extent. PID 1 uses it to name an executable to the kernel by its place
// in the immutable image rather than by a path the kernel would have to
// resolve.
func Extent(archive []byte, path []byte) (int, int, bool) {
	return extent(archive, memberName{head: path})
}

// ExtentUnder is Extent for the member named prefix followed by rest. A
// caller that maps a mount point to an archive directory looks the member up
// without joining the two into a buffer of its own.
func ExtentUnder(archive []byte, prefix []byte, rest []byte) (int, int, bool) {
	return extent(archive, memberName{head: prefix, tail: rest})
}

func extent(archive []byte, path memberName) (int, int, bool) {
	entry, ok := find(archive, path, 0)
	if !ok {
		return 0, 0, false
	}
	return entry.offset + blockSize, len(entry.data), true
}

// memberName is a member path in two parts, head then tail.
type memberName struct{ head, tail []byte }

func (path memberName) size() int { return len(path.head) + len(path.tail) }

func (path memberName) at(index int) byte {
	if index < len(path.head) {
		return path.head[index]
	}
	return path.tail[index-len(path.head)]
}

func find(archive []byte, path memberName, depth uint8) (member, bool) {
	if !validPath(path) {
		return member{}, false
	}
	for offset := 0; ; {
		entry, next, ok := scan(archive, offset)
		if !ok {
			return member{}, false
		}
		if entry.typeFlag == typeLink && equalMember(entry.name, path) {
			if depth >= maxLinkDepth || len(entry.link) == 0 {
				return member{}, false
			}
			return find(archive, memberName{head: entry.link}, depth+1)
		}
		if entry.typeFlag != typeDirectory && equalMember(entry.name, path) {
			return entry, true
		}
		offset = next
	}
}

// member is one validated archive entry. Its fields alias the archive, so a
// scan neither copies nor allocates and stays usable on the launch path.
type member struct {
	offset   int
	typeFlag byte
	name     []byte
	link     []byte
	data     []byte
}

// scan validates the member starting at offset and reports where the next one
// begins. A zero block, a malformed header, or a member reaching past the end
// of the archive stops the scan: the archive is untrusted input and a partial
// read is never treated as a shorter archive.
func scan(archive []byte, offset int) (member, int, bool) {
	if offset < 0 || offset > len(archive)-blockSize {
		return member{}, 0, false
	}
	header := archive[offset : offset+blockSize]
	if zero(header) {
		return member{}, 0, false
	}
	typeFlag := header[156]
	if !checksumValid(header) || typeFlag != 0 && typeFlag != typeFile && typeFlag != typeLink && typeFlag != typeDirectory || header[345] != 0 {
		return member{}, 0, false
	}
	nameSize := fieldSize(header[0:100])
	size, ok := octal(header[124:136])
	dataStart := offset + blockSize
	dataEnd := dataStart + size
	if !ok || nameSize == 0 || dataEnd < dataStart || dataEnd > len(archive) {
		return member{}, 0, false
	}
	next := dataStart + align(size)
	if next < dataStart || next > len(archive) {
		return member{}, 0, false
	}
	linkSize := fieldSize(header[157:257])
	return member{
		offset:   offset,
		typeFlag: typeFlag,
		name:     header[:nameSize],
		link:     header[157 : 157+linkSize],
		data:     archive[dataStart:dataEnd],
	}, next, true
}

func checksumValid(header []byte) bool {
	expected, ok := octal(header[148:156])
	if !ok {
		return false
	}
	sum := 0
	for index, value := range header {
		if index >= 148 && index < 156 {
			sum += ' '
		} else {
			sum += int(value)
		}
	}
	return sum == expected
}

func octal(field []byte) (int, bool) {
	value := 0
	seen := false
	for _, digit := range field {
		if digit == 0 || digit == ' ' {
			if seen {
				break
			}
			continue
		}
		if digit < '0' || digit > '7' || value > (int(^uint(0)>>1)-int(digit-'0'))/8 {
			return 0, false
		}
		seen = true
		value = value*8 + int(digit-'0')
	}
	return value, seen
}

func fieldSize(field []byte) int {
	for index, value := range field {
		if value == 0 {
			return index
		}
	}
	return len(field)
}

func align(size int) int { return (size + blockSize - 1) &^ (blockSize - 1) }

func zero(block []byte) bool {
	for _, value := range block {
		if value != 0 {
			return false
		}
	}
	return true
}

func equalName(value []byte, expected []byte) bool {
	return equalMember(value, memberName{head: expected})
}

func equalMember(value []byte, expected memberName) bool {
	if len(value) != expected.size() {
		return false
	}
	for index := range value {
		if value[index] != expected.at(index) {
			return false
		}
	}
	return true
}

func validPath(path memberName) bool {
	size := path.size()
	if size == 0 || path.at(0) == '/' {
		return false
	}
	componentStart := 0
	for index := 0; index < size; index++ {
		value := path.at(index)
		if value == 0 || index != 0 && value == '/' && path.at(index-1) == '/' {
			return false
		}
		if value == '/' {
			if parentComponent(path, componentStart, index) {
				return false
			}
			componentStart = index + 1
		}
	}
	return componentStart < size && !parentComponent(path, componentStart, size)
}

func parentComponent(path memberName, start int, end int) bool {
	return end-start == 2 && path.at(start) == '.' && path.at(start+1) == '.'
}
