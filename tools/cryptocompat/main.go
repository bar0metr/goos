// cryptocompat prepares a copy-on-write native TinyGo runtime root.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const pinnedVersion = "0.42.0"
const pinnedUEFI = "e17517d3eca2177d9d4094abcc35347c3f650d5e52d1df92e3dd45ebe8b0f393"
const pinnedBaremetal = "111e71bb241f682738895ec039dec7115b0ee16183fb240d42017545027b2e1f"
const metadataFile = "cryptocompat.json"

const originalNow = `func now() (sec int64, nsec int32, mono int64) {
	mono = nanotime()
	to := timeOffset.Load()
	sec = (mono + to) / (1000 * 1000 * 1000)
	nsec = int32((mono + to) - sec*(1000*1000*1000))
	return
}`

const replacementNow = `func now() (sec int64, nsec int32, mono int64) {
	return goosNativeNow()
}`

type sourcePins struct {
	UEFI, Baremetal string
}

type metadata struct {
	TinyGoVersion    string `json:"tinygo_version"`
	Toolchain        string `json:"toolchain"`
	SourceUEFI       string `json:"source_runtime_uefi_sha256"`
	SourceBaremetal  string `json:"source_baremetal_sha256"`
	Backend          string `json:"backend_sha256"`
	PatchedBaremetal string `json:"patched_baremetal_sha256"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cryptocompat:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 3 {
		return errors.New("usage: cryptocompat SOURCE_TINYGOROOT OUTPUT_EMPTY_ROOT BACKEND_FILE")
	}
	output, err := filepath.Abs(args[1])
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(filepath.Join(parent, filepath.Base(output)), "/Volumes/Data/tmp/") {
		return errors.New("output must be under /Volumes/Data/tmp")
	}
	if err := emptyOutput(output); err != nil {
		return err
	}
	version, err := exec.Command(filepath.Join(args[0], "bin", "tinygo"), "version").Output()
	if err != nil {
		return fmt.Errorf("read TinyGo version: %w", err)
	}
	toolchain := strings.TrimSpace(string(version))
	if !strings.HasPrefix(toolchain, "tinygo version "+pinnedVersion+" ") {
		return fmt.Errorf("expected TinyGo %s, got %q", pinnedVersion, toolchain)
	}
	return prepare(args[0], output, args[2], sourcePins{pinnedUEFI, pinnedBaremetal}, toolchain)
}

func emptyOutput(output string) error {
	info, err := os.Lstat(output)
	if err != nil {
		return fmt.Errorf("output must exist as an empty directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("output must be a directory, not a symlink")
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("output directory is not empty")
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func patchBaremetal(source []byte) ([]byte, error) {
	if bytes.Count(source, []byte(originalNow)) != 1 || bytes.Count(source, []byte("func now()")) != 1 {
		return nil, errors.New("baremetal.go must contain exactly the pinned now function")
	}
	return bytes.Replace(source, []byte(originalNow), []byte(replacementNow), 1), nil
}

func prepare(source, output, backendPath string, pins sourcePins, toolchain string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	if err := emptyOutput(output); err != nil {
		return err
	}
	uefi, err := os.ReadFile(filepath.Join(source, "src", "runtime", "runtime_uefi.go"))
	if err != nil {
		return err
	}
	baremetal, err := os.ReadFile(filepath.Join(source, "src", "runtime", "baremetal.go"))
	if err != nil {
		return err
	}
	if digest(uefi) != pins.UEFI || digest(baremetal) != pins.Baremetal {
		return errors.New("TinyGo runtime source SHA256 does not match pinned inputs")
	}
	backend, err := os.ReadFile(backendPath)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(backend, []byte("//go:build uefi\n")) {
		return errors.New("backend must begin with //go:build uefi")
	}
	patched, err := patchBaremetal(baremetal)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(source, metadataFile)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("source root contains reserved metadata filename")
	}
	// Stage first: validation or copying failure leaves the requested root empty.
	stage, err := os.MkdirTemp(filepath.Dir(output), ".cryptocompat-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := mirror(source, stage, false, "src"); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(stage, "src"), 0755); err != nil {
		return err
	}
	if err := mirror(filepath.Join(source, "src"), filepath.Join(stage, "src"), true, "runtime"); err != nil {
		return err
	}
	runtimeDir := filepath.Join(stage, "src", "runtime")
	if err := os.Mkdir(runtimeDir, 0755); err != nil {
		return err
	}
	if err := mirror(filepath.Join(source, "src", "runtime"), runtimeDir, true, "runtime_uefi.go", "baremetal.go"); err != nil {
		return err
	}
	for name, data := range map[string][]byte{"runtime_uefi.go": backend, "baremetal.go": patched} {
		if err := os.WriteFile(filepath.Join(runtimeDir, name), data, 0644); err != nil {
			return err
		}
	}
	record := metadata{
		TinyGoVersion: pinnedVersion, Toolchain: toolchain,
		SourceUEFI: digest(uefi), SourceBaremetal: digest(baremetal),
		Backend: digest(backend), PatchedBaremetal: digest(patched),
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, metadataFile), append(data, '\n'), 0644); err != nil {
		return err
	}
	if err := emptyOutput(output); err != nil {
		return err
	}
	info, err := os.Lstat(output)
	if err != nil {
		return err
	}
	if err := os.Chmod(stage, info.Mode().Perm()); err != nil {
		return err
	}
	// macOS cannot rename over an empty directory. Removing it creates a brief
	// gap before rename; this is not an atomic replacement of the output root.
	if err := os.Remove(output); err != nil {
		return err
	}
	if err := os.Rename(stage, output); err != nil {
		return errors.Join(err, os.Mkdir(output, info.Mode().Perm()))
	}
	return nil
}

func mirror(source, output string, recursive bool, excluded ...string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		skip := false
		for _, name := range excluded {
			skip = skip || entry.Name() == name
		}
		if skip {
			continue
		}
		sourcePath := filepath.Join(source, entry.Name())
		outputPath := filepath.Join(output, entry.Name())
		if recursive {
			// TinyGo merges source namespaces and requires real directories.
			// Stat follows directory links in the trusted installed toolchain.
			info, err := os.Stat(sourcePath)
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err := os.Mkdir(outputPath, 0755); err != nil {
					return err
				}
				if err := mirror(sourcePath, outputPath, true); err != nil {
					return err
				}
				continue
			}
		}
		if err := os.Symlink(sourcePath, outputPath); err != nil {
			return err
		}
	}
	return nil
}
