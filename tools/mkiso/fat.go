package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// The EFI system partition is an 8 MiB FAT16 volume with 1 KiB clusters.
// Every field the image contains is derived from its inputs and the build
// timestamp, so identical inputs give an identical volume.
const (
	fatSectorSize     = 512
	fatImageSize      = 8 * 1024 * 1024
	fatTotalSectors   = fatImageSize / fatSectorSize
	fatClusterSectors = 2
	fatClusterSize    = fatClusterSectors * fatSectorSize
	fatReserved       = 1
	fatCopies         = 2
	fatRootEntries    = 512
	fatRootSectors    = fatRootEntries * 32 / fatSectorSize
	fatVolumeLabel    = "GOOS_EFI"
	// The serial number is fixed rather than random: nothing uses it to tell
	// volumes apart, and a random one would make every build differ.
	fatVolumeID = 0x474f4f53

	attrDirectory = 0x10
	attrVolumeID  = 0x08
	attrArchive   = 0x20
	attrLongName  = 0x0f
)

// fatNode is one file or directory of the volume being built.
type fatNode struct {
	name     string
	data     []byte
	children []*fatNode
	dir      bool
	cluster  uint16
	clusters int
}

func (n *fatNode) child(name string) *fatNode {
	for _, c := range n.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

// buildFAT returns the volume holding each file at its slash-separated path.
func buildFAT(files map[string][]byte, stamp time.Time) ([]byte, error) {
	root := &fatNode{dir: true}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		parts := strings.Split(path, "/")
		dir := root
		for _, part := range parts[:len(parts)-1] {
			next := dir.child(part)
			if next == nil {
				next = &fatNode{name: part, dir: true}
				dir.children = append(dir.children, next)
			} else if !next.dir {
				return nil, fmt.Errorf("%s: %s is a file", path, part)
			}
			dir = next
		}
		name := parts[len(parts)-1]
		if name == "" || dir.child(name) != nil {
			return nil, fmt.Errorf("%s: invalid or duplicate path", path)
		}
		dir.children = append(dir.children, &fatNode{name: name, data: files[path]})
	}

	fatSectors, clusterCount := fatGeometry()
	dataStart := fatReserved + fatCopies*fatSectors + fatRootSectors
	image := make([]byte, fatImageSize)
	table := make([]uint16, clusterCount+2)
	table[0], table[1] = 0xfff8, 0xffff
	next := uint16(2)

	// Clusters are handed out depth-first in name order.
	var allocate func(n *fatNode) error
	allocate = func(n *fatNode) error {
		size := len(n.data)
		if n.dir {
			sortChildren(n.children)
			size = (entryCount(n.children) + 2) * 32
		}
		n.clusters = (size + fatClusterSize - 1) / fatClusterSize
		if n.dir && n.clusters == 0 {
			n.clusters = 1
		}
		if n.clusters > 0 {
			if int(next)+n.clusters > len(table) {
				return errors.New("files do not fit in the EFI system partition")
			}
			n.cluster = next
			for i := 0; i < n.clusters; i++ {
				c := int(next) + i
				if i == n.clusters-1 {
					table[c] = 0xffff
				} else {
					table[c] = uint16(c + 1)
				}
			}
			next += uint16(n.clusters)
		}
		for _, c := range n.children {
			if err := allocate(c); err != nil {
				return err
			}
		}
		return nil
	}
	sortChildren(root.children)
	for _, c := range root.children {
		if err := allocate(c); err != nil {
			return nil, err
		}
	}

	date, clock := fatTimestamp(stamp)
	var write func(n *fatNode, parent uint16)
	write = func(n *fatNode, parent uint16) {
		if !n.dir {
			offset := (dataStart + (int(n.cluster)-2)*fatClusterSectors) * fatSectorSize
			copy(image[offset:], n.data)
			return
		}
		offset := (dataStart + (int(n.cluster)-2)*fatClusterSectors) * fatSectorSize
		entries := image[offset : offset+n.clusters*fatClusterSize]
		at := 0
		at += putEntry(entries[at:], dotName("."), attrDirectory, n.cluster, 0, date, clock)
		at += putEntry(entries[at:], dotName(".."), attrDirectory, parent, 0, date, clock)
		putChildren(entries[at:], n.children, date, clock)
		for _, c := range n.children {
			write(c, n.cluster)
		}
	}

	rootOffset := (fatReserved + fatCopies*fatSectors) * fatSectorSize
	rootEntries := image[rootOffset : rootOffset+fatRootSectors*fatSectorSize]
	var label [11]byte
	copy(label[:], fmt.Sprintf("%-11s", fatVolumeLabel))
	at := putEntry(rootEntries, label, attrVolumeID, 0, 0, date, clock)
	if (entryCount(root.children)+1)*32 > len(rootEntries) {
		return nil, errors.New("too many files in the root directory")
	}
	putChildren(rootEntries[at:], root.children, date, clock)
	for _, c := range root.children {
		write(c, 0)
	}

	putBootSector(image[:fatSectorSize], fatSectors)
	for copyIndex := 0; copyIndex < fatCopies; copyIndex++ {
		offset := (fatReserved + copyIndex*fatSectors) * fatSectorSize
		for i, value := range table {
			binary.LittleEndian.PutUint16(image[offset+i*2:], value)
		}
	}
	return image, nil
}

