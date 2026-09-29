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
		file("system/linux/raw-probe", 0o555, linuxProbe()),
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

// linuxProbe is a static ELF64 ET_EXEC that exercises anonymous memory, brk,
// FS-base TLS state, stdio, and process exit using raw Linux syscalls.
func linuxProbe() []byte {
	const (
		base       = uint64(0x400000)
		headerSize = 64
		phdrSize   = 56
		phdrCount  = 2
		entry      = headerSize + phdrCount*phdrSize
		dataOffset = 0x1000
		data       = uint32(base + dataOffset)
		markerAt   = 0x800
		marker     = uint32(base + markerAt)
	)
	const markerText = "LINUX_MEMORY_TLS_OK\n"
	image := make([]byte, dataOffset)
	copy(image[:], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(image[16:], 2)
	binary.LittleEndian.PutUint16(image[18:], 62)
	binary.LittleEndian.PutUint32(image[20:], 1)
	binary.LittleEndian.PutUint64(image[24:], base+entry)
	binary.LittleEndian.PutUint64(image[32:], headerSize)
	binary.LittleEndian.PutUint16(image[52:], headerSize)
	binary.LittleEndian.PutUint16(image[54:], phdrSize)
	binary.LittleEndian.PutUint16(image[56:], phdrCount)
	program := image[headerSize:]
	binary.LittleEndian.PutUint32(program[0:], 1)
	binary.LittleEndian.PutUint32(program[4:], 5)
	binary.LittleEndian.PutUint64(program[16:], base)
	binary.LittleEndian.PutUint64(program[24:], base)
	binary.LittleEndian.PutUint64(program[32:], uint64(markerAt+len(markerText)))
	binary.LittleEndian.PutUint64(program[40:], uint64(markerAt+len(markerText)))
	binary.LittleEndian.PutUint64(program[48:], 0x1000)
	dataProgram := program[phdrSize:]
	binary.LittleEndian.PutUint32(dataProgram[0:], 1)
	binary.LittleEndian.PutUint32(dataProgram[4:], 6)
	binary.LittleEndian.PutUint64(dataProgram[8:], dataOffset)
	binary.LittleEndian.PutUint64(dataProgram[16:], uint64(data))
	binary.LittleEndian.PutUint64(dataProgram[24:], uint64(data))
	binary.LittleEndian.PutUint64(dataProgram[40:], 4096)
	binary.LittleEndian.PutUint64(dataProgram[48:], 0x1000)
	code := &probeProgram{}
	code.movEAX(12).xorEDI().syscall().testRAX().fail(0x8e) // brk(0), fail if <= 0
	code.emit(0x49, 0x89, 0xc4)                             // mov r12,rax
	code.emit(0x4c, 0x89, 0xe7, 0x48, 0x81, 0xc7, 0x00, 0x10, 0x00, 0x00)
	code.movEAX(12).syscall().emit(0x48, 0x39, 0xf8).fail(0x85)
	code.emit(0x41, 0xc6, 0x04, 0x24, 0x5a) // touch brk page
	code.movEAX(12).emit(0x4c, 0x89, 0xe7).syscall().emit(0x4c, 0x39, 0xe0).fail(0x85)

	code.movEAX(9).xorEDI().movESI(4096).movEDX(3)
	code.emit(0x41, 0xba).imm32(0x22)                          // r10d = MAP_PRIVATE|MAP_ANONYMOUS
	code.emit(0x49, 0xc7, 0xc0).imm32(0xffffffff)              // r8 = -1
	code.emit(0x45, 0x31, 0xc9).syscall().testRAX().fail(0x88) // js
	code.emit(0x49, 0x89, 0xc5, 0x41, 0xc6, 0x45, 0x00, 0x5a)
	code.memoryCall(10, 1).memoryCall(28, 4)
	code.emit(0x41, 0x80, 0x7d, 0x00, 0x00).fail(0x85) // discarded page reads zero
	code.memoryCall(10, 3)

	code.movEAX(0).xorEDI().movESI(data).movEDX(1).syscall()
	code.emit(0x48, 0x83, 0xf8, 0x01).fail(0x85)
	code.emit(0x0f, 0xb6, 0x3c, 0x25).imm32(data)                   // movzx edi,byte [data]
	code.emit(0x83, 0xe7, 0x7f, 0xc1, 0xe7, 0x04, 0x4c, 0x01, 0xef) // rdi=r13+(byte&127)*16
	code.emit(0x49, 0x89, 0xfe)                                     // mov r14,rdi
	code.movEAX(158).movESIFromR14().syscall().testRAX().fail(0x85) // ARCH_SET_FS
	code.movEAX(1).movEDI(1).movESI(marker).movEDX(1).syscall()
	code.emit(0x48, 0x83, 0xf8, 0x01).fail(0x85)
	code.movEAX(0).xorEDI().movESI(data + 1).movEDX(2).syscall()
	code.emit(0x48, 0x83, 0xf8, 0x02).fail(0x85)
	code.movEAX(158).movEDI(0x1003).emit(0x49, 0x8d, 0x76, 0x08).syscall().testRAX().fail(0x85)
	code.emit(0x49, 0x8b, 0x46, 0x08, 0x4c, 0x39, 0xf0).fail(0x85)
	code.emit(0x49, 0xc7, 0x46, 0x10).imm32(0x12345678)
	code.emit(0x64, 0x48, 0x8b, 0x04, 0x25, 0x10, 0x00, 0x00, 0x00)
	code.emit(0x3d).imm32(0x12345678).fail(0x85)

	code.movEAX(11).emit(0x4c, 0x89, 0xef).movESI(4096).syscall().testRAX().fail(0x85)
	code.movEAX(1).movEDI(1).movESI(data).movEDX(3).syscall()
	code.emit(0x48, 0x83, 0xf8, 0x03).fail(0x85)
	code.movEAX(1).movEDI(1).movESI(marker).movEDX(uint32(len(markerText))).syscall()
	code.emit(0x48, 0x83, 0xf8, byte(len(markerText))).fail(0x85)
	code.movEAX(231).xorEDI().syscall().emit(0x0f, 0x0b)
	code.finishFailure()
	copy(image[entry:markerAt], code.bytes)
	copy(image[markerAt:], markerText)
	return image
}

type probeProgram struct {
	bytes []byte
	fails []int
}

func (program *probeProgram) emit(bytes ...byte) *probeProgram {
	program.bytes = append(program.bytes, bytes...)
	return program
}

func (program *probeProgram) imm32(value uint32) *probeProgram {
	start := len(program.bytes)
	program.bytes = append(program.bytes, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(program.bytes[start:], value)
	return program
}

func (program *probeProgram) movEAX(value uint32) *probeProgram {
	return program.emit(0xb8).imm32(value)
}
func (program *probeProgram) movEDI(value uint32) *probeProgram {
	return program.emit(0xbf).imm32(value)
}
func (program *probeProgram) movESI(value uint32) *probeProgram {
	return program.emit(0xbe).imm32(value)
}
func (program *probeProgram) movEDX(value uint32) *probeProgram {
	return program.emit(0xba).imm32(value)
}
func (program *probeProgram) xorEDI() *probeProgram  { return program.emit(0x31, 0xff) }
func (program *probeProgram) syscall() *probeProgram { return program.emit(0x0f, 0x05) }
func (program *probeProgram) testRAX() *probeProgram { return program.emit(0x48, 0x85, 0xc0) }

func (program *probeProgram) fail(condition byte) *probeProgram {
	program.emit(0x0f, condition)
	program.fails = append(program.fails, len(program.bytes))
	return program.imm32(0)
}

func (program *probeProgram) memoryCall(number uint32, protectionOrAdvice uint32) *probeProgram {
	return program.movEAX(number).emit(0x4c, 0x89, 0xef).movESI(4096).movEDX(protectionOrAdvice).syscall().testRAX().fail(0x85)
}

func (program *probeProgram) movESIFromR14() *probeProgram {
	return program.movEDI(0x1002).emit(0x4c, 0x89, 0xf6)
}

func (program *probeProgram) finishFailure() {
	failure := len(program.bytes)
	program.movEAX(231).movEDI(1).syscall().emit(0x0f, 0x0b)
	for _, offset := range program.fails {
		delta := int64(failure - (offset + 4))
		binary.LittleEndian.PutUint32(program.bytes[offset:], uint32(int32(delta)))
	}
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
