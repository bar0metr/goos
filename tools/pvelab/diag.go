package main

import (
	"fmt"
	"strconv"
	"strings"
)

// DiagLinux is the kernel's "DIAG linux" line (kernel/diagnostics.go):
// space-separated key=value fields in any order. Fields this version does
// not know are kept in Extra.
type DiagLinux struct {
	Syscalls          uint64            `json:"syscalls"`
	Unsupported       uint64            `json:"unsupported_count"`
	LastUnsupported   uint64            `json:"last_unsupported"`
	RecentUnsupported []uint64          `json:"recent_unsupported"`
	Refused           []uint64          `json:"refused"`
	BadPointers       uint64            `json:"bad_pointers"`
	LastBadPointer    uint64            `json:"last_bad_pointer"`
	VFSRequests       uint64            `json:"vfs_requests"`
	VFSFailures       uint64            `json:"vfs_failures"`
	VFSStale          uint64            `json:"vfs_stale"`
	Extra             map[string]string `json:"extra,omitempty"`
	Line              string            `json:"line"`
}

const diagLinuxPrefix = "DIAG linux"

// FindDiagLinux returns the last DIAG linux line in text.
func FindDiagLinux(text string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == diagLinuxPrefix || strings.HasPrefix(line, diagLinuxPrefix+" ") {
			return line, true
		}
	}
	return "", false
}

func ParseDiagLinux(line string) (*DiagLinux, error) {
	line = strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(line, diagLinuxPrefix)
	if !ok || rest != "" && rest[0] != ' ' {
		return nil, fmt.Errorf("not a DIAG linux line: %q", line)
	}
	d := &DiagLinux{Line: line, RecentUnsupported: []uint64{}, Refused: []uint64{}}
	numbers := map[string]*uint64{
		"syscalls":         &d.Syscalls,
		"unsupported":      &d.Unsupported,
		"last-unsupported": &d.LastUnsupported,
		"bad-pointers":     &d.BadPointers,
		"last-bad-pointer": &d.LastBadPointer,
		"vfs-requests":     &d.VFSRequests,
		"vfs-failures":     &d.VFSFailures,
		"vfs-stale":        &d.VFSStale,
	}
	lists := map[string]*[]uint64{
		"recent-unsupported": &d.RecentUnsupported,
		"refused":            &d.Refused,
	}
	for _, field := range strings.Fields(rest) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("DIAG linux: malformed field %q", field)
		}
		if target, known := numbers[key]; known {
			n, err := parseDiagNumber(value)
			if err != nil {
				return nil, fmt.Errorf("DIAG linux: %s: %w", key, err)
			}
			*target = n
			continue
		}
		if target, known := lists[key]; known {
			list, err := parseDiagList(value)
			if err != nil {
				return nil, fmt.Errorf("DIAG linux: %s: %w", key, err)
			}
			*target = list
			continue
		}
		if d.Extra == nil {
			d.Extra = map[string]string{}
		}
		d.Extra[key] = value
	}
	return d, nil
}

// parseDiagNumber accepts decimal and the kernel's 0x hexadecimal.
func parseDiagNumber(s string) (uint64, error) {
	base := 10
	if t, ok := strings.CutPrefix(s, "0x"); ok {
		s, base = t, 16
	}
	n, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return n, nil
}

func parseDiagList(s string) ([]uint64, error) {
	out := []uint64{}
	if s == "" {
		return out, nil
	}
	for _, item := range strings.Split(s, ",") {
		n, err := parseDiagNumber(item)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
