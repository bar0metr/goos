package fat32

import (
	"bytes"
	"fmt"
	"goos/kernel/ramfs"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

var unicodeNames = []string{
	"Фото с моря.jpg",
	"Документы",
	"日本語.txt",
	"音乐.mp3",
	"emoji-😀.txt",
	"Ελληνικά.md",
	"Ünïcödé Ñame.txt",
	"𝄞 music 𝄞",
}

func TestUnicodeNamesRoundTrip(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	docs := mkdir(t, fs, root, "Документы")
	for i, name := range unicodeNames {
		writeFile(t, fs, docs, name, fmt.Sprint(i))
	}
	fs, root = mounted(t, device)
	docs = mustOpen(t, fs, root, "Документы")
	listed := listNames(t, fs, docs)
	want := append([]string(nil), unicodeNames...)
	sort.Strings(listed)
	sort.Strings(want)
	if strings.Join(listed, "|") != strings.Join(want, "|") {
		t.Fatalf("listed %q, want %q", listed, want)
	}
	for i, name := range unicodeNames {
		if got := readFile(t, fs, mustOpen(t, fs, docs, name)); got != fmt.Sprint(i) {
			t.Fatalf("%s read %q", name, got)
		}
	}
	check(t, device)
}

func TestMaximumVFATNamesRoundTrip(t *testing.T) {
	names := []string{
		strings.Repeat("a", 255),
		strings.Repeat("Ж", 255),
		strings.Repeat("界", 255),
		strings.Repeat("😀", 127) + "x",
		strings.Repeat("AЖ界", 63) + strings.Repeat("😀", 31) + "zxcv",
	}
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	for index, name := range names {
		writeFile(t, fs, root, name, fmt.Sprint(index))
	}
	fs, root = mounted(t, device)
	listed := listNames(t, fs, root)
	if len(listed) != len(names) {
		t.Fatalf("listed %d names, want %d", len(listed), len(names))
	}
	for index, name := range names {
		id := mustOpen(t, fs, root, name)
		if got := readFile(t, fs, id); got != fmt.Sprint(index) {
			t.Fatalf("%d-byte name read %q", len(name), got)
		}
	}
}

func TestUnicodeCaseCollisionOutsideLegacyScriptSubset(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	writeFile(t, fs, root, "\U00010428.txt", "lower")
	if _, status := fs.CreateName(root, mustName(t, "\U00010400.TXT")); status != ramfs.StatusAlreadyExists {
		t.Fatalf("Deseret case collision: %v", status)
	}
}

func BenchmarkLongUnicodeCreateLookup(b *testing.B) {
	name := strings.Repeat("界", 255)
	for i := 0; i < b.N; i++ {
		fs, root := mounted(b, formattedDevice())
		id := writeFile(b, fs, root, name, "x")
		if opened, status := fs.OpenName(root, mustName(b, name)); status != ramfs.StatusOK || opened != id {
			b.Fatal(status)
		}
	}
}

func BenchmarkCaseInsensitiveLookupInLargeDirectory(b *testing.B) {
	fs, root := mounted(b, formattedDevice())
	for i := 0; i < 512; i++ {
		writeFile(b, fs, root, fmt.Sprintf("Отчёт-%04d-Данные.txt", i), "x")
	}
	want := mustName(b, "ОТЧЁТ-0511-ДАННЫЕ.TXT")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, status := fs.OpenName(root, want); status != ramfs.StatusOK {
			b.Fatal(status)
		}
	}
}

