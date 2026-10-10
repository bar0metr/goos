// trustscan checks production images for validation signing material.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: trustscan SYSTEM_TREE PRODUCTION_PUBLIC_KEY VALIDATION_PUBLIC_KEY TEST_SIGNING_SEED")
		os.Exit(2)
	}
	if err := scan(os.Args[1], os.Args[2], os.Args[3], os.Args[4]); err != nil {
		fmt.Fprintln(os.Stderr, "trustscan:", err)
		os.Exit(1)
	}
}
func scan(root, productionPath, validationPath, seedPath string) error {
	production, err := os.ReadFile(productionPath)
	if err != nil {
		return err
	}
	validation, err := os.ReadFile(validationPath)
	if err != nil {
		return err
	}
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		return err
	}
	if len(production) != 32 || len(validation) != 32 || len(seed) != 32 || bytes.Equal(production, validation) {
		return errors.New("production and validation signing inputs are not distinct raw Ed25519 keys")
	}
	installed, err := os.ReadFile(filepath.Join(root, "system/security/trust-update-key.pub"))
	if err != nil {
		return err
	}
	if !bytes.Equal(installed, production) {
		return errors.New("installed update public key differs from production input")
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("non-regular production artifact: %s", path)
		}
		name := entry.Name()
		if strings.Contains(name, "testonly") || name == "private.seed" || strings.HasSuffix(name, ".seed") {
			return fmt.Errorf("private/test artifact in production tree: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, seed) || bytes.Contains(data, validation) || bytes.Contains(data, []byte("-----BEGIN PRIVATE KEY-----")) || bytes.Contains(data, []byte("-----BEGIN EC PRIVATE KEY-----")) || bytes.Contains(data, []byte("-----BEGIN RSA PRIVATE KEY-----")) {
			return fmt.Errorf("validation key or private-key material in production tree: %s", path)
		}
		return nil
	})
}
