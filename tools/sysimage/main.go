// sysimage lays out the installed system as a directory tree, which mkfat32
// -tree copies onto a volume (ADR 0175):
//
//	system/bin/        the native commands
//	system/services/   the shell, configuration, log, and network services
//	                   and the network driver
//	system/linux/      the static Linux programs
//	system/security/   the pinned certificate-authority bundle
//	system/commands    the command index
//	system/motd        the message of the day
//	system/release     the installation record
//	apps/              applications from LINUX_APPS_DIR, if set
//	home/user/bin/     the user's commands, empty
//	state/             service state, empty
//
// The tree also includes files supplied through LINUX_EXTRA and
// LINUX_APPS_DIR when those variables are set.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"goos/userspace/trustupdate"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"goos/tools/internal/commandset"
	linuxfixtures "goos/userspace/linux"
	"goos/userspace/release"
)

const usage = "usage: sysimage SHELL CONFIG LOG NETD VIRTIO_NET RESOLVED COMMAND_DIR COMMAND_INDEX OUTPUT_DIR"

func main() {
	if len(os.Args) != 10 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	caBundle, err := readCABundle(os.Getenv("SYSTEM_CA_BUNDLE"))
	if err != nil {
		fail(err)
	}
	updateKey, updateURL, err := readTrustInputs(os.Getenv("SYSTEM_TRUST_UPDATE_KEY"), os.Getenv("SYSTEM_TRUST_UPDATE_URL"))
	if err != nil {
		fail(err)
	}
	output := os.Args[9]
	appsSource := os.Getenv("LINUX_APPS_DIR")
	if appsSource != "" {
		if err := checkAppsSource(appsSource, output); err != nil {
			fail(err)
		}
	}
	if err := os.RemoveAll(output); err != nil {
		fail(err)
	}
	for _, directory := range []string{"system/bin", "system/services", "system/linux", "system/security", "apps", "home/user/bin", "state"} {
		if err := os.MkdirAll(filepath.Join(output, directory), 0o755); err != nil {
			fail(err)
		}
	}
	for index, name := range []string{"shell", "config", "log", "netd", "virtio-net", "resolved"} {
		write(output, "system/services/"+name, read(os.Args[1+index]))
	}
	commandDirectory, index := os.Args[7], read(os.Args[8])
	names, err := commandset.Names(commandDirectory, index)
	if err != nil {
		fail(err)
	}
	for _, name := range names {
		write(output, "system/bin/"+name, read(filepath.Join(commandDirectory, name)))
	}
	write(output, "system/commands", index)
	write(output, "system/security/ca-certificates.pem", caBundle)
	write(output, "system/security/trust-update-key.pub", updateKey)
	write(output, "system/security/trust-update-url", updateURL)
	for _, fixture := range linuxfixtures.All() {
		write(output, "system/linux/"+fixture.Name, fixture.Image)
	}
	// LINUX_EXTRA adds Linux programs the repository does not carry, such as
	// a BusyBox the interoperability test was given, each under its own
	// name. Several paths are separated as in PATH.
	for _, extra := range filepath.SplitList(os.Getenv("LINUX_EXTRA")) {
		if extra != "" {
			write(output, "system/linux/"+filepath.Base(extra), read(extra))
		}
	}
	if appsSource != "" {
		if err := copyApps(appsSource, filepath.Join(output, "apps")); err != nil {
			fail(err)
		}
	}
	write(output, "system/motd", []byte("Welcome to GoOS\n"))
	record := release.Current()
	write(output, "system/release", release.Encode(&record))
}

func readCABundle(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("SYSTEM_CA_BUNDLE must select a certificate bundle")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("SYSTEM_CA_BUNDLE: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, (512<<10)+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) == 0 || len(data) > 512<<10 {
		return nil, errors.New("SYSTEM_CA_BUNDLE size outside 1..512KiB")
	}
	canonical := make([]byte, 0, len(data))
	remaining := data
	count := 0
	for len(remaining) != 0 {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----\n")) || count == 256 {
			return nil, errors.New("SYSTEM_CA_BUNDLE must contain 1..256 canonical certificate blocks")
		}
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(block.Bytes) == 0 || len(block.Bytes) > 32<<10 {
			return nil, errors.New("SYSTEM_CA_BUNDLE has invalid PEM or oversized DER")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("SYSTEM_CA_BUNDLE certificate %d: %w", count+1, err)
		}
		if !cert.BasicConstraintsValid || !cert.IsCA || cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 || len(cert.DNSNames) > 64 {
			return nil, errors.New("SYSTEM_CA_BUNDLE certificate violates CA signing or DNS bounds")
		}
		canonical = append(canonical, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})...)
		remaining = rest
		count++
	}
	if count == 0 || !bytes.Equal(canonical, data) {
		return nil, errors.New("SYSTEM_CA_BUNDLE is not canonical certificate-only PEM")
	}
	return data, nil
}

func checkAppsSource(source, output string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("LINUX_APPS_DIR: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("LINUX_APPS_DIR %q is not a directory", source)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{source, output}, {output, source}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return err
		}
		if relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("LINUX_APPS_DIR %q overlaps output %q", source, output)
		}
	}
	return nil
}

func copyApps(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.Mkdir(destination, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("LINUX_APPS_DIR %q: not a regular file or directory", path)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputErr := input.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return fmt.Errorf("copy %q: %w", path, copyErr)
		}
		if inputErr != nil {
			return inputErr
		}
		return closeErr
	})
}

func write(root, path string, data []byte) {
	if err := os.WriteFile(filepath.Join(root, path), data, 0o644); err != nil {
		fail(err)
	}
}

func read(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	return data
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func readTrustInputs(keyPath, url string) ([]byte, []byte, error) {
	if keyPath == "" {
		return nil, nil, errors.New("SYSTEM_TRUST_UPDATE_KEY must select a public update key")
	}
	file, err := os.Open(keyPath)
	if err != nil {
		return nil, nil, err
	}
	key, readErr := io.ReadAll(io.LimitReader(file, ed25519.PublicKeySize+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, nil, readErr
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	if len(key) != ed25519.PublicKeySize || bytes.Equal(key, make([]byte, ed25519.PublicKeySize)) {
		return nil, nil, errors.New("trust update public key must be a nonzero raw Ed25519 public key")
	}
	if url != "" {
		if err := trustupdate.ValidateURL(url); err != nil {
			return nil, nil, err
		}
	}
	if len(url)+1 > 4096 {
		return nil, nil, errors.New("trust update URL too long")
	}
	return key, []byte(url + "\n"), nil
}