// TestLongNamesAreUTF16OnDisk reads the entries back and decodes them as a
// VFAT reader would: UTF-16LE, a surrogate pair for a character outside the
// Basic Multilingual Plane, a NUL terminator, 0xffff padding.
func TestLongNamesAreUTF16OnDisk(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	name := "emoji-😀x.txt"
	writeFile(t, fs, root, name, "x")
	units := utf16.Encode([]rune(name))
	if len(units) != 13 {
		t.Fatalf("the test name is %d units", len(units))
	}
	// Thirteen units fill one entry exactly: no terminator, no padding.
	long := rootEntry(t, fs, 0)
	if long[0] != longLast|1 || !isLongEntry(long) {
		t.Fatalf("entry 0 %#x", long[0])
	}
	for i, slot := range longSlots {
		if got := get16(long[slot:]); got != units[i] {
			t.Fatalf("unit %d = %#x, want %#x", i, got, units[i])
		}
	}
	writeFile(t, fs, root, "Фото.jpg", "y")
	long = rootEntry(t, fs, 2)
	units = utf16.Encode([]rune("Фото.jpg"))
	for i, slot := range longSlots {
		want := uint16(0xffff)
		switch {
		case i < len(units):
			want = units[i]
		case i == len(units):
			want = 0
		}
		if got := get16(long[slot:]); got != want {
			t.Fatalf("unit %d = %#x, want %#x", i, got, want)
		}
	}
	short := rootEntry(t, fs, 3)
	if long[13] != shortChecksum(shortOf(short)) {
		t.Fatal("checksum does not cover the alias")
	}
	// The alias of a name outside ASCII is legal 8.3: one '_' per character.
	if got := string(short[:11]); got != "____~1  JPG" {
		t.Fatalf("alias %q", got)
	}
}

func TestUnicodeMatchingIsCaseInsensitive(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	writeFile(t, fs, root, "Фото с моря.JPG", "sea")
	writeFile(t, fs, root, "Ελληνικά", "greek")
	writeFile(t, fs, root, "Ünïcödé", "latin")
	for alias, want := range map[string]string{
		"ФОТО С МОРЯ.jpg": "sea",
		"фото с моря.jpg": "sea",
		"ΕΛΛΗΝΙΚΆ":        "greek",
		"ελληνικά":        "greek",
		"üNÏCÖDÉ":         "latin",
	} {
		if got := readFile(t, fs, mustOpen(t, fs, root, alias)); got != want {
			t.Fatalf("%s read %q", alias, got)
		}
	}
	if _, status := fs.CreateName(root, mustName(t, "фОТО С МОРЯ.jpg")); status != ramfs.StatusAlreadyExists {
		t.Fatalf("a case variant was created twice: %v", status)
	}
	// Different letters remain different, including the ones folding does
	// not cover and names that differ only in normalization.
	for _, name := range []string{"Фото с моря.jpe", "日本語", "é", "é"} {
		if _, status := fs.CreateName(root, mustName(t, name)); status != ramfs.StatusOK {
			t.Fatalf("%q: %v", name, status)
		}
	}
	if _, status := fs.OpenName(root, mustName(t, "É")); status != ramfs.StatusOK {
		t.Fatalf("precomposed É does not match é: %v", status)
	}
}

func TestInvalidNamesAreRefused(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	for _, name := range [][]byte{
		{0xff, 'a'},              // not UTF-8
		{0xc0, 0x80},             // overlong
		{0xed, 0xa0, 0x80},       // a surrogate encoded in UTF-8
		{0xf4, 0x90, 0x80, 0x80}, // past U+10FFFF
		[]byte("tab\there"),
		[]byte("a:b"), []byte("a*b"), []byte("a?b"), []byte("a\\b"), []byte("a|b"), []byte("a<b"), []byte("a>b"), []byte("a\"b"),
		[]byte(" leading"), []byte("trailing "), []byte("trailing."),
	} {
		value, ok := ramfs.MakeName(name)
		if !ok {
			continue
		}
		if _, status := fs.CreateName(root, value); status != ramfs.StatusInvalidArgument {
			t.Errorf("%q: %v", name, status)
		}
	}
	// 255 bytes is the native bound: 127 two-byte characters and one more.
	long := strings.Repeat("Ж", 127) + "x"
	if _, status := fs.CreateName(root, mustName(t, long)); status != ramfs.StatusOK {
		t.Fatalf("255-byte Cyrillic name: %v", status)
	}
	if got := listNames(t, fs, root); len(got) != 1 || got[0] != long {
		t.Fatalf("listed %q", got)
	}
}

