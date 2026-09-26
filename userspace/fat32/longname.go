package fat32

import "goos/kernel/ramfs"

const (
	// maxLongName is both VFAT's bound, 255 UTF-16 units, and the native
	// name limit, 255 bytes (ADR 0110). A UTF-8 name never has more UTF-16
	// units than bytes, so every native name fits a chain; an on-disk name
	// whose UTF-8 form is longer than 255 bytes is shown by its 8.3 alias.
	maxLongName    = ramfs.MaxName
	longChars      = 13
	maxLongEntries = (maxLongName + longChars - 1) / longChars
	// maxShortName is the longest UTF-8 form of an 8.3 name: eleven OEM
	// characters of up to three bytes each and the dot.
	maxShortName = 11*3 + 1
	// maxAliasAttempts bounds the hashed aliases tried for one name.
	maxAliasAttempts = 4096

	attrLongName = byte(0x0f)
	attrMask     = byte(0x3f)
	longLast     = byte(0x40)
	longOrdinal  = byte(0x3f)
	longPad      = uint16(0xffff)
)

// longSlots holds the offsets of the 5/6/2 UTF-16 units a long entry
// carries.
var longSlots = [longChars]int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}

// request is a requested name: its UTF-8 bytes, its UTF-16 form, and the 8.3
// form with its case flags when it fits a single directory entry.
type request struct {
	raw   [maxLongName]byte
	size  int
	units [maxLongName]uint16
	count int
	short [11]byte
	flags byte
	fits  bool
}

// chain reports how many long entries the name needs before its short entry.
func (want *request) chain() int {
	if want.fits {
		return 0
	}
	return (want.count + longChars - 1) / longChars
}

// set decodes and validates a requested name. Its rules are the ones every
// name read from disk must pass to be listed under its long form, so that
// every listed name can be opened.
func (want *request) set(name ramfs.Name) bool {
	want.size = name.CopyTo(want.raw[:])
	if !validLongName(want.raw[:want.size]) {
		return false
	}
	want.count = 0
	for rest := want.raw[:want.size]; len(rest) != 0; {
		value, size := decodeRune(rest)
		rest = rest[size:]
		if value >= 0x10000 {
			value -= 0x10000
			want.units[want.count] = uint16(surrogateHigh + value>>10)
			want.units[want.count+1] = uint16(surrogateLow + value&0x3ff)
			want.count += 2
			continue
		}
		want.units[want.count] = uint16(value)
		want.count++
	}
	want.short, want.flags, want.fits = shortForm(want.raw[:want.size])
	return true
}

// validLongName accepts a name VFAT can store and this system can name:
// valid UTF-8 of 1 to 255 bytes without control characters or the
// characters FAT reserves (" * / : < > ? \ |), not starting with a space and
// not ending with a space or a dot, which Windows would strip.
func validLongName(name []byte) bool {
	if len(name) == 0 || len(name) > maxLongName || name[0] == ' ' || name[len(name)-1] == ' ' || name[len(name)-1] == '.' {
		return false
	}
	if len(name) <= 2 && name[0] == '.' && (len(name) == 1 || name[1] == '.') {
		return false
	}
	for rest := name; len(rest) != 0; {
		value, size := decodeRune(rest)
		if size == 0 || value < 0x20 {
			return false
		}
		switch value {
		case '"', '*', '/', ':', '<', '>', '?', '\\', '|':
			return false
		}
		rest = rest[size:]
	}
	return true
}

// caseFlags is byte 12 of the short entry of a name stored in one 8.3 entry:
// the Windows NT flags saying its base or extension is displayed lower case.
func (want *request) caseFlags() byte {
	if want.fits {
		return want.flags
	}
	return 0
}

// hash folds the name into the sixteen bits a hashed alias carries. It hashes
// the folded name, so names that match each other hash alike.
func (want *request) hash() uint16 {
	value := uint32(2166136261)
	for rest := want.raw[:want.size]; len(rest) != 0; {
		code, size := decodeRune(rest)
		rest = rest[size:]
		value = (value ^ uint32(foldRune(code))) * 16777619
	}
	return uint16(value ^ value>>16)
}

