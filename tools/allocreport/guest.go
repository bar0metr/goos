package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Guest is one run of tests/boot/allocator.sh.
type Guest struct {
	Label, Revision string
	// Profiles holds the last profile each process name reported.
	Profiles map[string]*Profile
	// Order lists the profile names in the order they first appeared.
	Order []string
	// Lines holds every MEMRECLAIM_ line by its first word.
	Lines map[string]map[string]uint64
	// Rounds are the page counts after each round of shell work.
	Rounds []Round
	// VFS is the last VFS_MEMORY report.
	VFS map[string]uint64
	// Retained lists the retentions the workload ran with, in order.
	Retained []uint64
	// Complete and Incomplete count, per process, the profiles whose every
	// line arrived and the ones that lost a line or a counter.
	Complete, Incomplete map[string]int
}

func contains(values []uint64, value uint64) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// Round is one ALLOCATOR_ROUND line.
type Round struct{ Round, Kernel, Processes uint64 }

// Profile is one process's allocator profile.
type Profile struct {
	// lines counts the lines read for it, to check against its end line.
	lines  int
	Fields map[string]uint64
	// Sizes maps a 16-byte bucket to its request count and bytes.
	Sizes map[int][2]uint64
	// Slabs maps a class size to its partial, full, and free-slot counts.
	Slabs map[int][3]uint64
}

// readGuest reads a guest report or log. Profiles that lost a line are
// dropped; a file with no end line at all predates them, and its profiles
// count as they stand.
func readGuest(path string) (*Guest, error) { return readGuestProfiles(path, false) }

// readGuestProfiles is readGuest; strict requires every profile to end.
func readGuestProfiles(path string, strict bool) (*Guest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	g := &Guest{Profiles: map[string]*Profile{}, Lines: map[string]map[string]uint64{}, Complete: map[string]int{}, Incomplete: map[string]int{}}
	// A profile spans several lines; one starting with the counters begins
	// a new report of that process, and an end line closes it.
	pending := map[string]*Profile{}
	ended := false
	// unended holds, in order, the reports a new one started before they
	// ended and those still open at the end of the file. Whether they count
	// is decided once the whole file is read: a file that predates end lines
	// has none, and every report in it stands; in any other file an unended
	// report lost its end.
	type report struct {
		name    string
		profile *Profile
	}
	var unended []report
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for number := 1; scanner.Scan(); number++ {
		text := strings.TrimRight(scanner.Text(), "\r")
		// A line a service printed as the shell showed its prompt
		// follows "goos> ".
		if index := strings.Index(text, "ALLOC_PROFILE "); index > 0 {
			text = text[index:]
		}
		words := strings.Fields(text)
		if len(words) == 0 {
			continue
		}
		switch {
		case words[0] == "ALLOCATOR_RUN":
			for _, word := range words[1:] {
				if key, value, ok := strings.Cut(word, "="); ok {
					switch key {
					case "label":
						g.Label = value
					case "revision":
						g.Revision = value
					}
				}
			}
		case words[0] == "ALLOCATOR_ROUND":
			fields := pairs(words[1:])
			g.Rounds = append(g.Rounds, Round{fields["round"], fields["kernel"], fields["processes"]})
		case words[0] == "VFS_MEMORY":
			g.VFS = pairs(words[1:])
		case strings.HasPrefix(words[0], "MEMRECLAIM_"):
			// Either NAME=VALUE or NAME key=value ...; lines of one name
			// merge.
			if name, value, ok := strings.Cut(words[0], "="); ok {
				parsed, _ := strconv.ParseUint(value, 10, 64)
				g.Lines[name] = map[string]uint64{"value": parsed}
				continue
			}
			// A workload line names the retention it ran with; the
			// default run, the first, is also kept under its plain name.
			fields := pairs(words[1:])
			keys := []string{words[0]}
			if retained, ok := fields["retained"]; ok {
				name := fmt.Sprintf("%s/%d", words[0], retained)
				if g.Lines[words[0]] != nil {
					keys = nil
				}
				keys = append(keys, name)
				if !contains(g.Retained, retained) {
					g.Retained = append(g.Retained, retained)
				}
			}
			for _, key := range keys {
				if g.Lines[key] == nil {
					g.Lines[key] = map[string]uint64{}
				}
				for field, value := range fields {
					g.Lines[key][field] = value
				}
			}
		case words[0] == "ALLOC_PROFILE" && len(words) >= 3:
			name := words[1]
			if words[2] == "end" {
				ended = true
				p := pending[name]
				delete(pending, name)
				lines, ok := pairs(words[3:])["lines"]
				if p != nil && ok && uint64(p.lines) == lines && p.missing() == "" {
					g.keep(name, p)
				} else {
					g.Incomplete[name]++
				}
				continue
			}
			p := pending[name]
			// A report opens with its version, or, before version 2,
			// with its first counter.
			if strings.HasPrefix(words[2], "version=") || strings.HasPrefix(words[2], "large=") {
				if p != nil {
					unended = append(unended, report{name, p})
				}
				p = newProfile()
				pending[name] = p
			}
			if p == nil {
				// The first line of this report was lost; the end
				// line will count it incomplete.
				continue
			}
			p.lines++
			if err := p.add(words[2:]); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, number, err)
			}
		}
	}
	// A file written before profiles had an end line has none; there every
	// report counts as it stands.
	for _, name := range g.pendingOrder(pending) {
		unended = append(unended, report{name, pending[name]})
	}
	for _, r := range unended {
		if ended || strict {
			g.Incomplete[r.name]++
		} else {
			g.keep(r.name, r.profile)
		}
	}
	return g, scanner.Err()
}

