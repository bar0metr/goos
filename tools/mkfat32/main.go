// mkfat32 creates the FAT32 volume QEMU attaches as the virtio-blk disk. An
// existing FAT32 image is left alone, so the data on the development disk
// survives rebuilds; delete it to get a fresh volume.
package main

import (
	"flag"
	"fmt"
	"goos/kernel/ramfs"
	"goos/userspace/fat32"
	"os"
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

func main() {
	size := flag.Uint64("size", 512, "volume size in MiB")
	perCluster := flag.Uint("cluster-sectors", 8, "512-byte sectors per cluster (power of two, 1-128)")
	fats := flag.Uint("fats", 2, "number of FAT copies (1 or 2)")
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
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mkfat32 [-size MiB] [-cluster-sectors N] [-fats N] [-put HOSTFILE:PATH]... [-get PATH:HOSTFILE]... IMAGE")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || *perCluster == 0 || *perCluster > 128 || *fats == 0 || *fats > 2 {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)
	if !valid(path) {
		create(path, *size, *perCluster, *fats)
	}
	if len(puts) != 0 || len(gets) != 0 {
		transfer(path, puts, gets)
	}
}

// create formats a new volume at path.
func create(path string, size uint64, perCluster uint, fats uint) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	bytes := size << 20
	if err = file.Truncate(int64(bytes)); err != nil {
		fail(err)
	}
	layout := fat32.Layout{SectorsPerCluster: uint8(perCluster), FATs: uint8(fats)}
	if !fat32.Format(fileDevice{file: file, size: bytes}, layout) {
		fail(fmt.Errorf("cannot format %s as FAT32 (%d MiB, %+v)", path, size, layout))
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
	var volume fat32.FileSystem
	root, ok := volume.Mount(fileDevice{file: file, size: uint64(info.Size())})
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
// directories on the way and replacing a file already there.
func putFile(volume *fat32.FileSystem, root ramfs.NodeID, source, path string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	parent := root
	for index, part := range parts {
		name, ok := ramfs.MakeName([]byte(part))
		if !ok {
			return fmt.Errorf("invalid name %q in %s", part, path)
		}
		if index == len(parts)-1 {
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

// valid reports whether path already holds a FAT32 volume of any geometry.
func valid(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var data [512]byte
	if _, err = file.ReadAt(data[:], 0); err != nil {
		return false
	}
	return data[510] == 0x55 && data[511] == 0xaa && string(data[82:90]) == "FAT32   "
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
