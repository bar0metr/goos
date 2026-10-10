// Command tlsfault creates a copy-on-write Go 1.25.5 source root whose TLS
// server sends an intentionally invalid TLS 1.3 Finished value. It is a
// host-only negative-test tool; its output must never be used in production.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const goVersion = "go1.25.5"
const sourcePin = "2e5b7ae1a332c4c53b5c3f58c26fcba0d431d20ea9f8b9e82af09d829ef0008e"
const manifestName = "tlsfault.json"

const finishedSource = `	finished := &finishedMsg{
		verifyData: hs.suite.finishedHash(c.out.trafficSecret, hs.transcript),
	}
`
const finishedPatched = `	finished := &finishedMsg{
		verifyData: hs.suite.finishedHash(c.out.trafficSecret, hs.transcript),
	}
	finished.verifyData[0] ^= 1
`

type manifest struct {
	GoVersion string `json:"go_version"`
	Source    string `json:"source_handshake_server_tls13_sha256"`
	Output    string `json:"output_handshake_server_tls13_sha256"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tlsfault:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: tlsfault SOURCE_GOROOT EMPTY_OUTPUT_GOROOT")
	}
	return prepare(args[0], args[1], sourcePin)
}

func prepare(source, output, wantSource string) error {
	var err error
	source, err = realSourceDir(source)
	if err != nil {
		return err
	}
	output, err = realTmpDir(output)
	if err != nil {
		return err
	}
	if source == output {
		return errors.New("source and output roots must differ")
	}
	if err := requireEmpty(output); err != nil {
		return err
	}
	version, err := os.ReadFile(filepath.Join(source, "VERSION"))
	if err != nil {
		return err
	}
	if strings.SplitN(string(version), "\n", 2)[0] != goVersion {
		return errors.New("source GOROOT must be Go 1.25.5")
	}
	path := filepath.Join(source, "src", "crypto", "tls", "handshake_server_tls13.go")
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if hash(original) != wantSource {
		return errors.New("TLS server Finished source SHA256 does not match pinned input")
	}
	patched, err := patchFinished(original)
	if err != nil {
		return err
	}

	stage, err := os.MkdirTemp(filepath.Dir(output), ".tlsfault-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := mirror(source, stage); err != nil {
		return err
	}
	destination := filepath.Join(stage, "src", "crypto", "tls", "handshake_server_tls13.go")
	if err := os.Remove(destination); err != nil {
		return err
	}
	if err := os.WriteFile(destination, patched, 0o644); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest{GoVersion: goVersion, Source: hash(original), Output: hash(patched)}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, manifestName), append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Remove(output); err != nil {
		return err
	}
	if err := os.Rename(stage, output); err != nil {
		return err
	}
	return nil
}

func patchFinished(source []byte) ([]byte, error) {
	if bytes.Count(source, []byte(finishedSource)) != 1 || bytes.Contains(source, []byte("finished.verifyData[0] ^= 1")) {
		return nil, errors.New("TLS server Finished anchor does not match exactly once")
	}
	return bytes.Replace(source, []byte(finishedSource), []byte(finishedPatched), 1), nil
}

func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func realSourceDir(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("source GOROOT must be a real directory")
	}
	return real, nil
}

func realTmpDir(path string) (string, error) {
	real, err := realSourceDir(path)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(real, "/Volumes/Data/tmp/") {
		return "", errors.New("output GOROOT must be a real directory under /Volumes/Data/tmp")
	}
	return real, nil
}

func requireEmpty(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("output GOROOT is not empty")
	}
	return nil
}

// mirror makes all directories real and all ordinary files symlinks. The one
// patched TLS file is replaced with a regular file after this copy-on-write pass.
func mirror(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from, to := filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())
		info, err := os.Stat(from)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.Mkdir(to, 0o755); err != nil {
				return err
			}
			if err := mirror(from, to); err != nil {
				return err
			}
			continue
		}
		if err := os.Symlink(from, to); err != nil {
			return err
		}
	}
	return nil
}