// fatGeometry sizes one FAT so that it covers every cluster the remaining
// sectors hold. The cluster count lands in the FAT16 range.
func fatGeometry() (fatSectors, clusters int) {
	fatSectors = 1
	for {
		data := fatTotalSectors - fatReserved - fatRootSectors - fatCopies*fatSectors
		clusters = data / fatClusterSectors
		need := ((clusters+2)*2 + fatSectorSize - 1) / fatSectorSize
		if need <= fatSectors {
			return fatSectors, clusters
		}
		fatSectors = need
	}
}

func putBootSector(sector []byte, fatSectors int) {
	copy(sector[0:3], []byte{0xeb, 0x3c, 0x90})
	copy(sector[3:11], "MSWIN4.1")
	binary.LittleEndian.PutUint16(sector[11:], fatSectorSize)
	sector[13] = fatClusterSectors
	binary.LittleEndian.PutUint16(sector[14:], fatReserved)
	sector[16] = fatCopies
	binary.LittleEndian.PutUint16(sector[17:], fatRootEntries)
	binary.LittleEndian.PutUint16(sector[19:], fatTotalSectors)
	sector[21] = 0xf8
	binary.LittleEndian.PutUint16(sector[22:], uint16(fatSectors))
	binary.LittleEndian.PutUint16(sector[24:], 32)
	binary.LittleEndian.PutUint16(sector[26:], 64)
	sector[36] = 0x80
	sector[38] = 0x29
	binary.LittleEndian.PutUint32(sector[39:], fatVolumeID)
	copy(sector[43:54], fmt.Sprintf("%-11s", fatVolumeLabel))
	copy(sector[54:62], "FAT16   ")
	sector[510], sector[511] = 0x55, 0xaa
}

func sortChildren(children []*fatNode) {
	sort.Slice(children, func(i, j int) bool { return children[i].name < children[j].name })
}

// entryCount is the number of directory entries children occupy, long-name
// entries included.
func entryCount(children []*fatNode) int {
	count := 0
	for _, c := range children {
		count += 1 + (len(utf16.Encode([]rune(c.name)))+12)/13
	}
	return count
}

// putChildren writes each child's long-name entries and short entry, in the
// name order sortChildren gave them.
func putChildren(entries []byte, children []*fatNode, date, clock uint16) {
	used := map[[11]byte]bool{}
	at := 0
	for _, c := range children {
		short, exact := shortName(c.name, used)
		used[short] = true
		if !exact {
			at += putLongName(entries[at:], c.name, short)
		}
		attr := byte(attrArchive)
		if c.dir {
			attr = attrDirectory
		}
		size := uint32(len(c.data))
		if c.dir {
			size = 0
		}
		at += putEntry(entries[at:], short, attr, c.cluster, size, date, clock)
	}
}