// writeRawChain puts a hand-built VFAT chain of units, with alias short, at
// the start of an empty root directory.
func writeRawChain(t *testing.T, fs *FileSystem, units []uint16, short string) {
	t.Helper()
	dir, _ := fs.openDirectory(0)
	var alias [11]byte
	copy(alias[:], short)
	count := (len(units) + longChars - 1) / longChars
	if status := fs.cover(&dir, count+1); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	for ordinal := count; ordinal >= 1; ordinal-- {
		var long [32]byte
		long[0] = byte(ordinal)
		if ordinal == count {
			long[0] |= longLast
		}
		long[11], long[13] = attrLongName, shortChecksum(alias)
		for i, slot := range longSlots {
			code, at := uint16(0xffff), (ordinal-1)*longChars+i
			switch {
			case at < len(units):
				code = units[at]
			case at == len(units):
				code = 0
			}
			put16(long[slot:], code)
		}
		fs.writeEntry(&dir, count-ordinal, long)
	}
	var entry [32]byte
	copy(entry[:11], alias[:])
	entry[11] = attrArchive
	fs.writeEntry(&dir, count, entry)
}

// TestForeignChainsThatCannotBeNamedFallBackToTheAlias covers long names this
// system must not show: invalid UTF-16, or a UTF-8 form it could not open.
func TestForeignChainsThatCannotBeNamedFallBackToTheAlias(t *testing.T) {
	for name, units := range map[string][]uint16{
		"unpaired high surrogate": {'a', 0xd83d, 'b'},
		"unpaired low surrogate":  {'a', 0xde00},
		"surrogate at the end":    {'a', 0xd83d},
		"control character":       {'a', 0x0001},
		"slash":                   {'a', '/', 'b'},
		"reserved character":      {'a', '*'},
		"too many UTF-16 units":   utf16.Encode([]rune(strings.Repeat("Ж", maxLongUnits+1))),
	} {
		t.Run(name, func(t *testing.T) {
			fs, root := mounted(t, formattedDevice())
			writeRawChain(t, fs, units, "FOREIG~1TXT")
			if got := listNames(t, fs, root); len(got) != 1 || got[0] != "FOREIG~1.TXT" {
				t.Fatalf("listed %q", got)
			}
			if _, status := fs.OpenName(root, mustName(t, "foreig~1.txt")); status != ramfs.StatusOK {
				t.Fatalf("alias: %v", status)
			}
		})
	}
	// Valid foreign names, including trailing-dot names and names whose UTF-8
	// representation exceeds 255 bytes, are listed and can be opened exactly.
	for suffix, alias := range map[string]string{".": "FOREIG~1TXT", " ": "FOREIG~2TXT"} {
		fs, root := mounted(t, formattedDevice())
		foreign := strings.Repeat("Ж", 200) + suffix
		writeRawChain(t, fs, utf16.Encode([]rune(foreign)), alias)
		if got := listNames(t, fs, root); len(got) != 1 || got[0] != foreign {
			t.Fatalf("listed %q", got)
		}
		if _, status := fs.OpenName(root, mustName(t, foreign)); status != ramfs.StatusOK {
			t.Fatalf("open foreign trailing name: %v", status)
		}
		if _, status := fs.CreateName(root, mustName(t, "new"+suffix)); status != ramfs.StatusInvalidArgument {
			t.Fatalf("create trailing %q: %v", suffix, status)
		}
	}
}

// TestDamagedChainsFallBackToTheAlias covers chain validation: ordinals,
// checksum, padding, the type and cluster fields, and deletion inside a
// chain.
func TestDamagedChainsFallBackToTheAlias(t *testing.T) {
	name := "A long enough name for 3 entries.txt"
	for label, damage := range map[string]func(entries [][32]byte){
		"missing last flag":       func(entries [][32]byte) { entries[0][0] &^= longLast },
		"skipped ordinal":         func(entries [][32]byte) { entries[1][0] = 1 },
		"checksum of one entry":   func(entries [][32]byte) { entries[1][13]++ },
		"nonzero type":            func(entries [][32]byte) { entries[2][12] = 1 },
		"nonzero cluster":         func(entries [][32]byte) { entries[2][26] = 5 },
		"padding in a middle one": func(entries [][32]byte) { put16(entries[1][longSlots[4]:], 0xffff) },
		"data after padding":      func(entries [][32]byte) { put16(entries[0][longSlots[12]:], 'x') },
		"deleted middle entry":    func(entries [][32]byte) { entries[1][0] = entryDeleted },
		"ordinal zero":            func(entries [][32]byte) { entries[0][0] = longLast },
		"ordinal past twenty":     func(entries [][32]byte) { entries[0][0] = longLast | 21 },
	} {
		t.Run(label, func(t *testing.T) {
			fs, root := mounted(t, formattedDevice())
			writeFile(t, fs, root, name, "x")
			dir, _ := fs.openDirectory(0)
			entries := make([][32]byte, 4)
			for i := range entries {
				entries[i], _ = fs.readEntry(&dir, i)
			}
			if !isLongEntry(entries[2]) || isLongEntry(entries[3]) {
				t.Fatal("unexpected layout")
			}
			damage(entries)
			for i := range entries[:3] {
				fs.writeEntry(&dir, i, entries[i])
			}
			fs, root = mounted(t, fs.device.(*memoryDevice))
			if got := listNames(t, fs, root); len(got) != 1 || got[0] != "ALONGE~1.TXT" {
				t.Fatalf("listed %q", got)
			}
			if _, status := fs.OpenName(root, mustName(t, name)); status != ramfs.StatusNotFound {
				t.Fatalf("damaged chain still matched: %v", status)
			}
		})
	}
}

