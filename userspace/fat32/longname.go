package fat32

import "goos/kernel/ramfs"

const (
	// maxLongName is the VFS name bound. VFAT allows 255 bytes, but no name
	// wider than ramfs.MaxName survives a path component or a directory reply,
	// so the smaller bound is the honest one.
	maxLongName    = ramfs.MaxName
	longChars      = 13
	maxLongEntries = (maxLongName + longChars - 1) / longChars
	maxAlias       = 99

	attrLongName = byte(0x0f)
	attrMask     = byte(0x3f)
	longLast     = byte(0x40)
	longOrdinal  = byte(0x3f)
)

// longSlots holds the offsets of the 5/6/2 UCS-2 characters a long entry
// carries.
var longSlots = [longChars]int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}

// request is a requested name: its raw bytes and the 8.3 form when it fits a
// single directory entry.
type request struct {
	raw   [maxLongName]byte
	size  int
	short [11]byte
	fits  bool
}

// chain reports how many long entries the name needs before its short entry.
func (want *request) chain() int {
	if want.fits {
		return 0
	}
	return (want.size + longChars - 1) / longChars
}

func (want *request) set(name ramfs.Name) bool {
	want.size = name.CopyTo(want.raw[:])
	if want.size == 0 {
		return false
	}
	for i := 0; i < want.size; i++ {
		if !longByte(want.raw[i]) {
			return false
		}
	}
	if want.raw[0] == ' ' || want.raw[want.size-1] == ' ' || want.raw[want.size-1] == '.' {
		return false
	}
	want.short, want.fits = shortForm(want.raw[:want.size])
	return true
}

func (want *request) same(name []byte) bool {
	if len(name) != want.size {
		return false
	}
	for i, value := range name {
		if lower(value) != lower(want.raw[i]) {
			return false
		}
	}
	return true
}

// longByte accepts the printable ASCII a name may carry. Anything else is a
// code point this system cannot represent.
func longByte(value byte) bool {
	switch value {
	case '"', '*', '/', ':', '<', '>', '?', '\\', '|':
		return false
	}
	return value >= 0x20 && value < 0x7f
}

// placement is a reserved run of directory entries: where its long chain
// starts, how many long entries it holds, and the short name they cover.
type placement struct {
	start int
	count int
	short [11]byte
}

func (at *placement) index() int { return at.start + at.count }

// chainScan accumulates the long name carried by the entries before a short
// entry. A chain whose ordinals, padding, or checksum do not match is
// discarded and the short name is used instead, as other implementations do.
type chainScan struct {
	name  [maxLongEntries * longChars]byte
	size  int
	total int
	want  byte
	sum   byte
	live  bool
	start int
}

func (scan *chainScan) reset() { *scan = chainScan{} }

func (scan *chainScan) long(entry [32]byte, index int) {
	ordinal := entry[0] & longOrdinal
	if entry[0]&longLast != 0 {
		scan.reset()
		if ordinal == 0 || int(ordinal) > maxLongEntries {
			return
		}
		scan.live, scan.want, scan.sum, scan.start = true, ordinal, entry[13], index
		scan.size, scan.total = -1, int(ordinal)*longChars
	}
	if !scan.live || ordinal != scan.want || entry[13] != scan.sum || entry[12] != 0 {
		scan.live = false
		return
	}
	base := (int(ordinal) - 1) * longChars
	for i := 0; i < longChars; i++ {
		offset := longSlots[i]
		code := uint16(entry[offset]) | uint16(entry[offset+1])<<8
		if code == 0 || code == 0xffff {
			if scan.size < 0 {
				scan.size = base + i
			}
			continue
		}
		if scan.size >= 0 && base+i > scan.size || code < 0x20 || code >= 0x7f {
			scan.live = false
			return
		}
		scan.name[base+i] = byte(code)
	}
	scan.want--
}

func (scan *chainScan) complete(short [11]byte) bool {
	if !scan.live || scan.want != 0 || scan.sum != shortChecksum(short) {
		return false
	}
	if scan.size < 0 {
		scan.size = scan.total
	}
	return scan.size > 0 && scan.size <= maxLongName
}

func shortChecksum(short [11]byte) byte {
	sum := byte(0)
	for _, value := range short {
		sum = sum>>1 | sum<<7
		sum += value
	}
	return sum
}

func isLongEntry(entry [32]byte) bool {
	return entry[0] != entryEnd && entry[0] != entryDeleted && entry[11]&attrMask == attrLongName
}

func shortOf(entry [32]byte) [11]byte {
	var short [11]byte
	copy(short[:], entry[:11])
	return short
}

func sameShort(entry [32]byte, short [11]byte) bool {
	for i := range short {
		if entry[i] != short[i] {
			return false
		}
	}
	return true
}

