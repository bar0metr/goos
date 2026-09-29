package main

import (
	"archive/tar"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"goos/kernel/boot"
	"goos/userspace/commands"
)

// usage names the fixed system images in order, then the directory of
// command images and the command index the build generated from them.
const usage = "usage: initramfs INIT_IMAGE SERVICE_IMAGE VFS_IMAGE SHELL_IMAGE BLOCK_IMAGE VIRTIO_BLOCK_IMAGE LOG_IMAGE CONFIG_IMAGE TTY_IMAGE FAT32_IMAGE COMMAND_DIR COMMAND_INDEX OUTPUT"

func main() {
	if len(os.Args) != 14 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	epoch := time.Unix(0, 0)
	members := []member{
		directory("system/"),
		directory("system/bin/"),
		directory("system/linux/"),
		file("system/linux/phase6-probe", 0o555, linuxProbe()),
		// The message of the day belongs to the immutable system image rather
		// than to the volatile root, because /system is a read-only mount of
		// this archive.
		file("system/motd", 0o444, []byte("Welcome to GoOS\n")),
	}
	for index, name := range []string{"init", "fault-probe", "vfs", "shell", "block", "virtio-blk", "log", "config", "tty", "fat32"} {
		members = append(members, file("system/"+name, 0o555, read(os.Args[1+index])))
	}
	index := read(os.Args[12])
	members = append(members, file("system/commands", 0o444, index))
	binaries, err := commandImages(os.Args[11], index)
	if err != nil {
		fail(err)
	}
	members = append(members, binaries...)
	output, err := os.Create(os.Args[13])
	if err != nil {
		fail(err)
	}
	writer := tar.NewWriter(output)
	for _, entry := range members {
		entry.header.ModTime = epoch
		entry.header.Format = tar.FormatUSTAR
		if err := writer.WriteHeader(&entry.header); err != nil {
			fail(err)
		}
		if entry.header.Typeflag == tar.TypeReg {
			if _, err := writer.Write(entry.data); err != nil {
				fail(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		fail(err)
	}
	if err := output.Close(); err != nil {
		fail(err)
	}
	// The kernel reads at most boot.MaxInitramfs and refuses to boot from a
	// larger archive, so the build refuses to produce one.
	written, err := os.Stat(os.Args[13])
	if err != nil {
		fail(err)
	}
	if err := checkSize(written.Size()); err != nil {
		os.Remove(os.Args[13])
		fail(err)
	}
}

// linuxProbe is a static ELF64 ET_EXEC whose entry loops forever. Validation
// launches and kills it to exercise policy, mapping and initial-stack setup
// before semantic Linux exit support exists.
func linuxProbe() []byte {
	const (
		base       = uint64(0x400000)
		headerSize = 64
		phdrSize   = 56
	)
	image := make([]byte, headerSize+phdrSize+2)
	copy(image[:], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(image[16:], 2)
	binary.LittleEndian.PutUint16(image[18:], 62)
	binary.LittleEndian.PutUint32(image[20:], 1)
	binary.LittleEndian.PutUint64(image[24:], base+headerSize+phdrSize)
	binary.LittleEndian.PutUint64(image[32:], headerSize)
	binary.LittleEndian.PutUint16(image[52:], headerSize)
	binary.LittleEndian.PutUint16(image[54:], phdrSize)
	binary.LittleEndian.PutUint16(image[56:], 1)
	program := image[headerSize:]
	binary.LittleEndian.PutUint32(program[0:], 1)
	binary.LittleEndian.PutUint32(program[4:], 5)
	binary.LittleEndian.PutUint64(program[16:], base)
	binary.LittleEndian.PutUint64(program[24:], base)
	binary.LittleEndian.PutUint64(program[32:], uint64(len(image)))
	binary.LittleEndian.PutUint64(program[40:], uint64(len(image)))
	binary.LittleEndian.PutUint64(program[48:], 0x1000)
	copy(image[headerSize+phdrSize:], []byte{0xeb, 0xfe})
	return image
}

func checkSize(size int64) error {
	if size <= 0 || uint64(size) > boot.MaxInitramfs {
		return fmt.Errorf("initramfs is %d bytes, over the %d the kernel loads (boot.MaxInitramfs)", size, boot.MaxInitramfs)
	}
	return nil
}

type member struct {
	header tar.Header
	data   []byte
}

func directory(name string) member {
	return member{header: tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}}
}

func file(name string, mode int64, data []byte) member {
	return member{header: tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}, data: data}
}

func read(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	return data
}

// commandImages returns one /system/bin member per image in directory, in
// name order, and refuses an image the index does not describe or an index
// entry without an image: the two are built from the same tree and must agree,
// or PID 1 would grant from a description of a different program.
func commandImages(directory string, index []byte) ([]member, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var members []member
	for _, name := range names {
		if _, found := commands.Lookup(index, []byte(name)); !found {
			return nil, fmt.Errorf("%s: image without an index entry", name)
		}
		members = append(members, file("system/bin/"+name, 0o555, read(filepath.Join(directory, name))))
	}
	for offset := 0; ; {
		entry, next, ok := commands.Next(index, offset)
		if !ok {
			if offset != len(index) {
				return nil, fmt.Errorf("command index is malformed at byte %d", offset)
			}
			return members, nil
		}
		if sort.SearchStrings(names, string(entry.Name)) == len(names) || names[sort.SearchStrings(names, string(entry.Name))] != string(entry.Name) {
			return nil, fmt.Errorf("%s: index entry without an image", entry.Name)
		}
		offset = next
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
