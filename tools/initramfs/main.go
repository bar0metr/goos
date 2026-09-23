package main

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"goos/userspace/commands"
)

// usage names the fixed system images in order, then the directory of
// command images and the command index the build generated from them.
const usage = "usage: initramfs INIT_IMAGE SERVICE_IMAGE VFS_IMAGE SHELL_IMAGE BLOCK_IMAGE VIRTIO_BLOCK_IMAGE LOG_IMAGE CONFIG_IMAGE TTY_IMAGE COMMAND_DIR COMMAND_INDEX OUTPUT"

func main() {
	if len(os.Args) != 13 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	epoch := time.Unix(0, 0)
	members := []member{
		directory("system/"),
		directory("system/bin/"),
		// The message of the day belongs to the immutable system image rather
		// than to the volatile root, because /system is a read-only mount of
		// this archive.
		file("system/motd", 0o444, []byte("Welcome to GoOS\n")),
	}
	for index, name := range []string{"init", "fault-probe", "vfs", "shell", "block", "virtio-blk", "log", "config", "tty"} {
		members = append(members, file("system/"+name, 0o555, read(os.Args[1+index])))
	}
	index := read(os.Args[11])
	members = append(members, file("system/commands", 0o444, index))
	binaries, err := commandImages(os.Args[10], index)
	if err != nil {
		fail(err)
	}
	members = append(members, binaries...)
	output, err := os.Create(os.Args[12])
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
