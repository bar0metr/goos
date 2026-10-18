package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
