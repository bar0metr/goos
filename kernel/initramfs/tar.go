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

// Find takes the name as bytes: the kernel looks one up on every launch, and
// converting a path to or from a string would allocate on its leaking heap.
func Find(archive []byte, path []byte) ([]byte, bool) {
	return find(archive, path, 0)
}

func find(archive []byte, path []byte, depth uint8) ([]byte, bool) {
	if !validPath(path) {
		return nil, false
	}
	for offset := 0; ; {
		entry, next, ok := scan(archive, offset)
		if !ok {
			return nil, false
		}
		if entry.typeFlag == typeLink && equalName(entry.name, path) {
			if depth >= maxLinkDepth || len(entry.link) == 0 {
				return nil, false
			}
			return find(archive, entry.link, depth+1)
		}
		if entry.typeFlag != typeDirectory && equalName(entry.name, path) {
			return entry.data, true
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
	if len(value) != len(expected) {
		return false
	}
	for index := range value {
		if value[index] != expected[index] {
			return false
		}
	}
	return true
}

func validPath(path []byte) bool {
	if len(path) == 0 || path[0] == '/' {
		return false
	}
	componentStart := 0
	for index, value := range path {
		if value == 0 || index != 0 && value == '/' && path[index-1] == '/' {
			return false
		}
		if value == '/' {
			if parentComponent(path[componentStart:index]) {
				return false
			}
			componentStart = index + 1
		}
	}
	return componentStart < len(path) && !parentComponent(path[componentStart:])
}

func parentComponent(component []byte) bool {
	return len(component) == 2 && component[0] == '.' && component[1] == '.'
}
