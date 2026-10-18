package main

import (
	"bufio"
	"fmt"
	"os"
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
}

// Round is one ALLOCATOR_ROUND line.
type Round struct{ Round, Kernel, Processes uint64 }

// Profile is one process's allocator profile.
type Profile struct {
	Fields map[string]uint64
	// Sizes maps a 16-byte bucket to its request count and bytes.
	Sizes map[int][2]uint64
	// Slabs maps a class size to its partial, full, and free-slot counts.
	Slabs map[int][3]uint64
}

func readGuest(path string) (*Guest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	g := &Guest{Profiles: map[string]*Profile{}, Lines: map[string]map[string]uint64{}}
	// A profile spans several lines; one starting with the counters begins
	// a new report of that process.
	current := map[string]*Profile{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for number := 1; scanner.Scan(); number++ {
		words := strings.Fields(scanner.Text())
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
			if g.Lines[words[0]] == nil {
				g.Lines[words[0]] = map[string]uint64{}
			}
			for key, value := range pairs(words[1:]) {
				g.Lines[words[0]][key] = value
			}
		case words[0] == "ALLOC_PROFILE" && len(words) >= 3:
			name := words[1]
			p := current[name]
			if strings.HasPrefix(words[2], "large=") || p == nil {
				p = &Profile{Fields: map[string]uint64{}, Sizes: map[int][2]uint64{}, Slabs: map[int][3]uint64{}}
				current[name] = p
				if g.Profiles[name] == nil {
					g.Order = append(g.Order, name)
				}
				g.Profiles[name] = p
			}
			if err := p.add(words[2:]); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, number, err)
			}
		}
	}
	return g, scanner.Err()
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
