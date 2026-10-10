package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"goos/tools/internal/compatroot"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const keyCachePin = "d2052e4809787725708ebcbf88d10747e1031e1876fb46189ea1c2c3fd29b519"
const keyCachePath = "src/crypto/internal/fips140cache/cache.go"

func nativeKeyCache(source, output, overlayPath string) (retErr error) {
	if runtime.GOOS == "js" || runtime.GOOS == "plan9" {
		return errors.New("key cache overlay requires descriptor-root filesystem operations")
	}
	upstream, err := os.ReadFile(filepath.Join(source, keyCachePath))
	if err != nil {
		return err
	}
	if hash(upstream) != keyCachePin {
		return errors.New("Go key cache source SHA256 mismatch")
	}
	overlay, err := os.ReadFile(overlayPath)
	if err != nil {
		return err
	}
	patched, err := format.Source(overlay)
	if err != nil {
		return err
	}
	path, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != path || !compatroot.Allowed(real) {
		return errors.New("key cache output must be a private toolchain root")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return errors.New("key cache output must be a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(before, opened) {
		return errors.New("key cache output changed while opening")
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return err
	}
	if os.SameFile(sourceInfo, opened) {
		return errors.New("key cache output must not be the source root")
	}
	return writeNativeKeyCache(root, source, upstream, overlay, patched)
}

func writeNativeKeyCache(root *os.Root, source string, upstream, overlay, patched []byte) error {
	// All subsequent operations stay anchored to the opened directory descriptor.
	for _, suffix := range []string{"src", "src/crypto", "src/crypto/internal", "src/crypto/internal/fips140cache"} {
		info, err := root.Lstat(suffix)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("key cache output has a linked directory")
		}
	}
	info, err := root.Lstat(keyCachePath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := root.Readlink(keyCachePath)
		if err != nil {
			return err
		}
		expected, err := filepath.Abs(filepath.Join(source, keyCachePath))
		if err != nil {
			return err
		}
		if link != expected {
			return errors.New("private key cache link differs from pinned source")
		}
	} else {
		if !info.Mode().IsRegular() {
			return errors.New("private key cache input must be a file")
		}
		copied, err := root.ReadFile(keyCachePath)
		if err != nil {
			return err
		}
		if hash(copied) != keyCachePin {
			return errors.New("private key cache input differs from pinned source")
		}
	}
	record := struct {
		Source  string `json:"source_sha256"`
		Overlay string `json:"overlay_sha256"`
		Patched string `json:"patched_sha256"`
	}{hash(upstream), hash(overlay), hash(patched)}
	metadata, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	// Refuse preexisting metadata, including links, before changing the cache.
	if err := writeNewRootFile(root, "ssh-keycache.json", append(metadata, '\n')); err != nil {
		return fmt.Errorf("key cache metadata: %w", err)
	}
	if err := root.Remove(keyCachePath); err != nil {
		return err
	}
	return writeNewRootFile(root, keyCachePath, patched)
}

func writeNewRootFile(root *os.Root, path string, data []byte) error {
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, file.Close())
}