// TestShortNamesKeepTheirCase covers 8.3 names: the NT case flags are
// honoured when reading and written when a name fits one entry; a name
// whose parts are each in one case fits, a mixed-case part needs a chain.
func TestShortNamesKeepTheirCase(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	for _, test := range []struct {
		name    string
		entries int
		flags   byte
	}{
		{"lower.txt", 1, caseLowerBase | caseLowerExt},
		{"UPPER.TXT", 1, 0},
		{"mixa.TXT", 1, caseLowerBase},
		{"MIXB.txt", 1, caseLowerExt},
		{"ReadMe.txt", 2, 0},
		{"123.456", 1, 0},
		{"a$%'@~!.{}", 1, caseLowerBase},
		{"a+b", 2, 0},
		{"two.dots.x", 2, 0},
		{"toolongname", 2, 0},
	} {
		dir, _ := fs.openDirectory(0)
		result, _ := fs.find(&dir, 0, false)
		writeFile(t, fs, root, test.name, "x")
		dir, _ = fs.openDirectory(0)
		used := 0
		var short [32]byte
		for index := result.marker; index < fs.entries(&dir); index++ {
			entry, _ := fs.readEntry(&dir, index)
			if entry[0] == entryEnd {
				break
			}
			used++
			short = entry
		}
		if used != test.entries || test.entries == 1 && short[12] != test.flags {
			t.Errorf("%s: %d entries, flags %#x", test.name, used, short[12])
		}
	}
	fs, root = mounted(t, fs.device.(*memoryDevice))
	got := listNames(t, fs, root)
	for _, want := range []string{"lower.txt", "UPPER.TXT", "mixa.TXT", "MIXB.txt", "ReadMe.txt", "123.456", "a$%'@~!.{}"} {
		found := false
		for _, name := range got {
			found = found || name == want
		}
		if !found {
			t.Errorf("%s missing from %q", want, got)
		}
	}
}

func TestShortNameBytesFromOtherSystems(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	dir, _ := fs.openDirectory(0)
	put := func(index int, short string) {
		var entry [32]byte
		copy(entry[:11], short)
		entry[11] = attrArchive
		fs.writeEntry(&dir, index, entry)
	}
	put(0, "\x05BC     TXT")          // 0x05 stands for a leading 0xe5
	put(1, "CAF\x82    TXT")          // code page 437 é
	put(2, "NOEXT      ")             // no extension: no dot
	put(3, "\x8e\x99\x9a     \x9b  ") // ÄÖÜ.¢
	put(4, "BAD\x00    TXT")          // not a name: hidden, and its slot stays in use
	put(5, "A*B     TXT")
	got := listNames(t, fs, root)
	want := []string{"σBC.TXT", "CAFé.TXT", "NOEXT", "ÄÖÜ.¢"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("listed %q, want %q", got, want)
	}
	for _, name := range got {
		if !utf8.ValidString(name) {
			t.Fatalf("%q is not UTF-8", name)
		}
		if _, status := fs.OpenName(root, mustName(t, name)); status != ramfs.StatusOK {
			t.Fatalf("open %q: %v", name, status)
		}
	}
	if _, status := fs.OpenName(root, mustName(t, "café.txt")); status != ramfs.StatusOK {
		t.Fatalf("case-insensitive OEM match: %v", status)
	}
	id := writeFile(t, fs, root, "new", "x")
	if _, index, _, _ := fs.node(id); index != 6 {
		t.Fatalf("a create took slot %d, not the one after the unnamed entries", index)
	}
}

