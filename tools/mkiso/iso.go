package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The ISO is an ISO 9660 volume with one El Torito catalog whose only boot
// entry is the EFI system partition, marked with the UEFI platform ID. Its
// fixed layout:
//
//	0-15   system area (zero)
//	16     primary volume descriptor
//	17     El Torito boot record
//	18     volume descriptor set terminator
//	19     little-endian path table
//	20     big-endian path table
//	21     boot catalog
//	22..   directories, then file data in path order
const (
	isoSectorSize     = 2048
	isoPVDSector      = 16
	isoBootSector     = 17
	isoTermSector     = 18
	isoLPathSector    = 19
	isoMPathSector    = 20
	isoCatalogSector  = 21
	isoFirstDirSector = 22
	isoVolumeID       = "GOOS_EFI"
	uefiPlatform      = 0xef
	// El Torito counts the boot image in 512-byte virtual sectors.
	isoBootImageSectors = fatImageSize / 512
)

type isoNode struct {
	name     string // ISO 9660 identifier: upper case, files end in ";1"
	data     []byte
	children []*isoNode
	dir      bool
	sector   int
	size     int
	number   int // path table directory number
	parent   *isoNode
}

// isoIdentifier maps a file name to an ISO 9660 identifier. The volume holds
// few, fixed names, so a name that does not map cleanly is an error rather
// than something to mangle.
func isoIdentifier(name string, dir bool) (string, error) {
	upper := strings.ToUpper(name)
	for _, r := range upper {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.') {
			return "", fmt.Errorf("%q is not a valid ISO 9660 name", name)
		}
	}
	if dir {
		if strings.Contains(upper, ".") {
			return "", fmt.Errorf("directory %q contains a dot", name)
		}
		return upper, nil
	}
	if strings.Count(upper, ".") != 1 || len(upper) > 30 {
		return "", fmt.Errorf("%q is not a valid ISO 9660 file name", name)
	}
	return upper + ";1", nil
}

// buildISO returns the ISO holding files at their slash-separated paths, with
// the file at bootPath as the UEFI El Torito boot image.
func buildISO(files map[string][]byte, bootPath string, stamp time.Time) ([]byte, error) {
	if len(files[bootPath]) != fatImageSize {
		return nil, fmt.Errorf("boot image %s must be %d bytes", bootPath, fatImageSize)
	}
	root := &isoNode{dir: true}
	root.parent = root
	var boot *isoNode
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		parts := strings.Split(path, "/")
		dir := root
		for _, part := range parts[:len(parts)-1] {
			id, err := isoIdentifier(part, true)
			if err != nil {
				return nil, err
			}
			var next *isoNode
			for _, c := range dir.children {
				if c.name == id {
					next = c
				}
			}
			if next == nil {
				next = &isoNode{name: id, dir: true, parent: dir}
				dir.children = append(dir.children, next)
			} else if !next.dir {
				return nil, fmt.Errorf("%s: %s is a file", path, part)
			}
			dir = next
		}
		id, err := isoIdentifier(parts[len(parts)-1], false)
		if err != nil {
			return nil, err
		}
		for _, c := range dir.children {
			if c.name == id {
				return nil, fmt.Errorf("%s: duplicate path", path)
			}
		}
		node := &isoNode{name: id, data: files[path], parent: dir, size: len(files[path])}
		dir.children = append(dir.children, node)
		if path == bootPath {
			boot = node
		}
	}

	// Directories in path-table order: breadth first, children by name.
	dirs := []*isoNode{root}
	for i := 0; i < len(dirs); i++ {
		d := dirs[i]
		d.number = i + 1
		sort.Slice(d.children, func(a, b int) bool { return d.children[a].name < d.children[b].name })
		for _, c := range d.children {
			if c.dir {
				dirs = append(dirs, c)
			}
		}
	}
	sector := isoFirstDirSector
	for _, d := range dirs {
		d.size = isoSectorSize * sectorsFor(directorySize(d))
		d.sector = sector
		sector += d.size / isoSectorSize
	}
	var filesInOrder []*isoNode
	var walk func(d *isoNode)
	walk = func(d *isoNode) {
		for _, c := range d.children {
			if c.dir {
				walk(c)
			} else {
				filesInOrder = append(filesInOrder, c)
			}
		}
	}
	walk(root)
	for _, f := range filesInOrder {
		f.sector = sector
		sector += sectorsFor(f.size)
	}
	image := make([]byte, sector*isoSectorSize)

	pathTable := buildPathTable(dirs, binary.LittleEndian)
	if len(pathTable) > isoSectorSize {
		return nil, errors.New("path table does not fit in one sector")
	}
	copy(image[isoLPathSector*isoSectorSize:], pathTable)
	copy(image[isoMPathSector*isoSectorSize:], buildPathTable(dirs, binary.BigEndian))

	for _, d := range dirs {
		out := image[d.sector*isoSectorSize : d.sector*isoSectorSize+d.size]
		at := putRecord(out, d, "\x00", stamp)
		at += putRecord(out[at:], d.parent, "\x01", stamp)
		for _, c := range d.children {
			if at%isoSectorSize+recordSize(c.name) > isoSectorSize {
				at += isoSectorSize - at%isoSectorSize
			}
			at += putRecord(out[at:], c, c.name, stamp)
		}
	}
	for _, f := range filesInOrder {
		copy(image[f.sector*isoSectorSize:], f.data)
	}

	putPrimaryDescriptor(image[isoPVDSector*isoSectorSize:], root, sector, len(pathTable), stamp)
	putBootRecord(image[isoBootSector*isoSectorSize:])
	term := image[isoTermSector*isoSectorSize:]
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1
	putBootCatalog(image[isoCatalogSector*isoSectorSize:], boot.sector)
	return image, nil
}

