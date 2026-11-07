package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"goos/kernel/ramfs"
	"goos/userspace/fat32"
)

const variantPath = "/system/.goos-build-variant"

func protectedTarget(target string) bool {
	first, _, _ := strings.Cut(target, "/")
	return strings.EqualFold(first, "home") || strings.EqualFold(first, "state")
}

func updateImage(path, variant string, associate bool, puts []string) error {
	if err := validateVariant(variant); err != nil {
		return err
	}
	for _, entry := range puts {
		i := strings.IndexByte(entry, ':')
		if i < 0 {
			return fmt.Errorf("invalid update entry %q", entry)
		}
		target := strings.Trim(entry[i+1:], "/")
		if protectedTarget(target) {
			return fmt.Errorf("update target %q is protected", target)
		}
		for _, part := range strings.Split(target, "/") {
			if part == "." || part == ".." || part == "" {
				return fmt.Errorf("invalid update target %q", target)
			}
		}
		if strings.EqualFold("/"+target, variantPath) {
			return fmt.Errorf("update target %q is reserved", target)
		}
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open existing disk: %w", err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock disk: %w", err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("disk must be a regular file")
	}
	target, err := volume(fileDevice{file: file, size: uint64(info.Size())})
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	tx := newUpdateOverlay(target)
	var fs fat32.FileSystem
	root, ok := fs.Mount(tx)
	if !ok {
		return fmt.Errorf("%s is not a usable FAT32 volume", path)
	}
	marker, err := os.CreateTemp("", "goos-variant-*")
	if err != nil {
		return err
	}
	markerPath := marker.Name()
	marker.Close()
	defer os.Remove(markerPath)
	readErr := getFile(&fs, root, variantPath, markerPath)
	if readErr != nil && !associate {
		return fmt.Errorf("disk has no readable build association; use -associate explicitly: %w", readErr)
	}
	if readErr == nil {
		current, err := os.ReadFile(markerPath)
		if err != nil {
			return err
		}
		if string(current) != variant && !associate {
			return fmt.Errorf("disk build variant %q differs from %q; use -associate explicitly", current, variant)
		}
	}
	for _, entry := range puts {
		i := strings.IndexByte(entry, ':')
		if err := rejectProtectedAlias(&fs, root, entry[i+1:]); err != nil {
			return err
		}
		if err := putFile(&fs, root, entry[:i], entry[i+1:]); err != nil {
			return err
		}
	}
	if err := os.WriteFile(markerPath, []byte(variant), 0600); err != nil {
		return err
	}
	if err := putFile(&fs, root, markerPath, variantPath); err != nil {
		return err
	}
	return tx.commit()
}

func rejectProtectedAlias(fs *fat32.FileSystem, root ramfs.NodeID, target string) error {
	parts := strings.Split(strings.Trim(target, "/"), "/")
	open := func(parent ramfs.NodeID, part string) (ramfs.NodeID, ramfs.Status) {
		name, ok := ramfs.MakeName([]byte(part))
		if !ok {
			return 0, ramfs.StatusNotFound
		}
		return fs.OpenName(parent, name)
	}
	first, status := open(root, parts[0])
	if status != ramfs.StatusOK {
		return nil
	}
	for _, protected := range []string{"home", "state"} {
		id, s := open(root, protected)
		if s == ramfs.StatusOK && id == first {
			return fmt.Errorf("update target %q is protected", target)
		}
	}
	if len(parts) == 2 {
		system, s := open(root, "system")
		if s == ramfs.StatusOK && system == first {
			marker, ms := open(system, ".goos-build-variant")
			id, ts := open(first, parts[1])
			if ms == ramfs.StatusOK && ts == ramfs.StatusOK && marker == id {
				return fmt.Errorf("update target %q is reserved", target)
			}
		}
	}
	return nil
}

func validateVariant(variant string) error {
	parts := strings.Split(variant, ";")
	keys := []string{"HZ", "DIAG", "VALIDATION", "VFS_PROBE", "VFS_RUNTIME", "OPT"}
	if len(parts) != len(keys) {
		return fmt.Errorf("invalid build variant %q", variant)
	}
	for i, key := range keys {
		value, ok := strings.CutPrefix(parts[i], key+"=")
		if !ok || value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("invalid build variant %q", variant)
		}
	}
	return nil
}
