// mkiso writes the EFI system partition and the bootable ISO.
//
// Both images are generated from their inputs alone: entries are in name
// order, the volume serial number is fixed, and every timestamp is
// SOURCE_DATE_EPOCH (0 when unset). Two builds from the same sources therefore
// produce byte-identical images (ADR 0086). The tool replaces hdiutil, whose
// images carried the build time, a random serial number, and macOS metadata
// files.
//
//	mkiso esp KERNEL INITRAMFS OUT.img
//	mkiso iso ESP.img KERNEL INITRAMFS OUT.iso
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	kernelPath    = "EFI/BOOT/BOOTX64.EFI"
	initramfsPath = "initramfs.tar"
	bootImagePath = "efi.img"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "mkiso: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	stamp, err := sourceDate(os.Getenv("SOURCE_DATE_EPOCH"))
	if err != nil {
		return err
	}
	switch {
	case len(args) == 4 && args[0] == "esp":
		files, err := readFiles(map[string]string{kernelPath: args[1], initramfsPath: args[2]})
		if err != nil {
			return err
		}
		image, err := buildFAT(files, stamp)
		if err != nil {
			return err
		}
		return writeAtomic(args[3], image)
	case len(args) == 5 && args[0] == "iso":
		files, err := readFiles(map[string]string{bootImagePath: args[1], kernelPath: args[2], initramfsPath: args[3]})
		if err != nil {
			return err
		}
		image, err := buildISO(files, bootImagePath, stamp)
		if err != nil {
			return err
		}
		return writeAtomic(args[4], image)
	}
	return fmt.Errorf("usage: mkiso esp KERNEL INITRAMFS OUT | mkiso iso ESP KERNEL INITRAMFS OUT")
}

func sourceDate(value string) (time.Time, error) {
	if value == "" {
		return time.Unix(0, 0).UTC(), nil
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH %q is not a non-negative integer", value)
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func readFiles(sources map[string]string) (map[string][]byte, error) {
	files := make(map[string][]byte, len(sources))
	for path, source := range sources {
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		files[path] = data
	}
	return files, nil
}

func writeAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Chmod(0o644); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