func sectorsFor(size int) int { return (size + isoSectorSize - 1) / isoSectorSize }

func recordSize(name string) int {
	size := 33 + len(name)
	return size + size%2
}

// directorySize is the byte size of a directory's records. Records never
// straddle a sector boundary, so a record that would is moved to the next
// sector.
func directorySize(d *isoNode) int {
	size := recordSize("\x00") * 2
	for _, c := range d.children {
		r := recordSize(c.name)
		if size%isoSectorSize+r > isoSectorSize {
			size += isoSectorSize - size%isoSectorSize
		}
		size += r
	}
	return size
}

func putRecord(out []byte, node *isoNode, name string, stamp time.Time) int {
	size := recordSize(name)
	r := out[:size]
	r[0] = byte(size)
	putBoth32(r[2:], uint32(node.sector))
	putBoth32(r[10:], uint32(node.size))
	putRecordTime(r[18:25], stamp)
	if node.dir {
		r[25] = 0x02
	}
	putBoth16(r[28:], 1)
	r[32] = byte(len(name))
	copy(r[33:], name)
	return size
}

func buildPathTable(dirs []*isoNode, order binary.ByteOrder) []byte {
	var table []byte
	for _, d := range dirs {
		name := d.name
		if d.number == 1 {
			name = "\x00"
		}
		entry := make([]byte, 8+len(name)+len(name)%2)
		entry[0] = byte(len(name))
		order.PutUint32(entry[2:], uint32(d.sector))
		order.PutUint16(entry[6:], uint16(d.parent.number))
		copy(entry[8:], name)
		table = append(table, entry...)
	}
	return table
}

func putPrimaryDescriptor(d []byte, root *isoNode, sectors, pathTableSize int, stamp time.Time) {
	d[0] = 1
	copy(d[1:6], "CD001")
	d[6] = 1
	fill := func(from, to int, text string) {
		copy(d[from:to], fmt.Sprintf("%-*s", to-from, text))
	}
	fill(8, 40, "")
	fill(40, 72, isoVolumeID)
	putBoth32(d[80:], uint32(sectors))
	putBoth16(d[120:], 1)
	putBoth16(d[124:], 1)
	putBoth16(d[128:], isoSectorSize)
	putBoth32(d[132:], uint32(pathTableSize))
	binary.LittleEndian.PutUint32(d[140:], isoLPathSector)
	binary.BigEndian.PutUint32(d[148:], isoMPathSector)
	putRecord(d[156:190], root, "\x00", stamp)
	fill(190, 318, "")
	fill(318, 446, "")
	fill(446, 574, "")
	fill(574, 702, "GOOS")
	fill(702, 813, "")
	putVolumeTime(d[813:830], stamp)
	putVolumeTime(d[830:847], stamp)
	copy(d[847:864], "0000000000000000")
	putVolumeTime(d[864:881], stamp)
	d[881] = 1
}

func putBootRecord(d []byte) {
	copy(d[1:6], "CD001")
	d[6] = 1
	copy(d[7:], "EL TORITO SPECIFICATION")
	binary.LittleEndian.PutUint32(d[71:], isoCatalogSector)
}

// putBootCatalog writes the validation entry and the default entry: a
// no-emulation UEFI image of the whole EFI system partition.
func putBootCatalog(c []byte, imageSector int) {
	c[0] = 1
	c[1] = uefiPlatform
	c[30], c[31] = 0x55, 0xaa
	var sum uint16
	for offset := 0; offset < 32; offset += 2 {
		sum += binary.LittleEndian.Uint16(c[offset:])
	}
	binary.LittleEndian.PutUint16(c[28:], -sum)
	c[32] = 0x88
	binary.LittleEndian.PutUint16(c[32+6:], isoBootImageSectors)
	binary.LittleEndian.PutUint32(c[32+8:], uint32(imageSector))
}

func putRecordTime(out []byte, stamp time.Time) {
	stamp = stamp.UTC()
	year := stamp.Year() - 1900
	if year < 0 {
		year = 0
	}
	if year > 255 {
		year = 255
	}
	out[0] = byte(year)
	out[1] = byte(stamp.Month())
	out[2] = byte(stamp.Day())
	out[3] = byte(stamp.Hour())
	out[4] = byte(stamp.Minute())
	out[5] = byte(stamp.Second())
	out[6] = 0
}

func putVolumeTime(out []byte, stamp time.Time) {
	stamp = stamp.UTC()
	copy(out, fmt.Sprintf("%04d%02d%02d%02d%02d%02d00", stamp.Year(), int(stamp.Month()), stamp.Day(), stamp.Hour(), stamp.Minute(), stamp.Second()))
	out[16] = 0
}

func putBoth16(out []byte, value uint16) {
	binary.LittleEndian.PutUint16(out, value)
	binary.BigEndian.PutUint16(out[2:], value)
}

func putBoth32(out []byte, value uint32) {
	binary.LittleEndian.PutUint32(out, value)
	binary.BigEndian.PutUint32(out[4:], value)
}
