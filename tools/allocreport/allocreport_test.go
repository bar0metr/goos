package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goos/userspace/runtime/arena"
)

func TestParseBenchLine(t *testing.T) {
	samples := map[string]map[string][]float64{}
	lines := []string{
		"BenchmarkBoundary/size=17-8   \t  2000\t  31.02 ns/op\t 0 discards/kop\t 4.000 held-pages",
		"BenchmarkFind-8 \t 1000000 \t 6.85 ns/op \t 0 B/op \t 0 allocs/op",
		"BenchmarkFind-8 \t 1000000 \t 7.15 ns/op \t 0 B/op \t 0 allocs/op",
		"PASS",
		"goos: darwin",
	}
	for _, line := range lines {
		if err := parseBenchLine(line, samples); err != nil {
			t.Fatal(err)
		}
	}
	if got := samples["BenchmarkBoundary/size=17"]["held-pages"]; len(got) != 1 || got[0] != 4 {
		t.Fatalf("boundary %v", samples)
	}
	if got := samples["BenchmarkFind"]["ns/op"]; len(got) != 2 || got[1] != 7.15 {
		t.Fatalf("find %v", samples)
	}
	if err := parseBenchLine("BenchmarkX-8 10 5 ns/op 3", samples); err == nil {
		t.Fatal("an odd metric list was accepted")
	}
}

func TestSummarize(t *testing.T) {
	median, spread := summarize([]float64{10, 12, 11, 30, 9})
	if median != 11 || math.Abs(spread-21.0/11) > 1e-9 {
		t.Fatalf("median %v spread %v", median, spread)
	}
	if median, _ := summarize([]float64{1, 3}); median != 2 {
		t.Fatalf("even median %v", median)
	}
}

func TestTablesFit(t *testing.T) {
	sizes := map[int][2]uint64{
		// 100 requests of 385..400 bytes, 300 each on average.
		24: {100, 39000},
		// 10 requests of 2033..2048 bytes.
		127: {10, 20480},
	}
	var current, dense Table
	for _, table := range candidateTables {
		switch table.Name {
		case "current":
			current = table
		case "dense":
			dense = table
		}
	}
	if current.classOf(400) != 512 || dense.classOf(400) != 400 || dense.classOf(401) != 512 {
		t.Fatal("classOf")
	}
	a, b := current.fit(sizes), dense.fit(sizes)
	if a.Requests != 110 || a.Slots != 100*512+10*2048 || b.Slots != 100*400+10*2048 {
		t.Fatalf("slots %+v %+v", a, b)
	}
	if a.Pages != 100.0/8+5 || b.Pages != 100.0/10+5 || b.Utilization() <= a.Utilization() {
		t.Fatalf("pages %+v %+v", a, b)
	}
	for _, table := range candidateTables {
		previous := 0
		for _, size := range table.Sizes {
			if size%16 != 0 || size <= previous || pageBytes/size > 256 {
				t.Fatalf("table %s: %d", table.Name, size)
			}
			previous = size
		}
		if previous != 2048 {
			t.Fatalf("table %s ends at %d", table.Name, previous)
		}
	}
	if _, err := parseTable("bad", "16,40,2048"); err == nil {
		t.Fatal("a size that is not a multiple of 16 was accepted")
	}
}

