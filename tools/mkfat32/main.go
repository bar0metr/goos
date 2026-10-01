// mkfat32 creates the FAT32 volume QEMU attaches as the virtio-blk disk: on the
// whole disk, or in the one partition of an MBR or GPT table (ADR 0115). An
// existing image holding a FAT32 volume the system would choose is left alone,
// so the data on the development disk survives rebuilds; delete it to get a
// fresh volume. With -new an existing file of any kind is refused instead, so
// a disk is never formatted over by mistake (ADR 0175).
package main

import (
	"flag"
	"fmt"
	"goos/kernel/ramfs"
	blockproto "goos/userspace/block"
	"goos/userspace/fat32"
	"goos/userspace/partition"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// fileDevice is a host file as the device fat32.Format writes to.
type fileDevice struct {
	file *os.File
	size uint64
}

func (device fileDevice) ReadAt(offset uint64, target []byte) bool {
	_, err := device.file.ReadAt(target, int64(offset))
	return err == nil
}
func (device fileDevice) WriteAt(offset uint64, data []byte) bool {
	_, err := device.file.WriteAt(data, int64(offset))
	return err == nil
}
func (device fileDevice) Flush() bool      { return device.file.Sync() == nil }
func (device fileDevice) Capacity() uint64 { return device.size }

// sliceDevice is one volume of the image, reached the way the block service
// lets the filesystem service reach it: addressed from zero, bounded by the
// slice.
type sliceDevice struct {
	device fileDevice
	slice  blockproto.Slice
}

func (device sliceDevice) ReadAt(offset uint64, target []byte) bool {
	at, ok := device.slice.Translate(offset, uint64(len(target)))
	return ok && device.device.ReadAt(at, target)
}
func (device sliceDevice) WriteAt(offset uint64, data []byte) bool {
	at, ok := device.slice.Translate(offset, uint64(len(data)))
	return ok && device.slice.Writable && device.device.WriteAt(at, data)
}
func (device sliceDevice) Flush() bool      { return device.device.Flush() }
func (device sliceDevice) Capacity() uint64 { return device.slice.Length }

// blockSize is the virtio-blk logical block size, and so the GPT's.
const blockSize = 512

// partitionStart is where the one partition begins: 1 MiB, as every current
// partitioning tool aligns it.
const partitionStart = (1 << 20) / blockSize

// The GPT's GUIDs are fixed, so that images are reproducible.
var (
	diskGUID      = partition.GUID{0x47, 0x4f, 0x4f, 0x53, 0x44, 0x49, 0x53, 0x4b, 0x00, 0x01, 0x04, 0x00, 0x00, 0x00, 0x00, 0x01}
	partitionGUID = partition.GUID{0x47, 0x4f, 0x4f, 0x53, 0x50, 0x41, 0x52, 0x54, 0x00, 0x01, 0x04, 0x00, 0x00, 0x00, 0x00, 0x01}
)

// volume finds the volume the system would choose on the image: its table
// scanned and the selector resolved as the block service does.
func volume(device fileDevice) (sliceDevice, error) {
	var scanner partition.Scanner
	var table partition.Table
	if status := scanner.Scan(device, device.size, blockSize, &table); status != partition.StatusOK {
		return sliceDevice{}, fmt.Errorf("partition table unusable (status %d)", status)
	}
	chosen, status := partition.Select(&table, &partition.Selector{})
	if status != partition.StatusOK {
		return sliceDevice{}, fmt.Errorf("no volume in the %s table (status %d)", table.Scheme, status)
	}
	return sliceDevice{device: device, slice: blockproto.Slice{Start: chosen.Start, Length: chosen.Length, Writable: true}}, nil
}

func main() {
	size := flag.Uint64("size", 512, "volume size in MiB")
	sector := flag.Uint("sector-size", 512, "logical sector size in bytes (512, 1024, 2048 or 4096)")
	perCluster := flag.Uint("cluster-sectors", 8, "sectors per cluster (power of two, 1-128)")
	fats := flag.Uint("fats", 2, "number of FAT copies (1 or 2)")
	scheme := flag.String("table", "none", "partition table: none (whole disk), mbr or gpt")
	var puts []string
	flag.Func("put", "copy HOSTFILE:PATH onto the volume, creating directories (repeatable)", func(value string) error {
		if !strings.Contains(value, ":") {
			return fmt.Errorf("want HOSTFILE:PATH")
		}
		puts = append(puts, value)
		return nil
	})
	var gets []string
	flag.Func("get", "copy PATH:HOSTFILE from the volume to the host (repeatable)", func(value string) error {
		if !strings.Contains(value, ":") {
			return fmt.Errorf("want PATH:HOSTFILE")
		}
		gets = append(gets, value)
		return nil
	})
	tree := flag.String("tree", "", "copy the host directory `DIR` onto the volume root, empty directories included, in name order")
	fresh := flag.Bool("new", false, "refuse an IMAGE that already exists rather than reuse or reformat it")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mkfat32 [-new] [-size MiB] [-table none|mbr|gpt] [-sector-size N] [-cluster-sectors N] [-fats N] [-tree DIR] [-put HOSTFILE:PATH]... [-get PATH:HOSTFILE]... IMAGE")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || *perCluster == 0 || *perCluster > 128 || *fats == 0 || *fats > 2 || *sector > 4096 ||
		*scheme != "none" && *scheme != "mbr" && *scheme != "gpt" {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)
	if *fresh {
		if _, err := os.Lstat(path); err == nil {
			fail(fmt.Errorf("%s already exists; refusing to overwrite it", path))
		}
	}
	if !valid(path) {
		create(path, *size, *scheme, *sector, *perCluster, *fats)
	}
	if *tree != "" {
		entries, err := treeEntries(*tree)
		if err != nil {
			fail(err)
		}
		puts = append(entries, puts...)
	}
	if len(puts) != 0 || len(gets) != 0 {
		transfer(path, puts, gets)
	}
}

// treeEntries lists what -tree copies: every file of root as HOSTFILE:PATH,
// and every directory as :PATH, which makes it without a file. WalkDir visits
// in lexical order, so the volume is written the same way every time.
func treeEntries(root string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(root, func(host string, entry fs.DirEntry, err error) error {
		if err != nil || host == root {
			return err
		}
		relative, err := filepath.Rel(root, host)
		if err != nil {
			return err
		}
		target := "/" + filepath.ToSlash(relative)
		switch {
		case entry.IsDir():
			entries = append(entries, ":"+target)
		case entry.Type().IsRegular():
			entries = append(entries, host+":"+target)
		default:
			return fmt.Errorf("%s: not a regular file or directory", host)
		}
		return nil
	})
	return entries, err
}

// create writes the partition table, if any, and formats the volume.
func create(path string, size uint64, scheme string, sector, perCluster, fats uint) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	bytes := size << 20
	if err = file.Truncate(int64(bytes)); err != nil {
		fail(err)
	}
	device := fileDevice{file: file, size: bytes}
	blocks := bytes / blockSize
	switch scheme {
	case "mbr":
		if blocks-partitionStart > 0xffffffff || !partition.WriteMBR(device, blockSize, []partition.MBRPartition{
			{Type: 0x0c, Start: partitionStart, Count: uint32(blocks - partitionStart)}}) {
			fail(fmt.Errorf("cannot write an MBR on %s", path))
		}
	case "gpt":
		_, last := partition.GPTUsable(bytes, blockSize)
		if !partition.WriteGPT(device, bytes, blockSize, diskGUID, []partition.GPTPartition{
			{Type: partition.TypeBasicData, Unique: partitionGUID, First: partitionStart, Last: last, Name: "GoOS data"}}) {
			fail(fmt.Errorf("cannot write a GPT on %s", path))
		}
	}
	target, err := volume(device)
	if err != nil {
		fail(err)
	}
	layout := fat32.Layout{BytesPerSector: uint16(sector), SectorsPerCluster: uint8(perCluster), FATs: uint8(fats),
		HiddenSectors: uint32(target.slice.Start / uint64(max(sector, 512)))}
	if !fat32.Format(target, layout) {
		fail(fmt.Errorf("cannot format %s as FAT32 (%d MiB, %s, %+v)", path, size, scheme, layout))
	}
}