// pendingOrder lists the processes with an open report sorted by name, so a
// file is read the same way every time.
func (g *Guest) pendingOrder(pending map[string]*Profile) []string {
	var names []string
	for name := range pending {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func newProfile() *Profile {
	return &Profile{Fields: map[string]uint64{}, Sizes: map[int][2]uint64{}, Slabs: map[int][3]uint64{}}
}

// keep records a complete report as the latest of its process.
func (g *Guest) keep(name string, p *Profile) {
	if g.Profiles[name] == nil {
		g.Order = append(g.Order, name)
	}
	g.Profiles[name] = p
	g.Complete[name]++
}

// profileKeys are the counters every complete profile carries
// (arena.Diagnostics.AppendProfile).
var profileKeys = []string{
	"large", "large_bytes", "large_pages", "new_slabs", "from_runs", "from_fresh", "recommitted", "cleared",
	"to_full", "to_partial", "searches", "candidates", "discards", "discard_failures", "discarded", "trims",
	"retained", "frontier", "meta_pages", "slab_chunks", "slab_chunks_touched", "free_runs", "free_pages",
	"longest_free", "dirty_free", "run_pages",
}

// threadKeys are the counters of the threads line, which version 2 added.
// They are kept in Fields with a "thread_" prefix.
var threadKeys = []string{
	"live", "peak", "local_allocs", "local_frees", "remote_frees", "refills", "drained", "returned",
	"heap_locks", "heap_contended", "gc_locks", "gc_contended", "suspensions", "suspend_failures",
	"rendezvous_last", "rendezvous_max", "rendezvous_total", "pause_last", "pause_max", "pause_total",
}

// Version is the profile format version; a report without the field is
// version 1.
func (p *Profile) Version() uint64 {
	if version, ok := p.Fields["version"]; ok {
		return version
	}
	return 1
}

// missing names the first counter a profile lacks, or is empty.
func (p *Profile) missing() string {
	for _, key := range profileKeys {
		if _, ok := p.Fields[key]; !ok {
			return key
		}
	}
	if p.Version() >= 2 {
		for _, key := range threadKeys {
			if _, ok := p.Fields["thread_"+key]; !ok {
				return "thread_" + key
			}
		}
	}
	return ""
}

func (p *Profile) add(words []string) error {
	switch words[0] {
	case "sizes":
		for _, word := range words[1:] {
			values, err := colonFields(word, 3)
			if err != nil {
				return err
			}
			p.Sizes[int(values[0])] = [2]uint64{values[1], values[2]}
		}
	case "slabs":
		for _, word := range words[1:] {
			values, err := colonFields(word, 4)
			if err != nil {
				return err
			}
			p.Slabs[int(values[0])] = [3]uint64{values[1], values[2], values[3]}
		}
	case "threads":
		for key, value := range pairs(words[1:]) {
			p.Fields["thread_"+key] = value
		}
	default:
		for key, value := range pairs(words) {
			p.Fields[key] = value
		}
	}
	return nil
}

func colonFields(word string, count int) ([]uint64, error) {
	parts := strings.Split(word, ":")
	if len(parts) != count {
		return nil, fmt.Errorf("%q has %d fields, want %d", word, len(parts), count)
	}
	values := make([]uint64, count)
	for index, part := range parts {
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", word, err)
		}
		values[index] = value
	}
	return values, nil
}

func pairs(words []string) map[string]uint64 {
	fields := map[string]uint64{}
	for _, word := range words {
		if key, value, ok := strings.Cut(word, "="); ok {
			if parsed, err := strconv.ParseUint(value, 10, 64); err == nil {
				fields[key] = parsed
			}
		}
	}
	return fields
}