func TestReadGuest(t *testing.T) {
	text := strings.Join([]string{
		"ALLOCATOR_RUN label=baseline revision=abc memory=512M",
		"MEMRECLAIM_PAGES_BEFORE=120",
		"MEMRECLAIM_BURST before=100 peak=400 idle=101 quiet=101",
		"ALLOC_PROFILE vfs large=3 large_bytes=20000 new_slabs=7",
		"ALLOC_PROFILE vfs discards=9 meta_pages=2",
		"ALLOC_PROFILE vfs slabs 32:1:0:100 2048:0:2:0",
		"ALLOC_PROFILE vfs sizes 1:10:300 127:2:4090",
		"ALLOC_PROFILE vfs large=5 large_bytes=30000 new_slabs=8",
		"ALLOC_PROFILE vfs sizes 1:12:360",
		"ALLOCATOR_ROUND round=1 kernel=1099 processes=2000",
		"MEMRECLAIM_WORKLOAD retained=32 rounds=10 ns=500 collections=2",
		"MEMRECLAIM_WORKLOAD retained=0 rounds=10 ns=900 collections=2",
		"MEMRECLAIM_WORKLOAD retained=32 rounds=10 ns=450 collections=2",
		"VFS_MEMORY requests=4096 live=1000 objects=10 collections=3 pages=150",
	}, "\n")
	path := filepath.Join(t.TempDir(), "guest.txt")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := readGuest(path)
	if err != nil {
		t.Fatal(err)
	}
	p := g.Profiles["vfs"]
	if g.Label != "baseline" || g.Revision != "abc" || len(g.Order) != 1 || p.Fields["large"] != 5 || p.Fields["discards"] != 0 || p.Sizes[1] != [2]uint64{12, 360} {
		t.Fatalf("the last report of a process does not replace the earlier one: %+v", p)
	}
	if g.Lines["MEMRECLAIM_PAGES_BEFORE"]["value"] != 120 || g.Lines["MEMRECLAIM_BURST"]["idle"] != 101 {
		t.Fatalf("lines %v", g.Lines)
	}
	if len(g.Rounds) != 1 || g.Rounds[0].Processes != 2000 || g.VFS["pages"] != 150 {
		t.Fatalf("rounds %v vfs %v", g.Rounds, g.VFS)
	}
	if g.Lines["MEMRECLAIM_WORKLOAD"]["ns"] != 500 || g.Lines["MEMRECLAIM_WORKLOAD/0"]["ns"] != 900 || g.Lines["MEMRECLAIM_WORKLOAD/32"]["ns"] != 450 || len(g.Retained) != 2 {
		t.Fatalf("workload lines %v retained %v", g.Lines, g.Retained)
	}
	r := &report{tables: candidateTables, guests: []*Guest{g, g}}
	out := r.render()
	for _, want := range []string{"burst: pages after idle | 101 | 101 | +0.0% |", "| vfs | dense |", "| 1 | 1099 | 2000 |", "| 0 | 900 | 2 |"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
}

// profileText renders a real profile for process name, as the guest prints
// it, with the lines from the writer.
func profileText(t *testing.T, name string) []string { return profileWith(t, name, 10) }

// profileWith is profileText with count requests in the first size bucket,
// which tells one report from another.
func profileWith(t *testing.T, name string, count uint64) []string {
	t.Helper()
	var d arena.Diagnostics
	d.SizeCounts[0], d.SizeBytes[0] = count, 12*count
	d.SizeCounts[64], d.SizeBytes[64] = 3, 3090
	d.PartialSlabs[0], d.FreeSlots[0] = 1, 200
	d.Frontier, d.MetadataPages = 40, 2
	var lines []string
	var buffer [160]byte
	d.AppendProfile(name, buffer[:], func(line []byte) { lines = append(lines, strings.TrimSuffix(string(line), "\n")) })
	return lines
}

func TestProfileCompleteness(t *testing.T) {
	good := profileText(t, "vfs")
	if len(good) < 4 || !strings.Contains(good[len(good)-1], " end lines=") {
		t.Fatalf("profile lines %q", good)
	}
	var sizesLine, slabsLine, threadsLine, cacheLine, gcLine, pauseLine int
	for index, line := range good {
		switch {
		case strings.HasPrefix(line, "ALLOC_PROFILE vfs cache"):
			cacheLine = index
		case strings.HasPrefix(line, "ALLOC_PROFILE vfs gc"):
			gcLine = index
		case strings.HasPrefix(line, "ALLOC_PROFILE vfs pause_hist"):
			pauseLine = index
		case strings.HasPrefix(line, "ALLOC_PROFILE vfs sizes"):
			sizesLine = index
		case strings.HasPrefix(line, "ALLOC_PROFILE vfs slabs"):
			slabsLine = index
		case strings.HasPrefix(line, "ALLOC_PROFILE vfs threads"):
			threadsLine = index
		}
	}
	if !strings.HasPrefix(good[0], "ALLOC_PROFILE vfs version=3 large=") || threadsLine == 0 || cacheLine == 0 || gcLine == 0 || pauseLine == 0 {
		t.Fatalf("profile lines %q", good)
	}
	drop := func(lines []string, index int) []string {
		return append(append([]string(nil), lines[:index]...), lines[index+1:]...)
	}
	noCounter := append([]string(nil), good...)
	noCounter[0] = strings.Replace(noCounter[0], " new_slabs=0", "", 1)
	// An older report lacks the lines its version did not have, and its
	// end line counts only the lines it has.
	older := func(version int, prefixes ...string) []string {
		var lines []string
		for _, line := range good {
			keep := true
			for _, prefix := range prefixes {
				keep = keep && !strings.HasPrefix(line, "ALLOC_PROFILE vfs "+prefix+" ")
			}
			if keep {
				lines = append(lines, line)
			}
		}
		version3 := " version=3"
		switch version {
		case 1:
			lines[0] = strings.Replace(lines[0], version3, "", 1)
		default:
			lines[0] = strings.Replace(lines[0], version3, fmt.Sprintf(" version=%d", version), 1)
		}
		last := len(lines) - 1
		lines[last] = strings.Replace(lines[last], fmt.Sprintf("lines=%d", len(good)-1), fmt.Sprintf("lines=%d", last), 1)
		return lines
	}
	versionTwo := older(2, "cache", "gc", "pause_hist", "rendezvous_hist")
	versionOne := older(1, "threads", "cache", "gc", "pause_hist", "rendezvous_hist")
	cases := []struct {
		name              string
		lines             []string
		complete, partial int
		sizes             bool
	}{
		{"complete", good, 1, 0, true},
		{"after the prompt", append([]string{"goos> " + good[0] + "\r"}, good[1:]...), 1, 0, true},
		{"lost sizes", drop(good, sizesLine), 0, 1, false},
		{"lost slabs", drop(good, slabsLine), 0, 1, false},
		{"lost first line", good[1:], 0, 1, false},
		{"never ended", good[:len(good)-1], 0, 1, false},
		{"lost counter", noCounter, 0, 1, false},
		{"lost threads", drop(good, threadsLine), 0, 1, false},
		{"lost cache", drop(good, cacheLine), 0, 1, false},
		{"lost gc", drop(good, gcLine), 0, 1, false},
		{"lost pause histogram", drop(good, pauseLine), 0, 1, false},
		{"version 2", versionTwo, 1, 0, true},
		{"version 1", versionOne, 1, 0, true},
		{"a good one after a broken one", append(drop(good, sizesLine), good...), 1, 1, true},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "log")
		if err := os.WriteFile(path, []byte(strings.Join(c.lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		g, err := readGuestProfiles(path, true)
		if err != nil {
			t.Fatal(err)
		}
		if g.Complete["vfs"] != c.complete || g.Incomplete["vfs"] != c.partial {
			t.Fatalf("%s: complete %d incomplete %d, want %d %d", c.name, g.Complete["vfs"], g.Incomplete["vfs"], c.complete, c.partial)
		}
		if hasSizes := g.Profiles["vfs"] != nil && len(g.Profiles["vfs"].Sizes) != 0; hasSizes != c.sizes {
			t.Fatalf("%s: sizes %v, want %v", c.name, hasSizes, c.sizes)
		}
	}
}

// TestLegacyProfilesHaveNoEnd reads a file written before profiles had an end
// line: every report counts, the last one is kept, and none is incomplete.
func TestLegacyProfilesHaveNoEnd(t *testing.T) {
	text := "ALLOC_PROFILE vfs large=1\nALLOC_PROFILE vfs sizes 1:5:80\nALLOC_PROFILE vfs large=2\nALLOC_PROFILE vfs sizes 1:9:144\n"
	path := filepath.Join(t.TempDir(), "old")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := readGuest(path)
	if err != nil {
		t.Fatal(err)
	}
	if g.Incomplete["vfs"] != 0 || g.Complete["vfs"] != 2 || g.Profiles["vfs"].Fields["large"] != 2 || g.Profiles["vfs"].Sizes[1] != [2]uint64{9, 144} {
		t.Fatalf("complete %d incomplete %d profile %+v", g.Complete["vfs"], g.Incomplete["vfs"], g.Profiles["vfs"])
	}
	if strict, err := readGuestProfiles(path, true); err != nil || strict.Complete["vfs"] != 0 {
		t.Fatalf("strict reading kept %d unended profiles", strict.Complete["vfs"])
	}
}

// TestProfileThatLostItsEndBeforeAnother covers a first report that lost its
// end line, followed by another: the first must not count, whatever the
// second is, in either reading.
func TestProfileThatLostItsEndBeforeAnother(t *testing.T) {
	first, second := profileWith(t, "vfs", 10), profileWith(t, "vfs", 20)
	unended := first[:len(first)-1]
	var damaged []string
	for _, line := range second {
		if !strings.HasPrefix(line, "ALLOC_PROFILE vfs sizes") {
			damaged = append(damaged, line)
		}
	}
	for _, c := range []struct {
		name             string
		lines            []string
		complete, broken int
		bucket           uint64
	}{
		{"then a complete one", append(append([]string(nil), unended...), second...), 1, 1, 20},
		{"then a damaged one", append(append([]string(nil), unended...), damaged...), 0, 2, 0},
	} {
		path := filepath.Join(t.TempDir(), "log")
		if err := os.WriteFile(path, []byte(strings.Join(c.lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, strict := range []bool{false, true} {
			g, err := readGuestProfiles(path, strict)
			if err != nil {
				t.Fatal(err)
			}
			if g.Complete["vfs"] != c.complete || g.Incomplete["vfs"] != c.broken {
				t.Fatalf("%s, strict %v: complete %d incomplete %d, want %d %d", c.name, strict, g.Complete["vfs"], g.Incomplete["vfs"], c.complete, c.broken)
			}
			p := g.Profiles["vfs"]
			switch {
			case c.complete == 0 && p != nil:
				t.Fatalf("%s, strict %v: the report uses an incomplete profile", c.name, strict)
			case c.complete != 0 && (p == nil || p.Sizes[0][0] != c.bucket):
				t.Fatalf("%s, strict %v: the report uses %+v", c.name, strict, p)
			}
		}
	}
}
