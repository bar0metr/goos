// mkfat32 creates the FAT32 volume QEMU attaches as the virtio-blk disk. An
// existing FAT32 image is left alone, so the data on the development disk
// survives rebuilds; delete it to get a fresh volume.
package main

import (
	"flag"
	"fmt"
	"goos/userspace/fat32"
	"os"
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
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mkfat32 [-size MiB] [-cluster-sectors N] [-fats N] IMAGE")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || *perCluster == 0 || *perCluster > 128 || *fats == 0 || *fats > 2 {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)
	if valid(path) {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	bytes := *size << 20
	if err = file.Truncate(int64(bytes)); err != nil {
		fail(err)
	}
	layout := fat32.Layout{SectorsPerCluster: uint8(*perCluster), FATs: uint8(*fats)}
	if !fat32.Format(fileDevice{file: file, size: bytes}, layout) {
		fail(fmt.Errorf("cannot format %s as FAT32 (%d MiB, %+v)", path, *size, layout))
	}
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
