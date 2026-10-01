// Package commandset checks that a directory of command images and the
// command index built from the same tree agree, and cuts the index down to a
// subset. The boot archive and the installed system both carry commands, and
// PID 1 grants each from an index, so neither may describe a program it does
// not hold (ADR 0080, ADR 0175).
package commandset

import (
	"fmt"
	"os"
	"sort"

	"goos/userspace/commands"
)

// Names returns the command images in directory, in name order, and refuses
// an image the index does not describe or an index entry without an image.
func Names(directory string, index []byte) ([]string, error) {
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
