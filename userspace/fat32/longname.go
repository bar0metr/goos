package fat32

import "goos/kernel/ramfs"

const (
	// maxLongName is both VFAT's bound, 255 UTF-16 units, and the native
	// name limit, 255 bytes (ADR 0110): names are printable ASCII, one unit
	// per byte, so the two agree.
	maxLongName    = ramfs.MaxName
	longChars      = 13
	maxLongEntries = (maxLongName + longChars - 1) / longChars
	// maxAliasAttempts bounds the hashed aliases tried for one name.
	maxAliasAttempts = 4096

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

// caseFlags is byte 12 of the short entry: a name stored in one 8.3 entry is
// lower case, and the Windows NT flags say so to other systems.
func (want *request) caseFlags() byte {
	if want.fits {
		return caseLowerBase | caseLowerExt
	}
	return 0
}

// hash folds the name into the sixteen bits a hashed alias carries.
func (want *request) hash() uint16 {
	value := uint32(2166136261)
	for i := 0; i < want.size; i++ {
		value = (value ^ uint32(lower(want.raw[i]))) * 16777619
	}
	return uint16(value ^ value>>16)
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

// alias builds the short name a long entry is stored under, the way Linux
// and Windows do: the first six characters of the name with a "~1" to "~9"
// tail, and after those are taken two characters, four hexadecimal digits of
// a hash of the long name, and a tail, varying the digits until the directory
// holds no entry with that 8.3 name. The hash keeps a directory of many
// similar names from costing a scan per candidate number.
func (fs *FileSystem) alias(dir *directory, want *request) ([11]byte, ramfs.Status) {
	var short [11]byte
	for i := range short {
		short[i] = ' '
	}
	dot := -1
	for i := want.size - 1; i > 0; i-- {
		if want.raw[i] == '.' {
			dot = i
			break
		}
	}
	stem := want.size
	if dot >= 0 {
		stem = dot
	}
	base := 0
	for i := 0; i < stem && base < 8; i++ {
		if want.raw[i] == '.' || want.raw[i] == ' ' {
			continue
		}
		short[base] = aliasByte(want.raw[i])
		base++
	}
	if base == 0 {
		short[0], base = '_', 1
	}
	if dot >= 0 {
		ext := 0
		for i := dot + 1; i < want.size && ext < 3; i++ {
			if want.raw[i] == ' ' {
				continue
			}
			short[8+ext] = aliasByte(want.raw[i])
			ext++
		}
	}
	if base > 6 {
		base = 6
	}
	for number := byte(1); number <= 9; number++ {
		for i := base; i < 8; i++ {
			short[i] = ' '
		}
		short[base], short[base+1] = '~', '0'+number
		taken, status := fs.taken(dir, short)
		if status != ramfs.StatusOK || !taken {
			return short, status
		}
	}
	if base > 2 {
		base = 2
	}
	seed := want.hash()
	for attempt := 0; attempt < maxAliasAttempts; attempt++ {
		value := seed - uint16(attempt*11)
		for i := 0; i < 4; i++ {
			short[base+i] = hexDigit(byte(value >> uint(12-4*i)))
		}
		short[base+4], short[base+5] = '~', '1'+byte(attempt%9)
		for i := base + 6; i < 8; i++ {
			short[i] = ' '
		}
		taken, status := fs.taken(dir, short)
		if status != ramfs.StatusOK || !taken {
			return short, status
		}
	}
	return short, ramfs.StatusNoSpace
}

func hexDigit(value byte) byte {
	value &= 0xf
	if value < 10 {
		return '0' + value
	}
	return 'A' + value - 10
}

// taken reports whether a live entry of dir already has the 8.3 name short.
func (fs *FileSystem) taken(dir *directory, short [11]byte) (bool, ramfs.Status) {
	for i := 0; i < fs.entries(dir); i++ {
		entry, status := fs.readEntry(dir, i)
		if status != ramfs.StatusOK {
			return false, status
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
func (fs *FileSystem) commit(dir *directory, at *placement, entry [32]byte) ramfs.Status {
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
		if status := fs.writeEntry(dir, at.start+at.count-ordinal, long); status != ramfs.StatusOK {
			return status
		}
	}
	copy(entry[:11], at.short[:])
	return fs.writeEntry(dir, at.index(), entry)
}

// erase marks a whole chain deleted, the short entry first so a scan never
// finds a live short entry whose long entries have already gone.
func (fs *FileSystem) erase(dir *directory, start, index int) ramfs.Status {
	for i := index; i >= start; i-- {
		entry, status := fs.readEntry(dir, i)
		if status != ramfs.StatusOK {
			return status
		}
		entry[0] = entryDeleted
		if status = fs.writeEntry(dir, i, entry); status != ramfs.StatusOK {
			return status
		}
	}
	return ramfs.StatusOK
}

// locate returns the first entry of the chain covering the short entry at
// index. A chain is at most maxLongEntries long, so the scan starts no
// further back than that.
func (fs *FileSystem) locate(dir *directory, index int) (int, ramfs.Status) {
	fs.scan.reset()
	from := index - maxLongEntries
	if from < 0 {
		from = 0
	}
	for i := from; i <= index && i < fs.entries(dir); i++ {
		entry, status := fs.readEntry(dir, i)
		if status != ramfs.StatusOK {
			return index, status
		}
		if isLongEntry(entry) {
			fs.scan.long(entry, i)
			continue
		}
		if i == index {
			_, start := fs.resolveName(entry, i)
			return start, ramfs.StatusOK
		}
		fs.scan.reset()
	}
	return index, ramfs.StatusOK
}