func putEntry(entry []byte, name [11]byte, attr byte, cluster uint16, size uint32, date, clock uint16) int {
	copy(entry[0:11], name[:])
	entry[11] = attr
	binary.LittleEndian.PutUint16(entry[14:], clock)
	binary.LittleEndian.PutUint16(entry[16:], date)
	binary.LittleEndian.PutUint16(entry[18:], date)
	binary.LittleEndian.PutUint16(entry[22:], clock)
	binary.LittleEndian.PutUint16(entry[24:], date)
	binary.LittleEndian.PutUint16(entry[26:], cluster)
	binary.LittleEndian.PutUint32(entry[28:], size)
	return 32
}

func dotName(name string) [11]byte {
	var out [11]byte
	copy(out[:], fmt.Sprintf("%-11s", name))
	return out
}

// shortName returns the 8.3 entry name and whether it spells the name
// exactly, in which case no long-name entries are needed.
func shortName(name string, used map[[11]byte]bool) ([11]byte, bool) {
	base, ext := name, ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		base, ext = name[:dot], name[dot+1:]
	}
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToUpper(s) {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_-~!#$%&'()@^`{}", r) {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	upperBase, upperExt := clean(base), clean(ext)
	var out [11]byte
	exact := upperBase == base && upperExt == ext && len(base) <= 8 && len(ext) <= 3 && base != "" && strings.Count(name, ".") <= 1
	if exact {
		copy(out[:], fmt.Sprintf("%-8s%-3s", base, ext))
		return out, true
	}
	if len(upperExt) > 3 {
		upperExt = upperExt[:3]
	}
	for n := 1; ; n++ {
		tail := fmt.Sprintf("~%d", n)
		prefix := upperBase
		if len(prefix) > 8-len(tail) {
			prefix = prefix[:8-len(tail)]
		}
		copy(out[:], fmt.Sprintf("%-8s%-3s", prefix+tail, upperExt))
		if !used[out] {
			return out, false
		}
	}
}

func shortChecksum(name [11]byte) byte {
	var sum byte
	for _, c := range name {
		sum = (sum&1)<<7 + sum>>1 + c
	}
	return sum
}

// putLongName writes the VFAT long-name entries for name, last part first.
func putLongName(entries []byte, name string, short [11]byte) int {
	units := utf16.Encode([]rune(name))
	count := (len(units) + 12) / 13
	if len(units)%13 != 0 {
		units = append(units, 0)
		for len(units)%13 != 0 {
			units = append(units, 0xffff)
		}
	}
	sum := shortChecksum(short)
	offsets := []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}
	at := 0
	for seq := count; seq >= 1; seq-- {
		entry := entries[at : at+32]
		entry[0] = byte(seq)
		if seq == count {
			entry[0] |= 0x40
		}
		entry[11] = attrLongName
		entry[13] = sum
		for i, offset := range offsets {
			binary.LittleEndian.PutUint16(entry[offset:], units[(seq-1)*13+i])
		}
		at += 32
	}
	return at
}

// fatTimestamp encodes stamp as a FAT date and time, clamped to the FAT epoch
// of 1980.
func fatTimestamp(stamp time.Time) (date, clock uint16) {
	stamp = stamp.UTC()
	if stamp.Year() < 1980 {
		return 1<<5 | 1, 0
	}
	if stamp.Year() > 2107 {
		stamp = time.Date(2107, 12, 31, 23, 59, 58, 0, time.UTC)
	}
	date = uint16(stamp.Year()-1980)<<9 | uint16(stamp.Month())<<5 | uint16(stamp.Day())
	clock = uint16(stamp.Hour())<<11 | uint16(stamp.Minute())<<5 | uint16(stamp.Second()/2)
	return date, clock
}
