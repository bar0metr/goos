// cryptopack adds isolated probe images to a disposable validation system tree.
package main

import (
	"fmt"
	"goos/tools/internal/commandset"
	"goos/userspace/commands"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 {
		fail(fmt.Errorf("usage: cryptopack SYSTEM_TREE PROBE_IMAGES"))
	}
	if err := pack(os.Args[1], os.Args[2]); err != nil {
		fail(err)
	}
}

func pack(tree, probes string) error {
	indexPath := filepath.Join(tree, "system/commands")
	index, err := os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	if !commands.Valid(index) {
		return fmt.Errorf("invalid input command index")
	}
	keep := []string{"cat", "write", "echo", "ps", "quota", "cryptoprobe"}
	subset, err := commandset.Subset(index, keep)
	if err != nil {
		return err
	}
	bin := filepath.Join(tree, "system/bin")
	names, err := commandset.Names(bin, index)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := commands.Lookup(subset, []byte(name)); !ok {
			if err := os.Remove(filepath.Join(bin, name)); err != nil {
				return err
			}
		}
	}
	entries, err := os.ReadDir(probes)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !commands.ValidName([]byte(entry.Name())) {
			return fmt.Errorf("invalid probe image %q", entry.Name())
		}
		name := "crypto-" + entry.Name()
		if _, exists := commands.Lookup(subset, []byte(name)); exists {
			return fmt.Errorf("duplicate probe %q", name)
		}
		manifest := commands.Manifest{Usage: name, Summary: "native crypto probe"}
		var ok bool
		subset, ok = commands.Append(subset, name, manifest)
		if !ok {
			return fmt.Errorf("invalid probe manifest %q", name)
		}
		image, err := os.ReadFile(filepath.Join(probes, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(bin, name), image, 0o555); err != nil {
			return err
		}
	}
	if !commands.Valid(subset) {
		return fmt.Errorf("probe command index exceeds bounds")
	}
	if _, err := commandset.Names(bin, subset); err != nil {
		return err
	}
	return os.WriteFile(indexPath, subset, 0o444)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cryptopack:", err)
	os.Exit(1)
}
