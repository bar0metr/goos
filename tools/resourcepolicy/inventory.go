package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Inventory holds the keys a document classifies.
type Inventory struct {
	Exact  map[string]bool // "kernel/ipc.MaxHandles"
	Groups map[string]bool // "userspace/dnswire" from `userspace/dnswire.Max*`
}

// keyPattern matches a backquoted inventory key inside a table row.
var keyPattern = regexp.MustCompile("`((?:kernel|userspace)/[A-Za-z0-9_/]+)\\.(Max\\*|Max[A-Za-z0-9_]+)`")

// parseInventory collects the keys from the table rows (lines starting with
// "|") of text. Keys in prose are ignored so that a paragraph naming a bound
// does not count as classifying it.
func parseInventory(text string) Inventory {
	inv := Inventory{Exact: map[string]bool{}, Groups: map[string]bool{}}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		for _, m := range keyPattern.FindAllStringSubmatch(line, -1) {
			if m[2] == "Max*" {
				inv.Groups[m[1]] = true
			} else {
				inv.Exact[m[1]+"."+m[2]] = true
			}
		}
	}
	return inv
}

// compare returns one line per bound missing from inv and per exact entry
// that names no bound.
func compare(bounds []Bound, inv Inventory) []string {
	var problems []string
	known := map[string]bool{}
	packages := map[string]bool{}
	for _, b := range bounds {
		known[b.Key()] = true
		packages[b.Package] = true
		if inv.Exact[b.Key()] || inv.Groups[b.Package] {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s:%d: %s has no resource-policy entry", b.File, b.Line, b.Key()))
	}
	var stale []string
	for key := range inv.Exact {
		if !known[key] {
			stale = append(stale, fmt.Sprintf("resource-policy entry `%s` names no Max* bound", key))
		}
	}
	for pkg := range inv.Groups {
		if !packages[pkg] {
			stale = append(stale, fmt.Sprintf("resource-policy group `%s.Max*` matches no package with Max* bounds", pkg))
		}
	}
	sort.Strings(stale)
	return append(problems, stale...)
}