// shortForm returns the 8.3 encoding of a name that needs no long entries.
// Upper case is rejected because the 8.3 form is read back lower case, so a
// name holding it does not round-trip through a single entry.
func shortForm(raw []byte) ([11]byte, bool) {
	var out [11]byte
	for i := range out {
		out[i] = ' '
	}
	n := len(raw)
	if n == 0 {
		return out, false
	}
	dot := -1
	for i := 0; i < n; i++ {
		if raw[i] == '.' {
			if dot != -1 {
				return out, false
			}
			dot = i
		}
	}
	base, ext := n, 0
	if dot >= 0 {
		base, ext = dot, n-dot-1
	}
	if base < 1 || base > 8 || ext > 3 {
		return out, false
	}
	for i := 0; i < base; i++ {
		value, ok := shortByte(raw[i])
		if !ok {
			return out, false
		}
		out[i] = value
	}
	for i := 0; i < ext; i++ {
		value, ok := shortByte(raw[dot+1+i])
		if !ok {
			return out, false
		}
		out[8+i] = value
	}
	return out, true
}

func shortByte(value byte) (byte, bool) {
	switch {
	case value >= 'a' && value <= 'z':
		return value - 32, true
	case value >= '0' && value <= '9' || value == '_' || value == '-':
		return value, true
	}
	return 0, false
}

func aliasByte(value byte) byte {
	switch {
	case value >= 'a' && value <= 'z':
		return value - 32
	case value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_' || value == '-':
		return value
	}
	return '_'
}

func decodeShort(short [11]byte, out *[maxLongName]byte) int {
	n := 0
	for i := 0; i < 8 && short[i] != ' '; i++ {
		out[n] = lower(short[i])
		n++
	}
	ext := 0
	for ext < 3 && short[8+ext] != ' ' {
		ext++
	}
	if ext > 0 {
		out[n] = '.'
		n++
		for i := 0; i < ext; i++ {
			out[n] = lower(short[8+i])
			n++
		}
	}
	return n
}

// alias builds the short name a long entry is stored under, numbering the
// `~N` suffix until the directory holds no entry with that 8.3 name.
func (fs *FileSystem) alias(dir *directory, want *request) ([11]byte, ramfs.Status) {
	var short [11]byte
	for i := range short {
		short[i] = ' '
	}
	base := 0
	for i := 0; i < want.size && base < 6; i++ {
		if want.raw[i] == '.' {
			continue
		}
		short[base] = aliasByte(want.raw[i])
		base++
	}
	if base == 0 {
		short[0], base = '_', 1
	}
	for i := want.size - 1; i > 0; i-- {
		if want.raw[i] != '.' {
			continue
		}
		for j := 0; j < 3 && i+1+j < want.size; j++ {
			short[8+j] = aliasByte(want.raw[i+1+j])
		}
		break
	}
	for number := 1; number <= maxAlias; number++ {
		tail := base
		if number >= 10 && tail > 5 {
			tail = 5
		}
		short[tail] = '~'
		end := tail + 1
		if number >= 10 {
			short[end] = byte('0' + number/10)
			end++
		}
		short[end] = byte('0' + number%10)
		for i := end + 1; i < 8; i++ {
			short[i] = ' '
		}
		taken, status := fs.taken(dir, short)
		if status != ramfs.StatusOK || !taken {
			return short, status
		}
	}
	return short, ramfs.StatusResourceExhausted
}

func (fs *FileSystem) taken(dir *directory, short [11]byte) (bool, ramfs.Status) {
	for i := 0; i < dir.entries(); i++ {
		entry, ok := fs.readEntry(dir, i)
		if !ok {
			return false, ramfs.StatusResourceExhausted
		}
		if entry[0] == entryEnd {
			return false, ramfs.StatusOK
		}
		if listed(entry) && sameShort(entry, short) {
			return true, ramfs.StatusOK
		}
	}
	return false, ramfs.StatusOK
}

// commit writes a reserved run: the long entries in descending ordinal order
// first, then the short entry, so a scan that races the write stops at the
// end marker instead of seeing a short entry without its chain.
func (fs *FileSystem) commit(dir *directory, at *placement, entry [32]byte) bool {
	sum := shortChecksum(at.short)
	for ordinal := at.count; ordinal >= 1; ordinal-- {
		var long [32]byte
		long[0] = byte(ordinal)
		if ordinal == at.count {
			long[0] |= longLast
		}
		long[11], long[13] = attrLongName, sum
		base := (ordinal - 1) * longChars
		for i := 0; i < longChars; i++ {
			code := uint16(0xffff)
			switch {
			case base+i < fs.want.size:
				code = uint16(fs.want.raw[base+i])
			case base+i == fs.want.size:
				code = 0
			}
			long[longSlots[i]] = byte(code)
			long[longSlots[i]+1] = byte(code >> 8)
		}
		if !fs.writeEntry(dir, at.start+at.count-ordinal, long) {
			return false
		}
	}
	copy(entry[:11], at.short[:])
	return fs.writeEntry(dir, at.index(), entry)
}

// erase marks a whole chain deleted, the short entry first so a scan never
// finds a live short entry whose long entries have already gone.
func (fs *FileSystem) erase(dir *directory, start, index int) bool {
	for i := index; i >= start; i-- {
		entry, ok := fs.readEntry(dir, i)
		if !ok {
			return false
		}
		entry[0] = entryDeleted
		if !fs.writeEntry(dir, i, entry) {
			return false
		}
	}
	return true
}