// transfer mounts the volume at path, copies each HOSTFILE:PATH onto it, and
// then each PATH:HOSTFILE off it.
func transfer(path string, puts []string, gets []string) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		fail(err)
	}
	target, err := volume(fileDevice{file: file, size: uint64(info.Size())})
	if err != nil {
		fail(fmt.Errorf("%s: %v", path, err))
	}
	var volume fat32.FileSystem
	root, ok := volume.Mount(target)
	if !ok {
		fail(fmt.Errorf("%s is not a FAT32 volume this system can mount", path))
	}
	for _, entry := range puts {
		separator := strings.Index(entry, ":")
		if err := putFile(&volume, root, entry[:separator], entry[separator+1:]); err != nil {
			fail(err)
		}
	}
	for _, entry := range gets {
		separator := strings.Index(entry, ":")
		if err := getFile(&volume, root, entry[:separator], entry[separator+1:]); err != nil {
			fail(err)
		}
	}
}

// getFile copies the file at path on the volume to the host file target.
func getFile(volume *fat32.FileSystem, root ramfs.NodeID, path, target string) error {
	node := root
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		name, ok := ramfs.MakeName([]byte(part))
		if !ok {
			return fmt.Errorf("invalid name %q in %s", part, path)
		}
		next, status := volume.OpenName(node, name)
		if status != ramfs.StatusOK {
			return fmt.Errorf("%s: status %d", path, status)
		}
		node = next
	}
	stat, status := volume.Stat(node)
	if status != ramfs.StatusOK || stat.Kind != ramfs.KindFile {
		return fmt.Errorf("%s is not a file", path)
	}
	data := make([]byte, stat.Size)
	if count, status := volume.Read(node, 0, data); status != ramfs.StatusOK || uint64(count) != stat.Size {
		return fmt.Errorf("%s: read %d of %d bytes, status %d", path, count, stat.Size, status)
	}
	return os.WriteFile(target, data, 0o644)
}