// TestNewNamesCannotCollideWithAnAlias covers the rule that an entry answers
// to its 8.3 alias as well: creating an 8.3 name that is already some long
// name's alias is refused instead of writing a second entry with the same
// short name.
func TestNewNamesCannotCollideWithAnAlias(t *testing.T) {
	device := formattedDevice()
	fs, root := mounted(t, device)
	writeFile(t, fs, root, "Long File Name.txt", "long")
	if got := shortNames(t, fs, 0); len(got) != 1 || got[0] != "LONGFI~1TXT" {
		t.Fatalf("alias %q", got)
	}
	if _, status := fs.CreateName(root, mustName(t, "longfi~1.txt")); status != ramfs.StatusAlreadyExists {
		t.Fatalf("create over an alias: %v", status)
	}
	if got := readFile(t, fs, mustOpen(t, fs, root, "LONGFI~1.TXT")); got != "long" {
		t.Fatalf("open by alias read %q", got)
	}
	other := writeFile(t, fs, root, "other", "o")
	if status := fs.RenameName(other, root, mustName(t, "LONGFI~1.TXT")); status != ramfs.StatusAlreadyExists {
		t.Fatalf("rename over an alias: %v", status)
	}
	// Many names that share an alias prefix and differ only outside ASCII
	// still get distinct aliases.
	for i := 0; i < 40; i++ {
		writeFile(t, fs, root, fmt.Sprintf("Отчёт %02d.txt", i), fmt.Sprint(i))
	}
	seen := map[string]bool{}
	for _, short := range shortNames(t, fs, 0) {
		if seen[short] {
			t.Fatalf("alias %q used twice", short)
		}
		seen[short] = true
	}
	check(t, device)
}

func TestOEMTableMatchesIconv(t *testing.T) {
	path, err := exec.LookPath("iconv")
	if err != nil {
		t.Skip("no iconv on this host")
	}
	input := make([]byte, 128)
	for i := range input {
		input[i] = byte(0x80 + i)
	}
	command := exec.Command(path, "-f", "CP437", "-t", "UTF-8")
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	if err != nil {
		t.Skipf("iconv has no CP437: %v", err)
	}
	runes := []rune(string(output))
	if len(runes) != 128 {
		t.Fatalf("iconv produced %d characters", len(runes))
	}
	for i, value := range runes {
		if got := oemRune(byte(0x80 + i)); got != value {
			t.Errorf("0x%02x maps to %U, iconv says %U", 0x80+i, got, value)
		}
	}
}

func TestUTFConversions(t *testing.T) {
	for _, text := range append(unicodeNames, "a", "ÿ", "߿", "ࠀ", "￿", "\U00010000", "\U0010ffff") {
		units := utf16.Encode([]rune(text))
		var target [maxLongName]byte
		size, ok := utf16To8(units, target[:])
		if !ok || string(target[:size]) != text {
			t.Fatalf("%q: %q %v", text, target[:size], ok)
		}
		for rest := []byte(text); len(rest) != 0; {
			value, size := decodeRune(rest)
			want, wantSize := utf8.DecodeRune(rest)
			if value != want || size != wantSize {
				t.Fatalf("%q: decoded %U/%d, want %U/%d", text, value, size, want, wantSize)
			}
			rest = rest[size:]
		}
	}
	var small [3]byte
	if _, ok := utf16To8(utf16.Encode([]rune("ab€")), small[:]); ok {
		t.Fatal("a name longer than its target was accepted")
	}
	// Folding is the generated Unicode simple upper-case mapping for every
	// scalar value, not a hand-picked set of scripts.
	for value := rune(0); value <= maxRune; value++ {
		if value >= surrogateHigh && value < surrogateEnd {
			continue
		}
		if got, want := foldRune(value), unicode.ToUpper(value); got != want {
			t.Fatalf("%U folds to %U, want %U", value, got, want)
		}
	}
}