func (want *request) same(name []byte) bool {
	return sameFolded(name, want.raw[:want.size])
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
// entry. A chain whose ordinals, checksums, or padding do not match, whose
// UTF-16 is not valid, or whose name this system could not open is discarded
// and the short name is used instead, as other implementations do.
type chainScan struct {
	units [maxLongEntries * longChars]uint16
	size  int
	total int
	want  byte
	sum   byte
	live  bool
	start int
}

func (scan *chainScan) reset() {
	scan.size, scan.total, scan.want, scan.sum, scan.live, scan.start = 0, 0, 0, 0, false, 0
}

func (scan *chainScan) long(entry [32]byte, index int) {
	ordinal := entry[0] & longOrdinal
	last := entry[0]&longLast != 0
	if last {
		scan.reset()
		if ordinal == 0 || int(ordinal) > maxLongEntries {
			return
		}
		scan.live, scan.want, scan.sum, scan.start = true, ordinal, entry[13], index
		scan.size, scan.total = -1, int(ordinal)*longChars
	}
	// A long entry has type 0 and no cluster; one that does is not part of
	// a VFAT name.
	if !scan.live || ordinal != scan.want || entry[13] != scan.sum || entry[12] != 0 || get16(entry[26:28]) != 0 {
		scan.live = false
		return
	}
	base := (int(ordinal) - 1) * longChars
	for i := 0; i < longChars; i++ {
		offset := longSlots[i]
		code := uint16(entry[offset]) | uint16(entry[offset+1])<<8
		if scan.size >= 0 && base+i >= scan.size {
			// Past the terminator only padding may follow.
			if code != longPad && !(code == 0 && base+i == scan.size) {
				scan.live = false
				return
			}
			continue
		}
		if code == 0 || code == longPad {
			// The name ends only in the entry that holds its last units.
			if !last {
				scan.live = false
				return
			}
			scan.size = base + i
			continue
		}
		scan.units[base+i] = code
	}
	scan.want--
}

// complete checks the gathered chain against the short entry after it and
// writes the name as UTF-8 into target.
func (scan *chainScan) complete(short [11]byte, target []byte) (int, bool) {
	if !scan.live || scan.want != 0 || scan.sum != shortChecksum(short) {
		return 0, false
	}
	units := scan.size
	if units < 0 {
		units = scan.total
	}
	if units == 0 || units > maxLongName {
		return 0, false
	}
	size, ok := utf16To8(scan.units[:units], target)
	if !ok || !validLongName(target[:size]) {
		return 0, false
	}
	return size, true
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

// shortForm returns the 8.3 encoding of a name that needs no long entries,
// with the NT case flags it needs to read back unchanged. It fits when it is
// ASCII from the 8.3 alphabet, has one base of one to eight characters and at
// most one extension of up to three, and each part is in one case: Windows
// records "readme.txt" and "README.TXT" this way, and a mixed-case part
// ("ReadMe.txt") needs a long name.
func shortForm(raw []byte) ([11]byte, byte, bool) {
	var out [11]byte
	for i := range out {
		out[i] = ' '
	}
	n := len(raw)
	dot := -1
	for i := 0; i < n; i++ {
		if raw[i] == '.' {
			if dot != -1 {
				return out, 0, false
			}
			dot = i
		}
	}
	base, ext := n, 0
	if dot >= 0 {
		base, ext = dot, n-dot-1
	}
	if base < 1 || base > 8 || ext > 3 || dot >= 0 && ext == 0 {
		return out, 0, false
	}
	baseCase, ok := shortPart(raw[:base], out[:8])
	if !ok {
		return out, 0, false
	}
	flags := byte(0)
	if baseCase == caseLower {
		flags |= caseLowerBase
	}
	if dot >= 0 {
		extCase, ok := shortPart(raw[dot+1:], out[8:])
		if !ok {
			return out, 0, false
		}
		if extCase == caseLower {
			flags |= caseLowerExt
		}
	}
	return out, flags, true
}

const (
	caseNone = iota
	caseLower
	caseUpper
)

// shortPart upper-cases one part of an 8.3 name into target, reporting the
// case its letters were in, and fails on a character outside the 8.3
// alphabet or on letters of both cases.
func shortPart(part []byte, target []byte) (int, bool) {
	seen := caseNone
	for i, value := range part {
		letter := caseNone
		switch {
		case value >= 'a' && value <= 'z':
			letter, value = caseLower, value-0x20
		case value >= 'A' && value <= 'Z':
			letter = caseUpper
		case !shortByte(value):
			return caseNone, false
		}
		if letter != caseNone {
			if seen != caseNone && seen != letter {
				return caseNone, false
			}
			seen = letter
		}
		target[i] = value
	}
	return seen, true
}

// shortByte accepts the non-letter characters an 8.3 name may carry.
func shortByte(value byte) bool {
	switch value {
	case '$', '%', '\'', '-', '_', '@', '~', '`', '!', '(', ')', '{', '}', '^', '#', '&':
		return true
	}
	return value >= '0' && value <= '9'
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

// decodeShort writes the name a short entry is listed under: its base, a dot
// and its extension when it has one, each part lower case when the entry's NT
// flags say so and as stored otherwise, bytes from 0x80 read as code page 437,
// and a leading 0x05 read as the 0xe5 it stands for.
func decodeShort(entry [32]byte, out []byte) int {
	n := 0
	for i := 0; i < 8 && entry[i] != ' '; i++ {
		value := entry[i]
		if i == 0 && value == entryKanji {
			value = entryDeleted
		}
		n += putShort(out[n:], value, entry[12]&caseLowerBase != 0)
	}
	ext := 0
	for ext < 3 && entry[8+ext] != ' ' {
		ext++
	}
	if ext > 0 {
		out[n] = '.'
		n++
		for i := 0; i < ext; i++ {
			n += putShort(out[n:], entry[8+i], entry[12]&caseLowerExt != 0)
		}
	}
	return n
}

func putShort(out []byte, value byte, lowerCase bool) int {
	if lowerCase {
		value = lower(value)
	}
	return encodeRune(out, oemRune(value))
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
	// A character outside ASCII becomes one '_': its continuation bytes are
	// skipped.
	base := 0
	for i := 0; i < stem && base < 8; i++ {
		if want.raw[i] == '.' || want.raw[i] == ' ' || want.raw[i]&0xc0 == 0x80 {
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
			if want.raw[i] == ' ' || want.raw[i]&0xc0 == 0x80 {
				continue
			}
			short[8+ext] = aliasByte(want.raw[i])
			ext++
		}
	}
	if base > 6 {
		base = 6
	}
	for i := base; i < 8; i++ {
		short[i] = ' '
	}
	short[base] = '~'
	numbers, status := fs.numberedAliases(dir, short, base)
	if status != ramfs.StatusOK {
		return short, status
	}
	for number := byte(1); number <= 9; number++ {
		if numbers&(1<<number) == 0 {
			short[base+1] = '0' + number
			return short, ramfs.StatusOK
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

// numberedAliases finds, in one pass over dir, which of the aliases
// base~1 to base~9 with the extension of short are already in use, as a mask
// of bits 1 to 9.
func (fs *FileSystem) numberedAliases(dir *directory, short [11]byte, base int) (uint16, ramfs.Status) {
	numbers := uint16(0)
	for i := 0; i < fs.entries(dir); i++ {
		entry, status := fs.readEntry(dir, i)
		if status != ramfs.StatusOK {
			return 0, status
		}
		if entry[0] == entryEnd {
			break
		}
		if !listed(entry) || entry[base] != '~' || entry[base+1] < '1' || entry[base+1] > '9' {
			continue
		}
		same := true
		for j := 0; j < 11 && same; j++ {
			switch {
			case j < base || j >= 8:
				same = entry[j] == short[j]
			case j >= base+2:
				same = entry[j] == ' '
			}
		}
		if same {
			numbers |= 1 << (entry[base+1] - '0')
		}
	}
	return numbers, ramfs.StatusOK
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
			case base+i < fs.want.count:
				code = fs.want.units[base+i]
			case base+i == fs.want.count:
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
			_, start, _ := fs.resolveName(entry, i)
			return start, ramfs.StatusOK
		}
		fs.scan.reset()
	}
	return index, ramfs.StatusOK
}