// putFile copies the host file at source to path on the volume, creating the
// directories on the way and replacing a file already there. With no source
// it makes path a directory.
func putFile(volume *fat32.FileSystem, root ramfs.NodeID, source, path string) error {
	var data []byte
	if source != "" {
		var err error
		if data, err = os.ReadFile(source); err != nil {
			return err
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	parent := root
	for index, part := range parts {
		name, ok := ramfs.MakeName([]byte(part))
		if !ok {
			return fmt.Errorf("invalid name %q in %s", part, path)
		}
		if index == len(parts)-1 && source != "" {
			id, status := volume.OpenName(parent, name)
			if status == ramfs.StatusNotFound {
				id, status = volume.CreateName(parent, name)
			}
			if status == ramfs.StatusOK {
				status = volume.Truncate(id, 0)
			}
			if status != ramfs.StatusOK {
				return fmt.Errorf("%s: status %d", path, status)
			}
			if count, status := volume.Write(id, 0, data); status != ramfs.StatusOK || count != len(data) {
				return fmt.Errorf("%s: wrote %d of %d bytes, status %d", path, count, len(data), status)
			}
			return nil
		}
		next, status := volume.OpenName(parent, name)
		if status == ramfs.StatusNotFound {
			next, status = volume.MkdirName(parent, name)
		}
		if status != ramfs.StatusOK {
			return fmt.Errorf("%s: directory %q: status %d", path, part, status)
		}
		parent = next
	}
	return nil
}

// valid reports whether path already holds a FAT32 volume, of any geometry,
// where the system would look for one.
func valid(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	target, err := volume(fileDevice{file: file, size: uint64(info.Size())})
	if err != nil {
		return false
	}
	var data [512]byte
	if !target.ReadAt(0, data[:]) {
		return false
	}
	return data[510] == 0x55 && data[511] == 0xaa && string(data[82:90]) == "FAT32   "
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
