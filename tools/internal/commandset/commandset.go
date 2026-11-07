// Package commandset validates command-image directories against command
// indexes and selects indexed command subsets. The boot archive and installed
// system both carry command images, while PID 1 grants each command from an
// index, so an index must not describe a program its tree does not hold (ADR
// 0080, ADR 0175).
package commandset

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"goos/userspace/commands"
)

// Names returns the command images named by index, in name order, and refuses
// an index entry without a regular image.
func Names(directory string, index []byte) ([]string, error) {
	seen := map[string]bool{}
	for offset := 0; ; {
		entry, next, ok := commands.Next(index, offset)
		if !ok {
			if offset != len(index) {
				return nil, fmt.Errorf("command index is malformed at byte %d", offset)
			}
			break
		}
		name := string(entry.Name)
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s: index entry without an image", entry.Name)
		}
		seen[name] = true
		offset = next
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	if err := Agree(names, index); err != nil {
		return nil, err
	}
	return names, nil
}

// Agree reports whether names and index describe the same commands.
func Agree(names []string, index []byte) error {
	for _, name := range names {
		if _, found := commands.Lookup(index, []byte(name)); !found {
			return fmt.Errorf("%s: image without an index entry", name)
		}
	}
	listed := 0
	for offset := 0; ; {
		entry, next, ok := commands.Next(index, offset)
		if !ok {
			if offset != len(index) {
				return fmt.Errorf("command index is malformed at byte %d", offset)
			}
			break
		}
		position := sort.SearchStrings(names, string(entry.Name))
		if position == len(names) || names[position] != string(entry.Name) {
			return fmt.Errorf("%s: index entry without an image", entry.Name)
		}
		listed++
		offset = next
	}
	if listed != len(names) {
		return fmt.Errorf("command index lists %d commands for %d images", listed, len(names))
	}
	return nil
}

// Subset returns the lines of index for the commands in allow, in index
// order. Every allowed command must be in the index.
func Subset(index []byte, allow []string) ([]byte, error) {
	wanted := map[string]bool{}
	for _, name := range allow {
		if _, found := commands.Lookup(index, []byte(name)); !found {
			return nil, fmt.Errorf("%s: not in the command index", name)
		}
		wanted[name] = true
	}
	var subset []byte
	for offset := 0; ; {
		entry, next, ok := commands.Next(index, offset)
		if !ok {
			if offset != len(index) {
				return nil, fmt.Errorf("command index is malformed at byte %d", offset)
			}
			return subset, nil
		}
		if wanted[string(entry.Name)] {
			subset = append(subset, index[offset:next]...)
		}
		offset = next
	}
}
