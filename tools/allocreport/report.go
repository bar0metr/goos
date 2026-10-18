package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

func reportCommand(args []string) error {
	flags := flag.NewFlagSet("report", flag.ExitOnError)
	benchPath := flags.String("bench", "", "results from allocreport bench")
	title := flags.String("title", "Native allocator report", "report title")
	out := flags.String("out", "", "output Markdown")
	var guests, tables listFlag
	flags.Var(&guests, "guest", "a guest profile from tests/boot/allocator.sh")
	flags.Var(&tables, "table", "NAME=SIZES: another size-class table to compare")
	flags.Parse(args)
	if *out == "" {
		usage()
	}
	r := &report{title: *title, tables: append([]Table(nil), candidateTables...)}
	for _, text := range tables {
		name, sizes, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("table %q is not NAME=SIZES", text)
		}
		t, err := parseTable(name, sizes)
		if err != nil {
			return err
		}
		r.tables = append(r.tables, t)
	}
	if *benchPath != "" {
		data, err := os.ReadFile(*benchPath)
		if err != nil {
			return err
		}
		r.bench = new(Results)
		if err := json.Unmarshal(data, r.bench); err != nil {
			return fmt.Errorf("%s: %w", *benchPath, err)
		}
	}
	for _, path := range guests {
		g, err := readGuest(path)
		if err != nil {
			return err
		}
		r.guests = append(r.guests, g)
	}
	return os.WriteFile(*out, []byte(r.render()), 0o644)
}

type report struct {
	title  string
	bench  *Results
	guests []*Guest
	tables []Table
	b      strings.Builder
}

func (r *report) printf(format string, args ...any) { fmt.Fprintf(&r.b, format, args...) }

func (r *report) render() string {
	r.printf("# %s\n\nGenerated %s by `tools/allocreport`.\n\n", r.title, time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	if r.bench != nil {
		r.renderBench()
	}
	if len(r.guests) > 0 {
		r.renderGuests()
		r.renderClasses()
	}
	return r.b.String()
}

// median and spread of samples: spread is (max - min) / median.
func summarize(samples []float64) (median, spread float64) {
	if len(samples) == 0 {
		return math.NaN(), math.NaN()
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	median = sorted[middle]
	if len(sorted)%2 == 0 {
		median = (sorted[middle-1] + sorted[middle]) / 2
	}
	if median != 0 {
		spread = (sorted[len(sorted)-1] - sorted[0]) / median
	}
	return median, spread
}

func formatValue(value float64) string {
	switch {
	case math.IsNaN(value):
		return "-"
	case value >= 1000 || value == math.Trunc(value):
		return fmt.Sprintf("%.0f", value)
	case value >= 10:
		return fmt.Sprintf("%.1f", value)
	default:
		return fmt.Sprintf("%.2f", value)
	}
}

func (r *report) renderBench() {
	res := r.bench
	r.printf("## Host benchmarks\n\n")
	r.printf("Host benchmarks of the heap model compiled by the host Go toolchain, not TinyGo. ")
	r.printf("Every tree runs the same `bench_test.go`; runs alternate between trees.\n\n")
	r.printf("| | |\n|---|---|\n")
	r.printf("| Started | %s |\n| Toolchain | %s %s |\n| CPU | %s |\n", res.Started, res.Go, res.Host, res.CPU)
	r.printf("| Runs per tree | %d, `-benchtime %s` |\n", res.Count, res.BenchTime)
	r.printf("| Baseline | `%s` |\n| Working tree | `%s`%s |\n", res.Revisions.Baseline, res.Revisions.Head, map[bool]string{true: " with uncommitted changes", false: ""}[res.Revisions.Dirty])
	r.printf("| Command | `go run ./tools/%s` |\n\n", strings.ReplaceAll(res.Command, "|", "\\|"))
	r.printf("Cells give the median and, in brackets, the spread (max - min) / median. ")
	r.printf("Δ is against the baseline median; a Δ smaller than the spread is noise.\n\n")
	var names []string
	for name := range res.Trees[0].Samples {
		names = append(names, name)
	}
	sort.Strings(names)
	units := []string{"ns/op", "peak-pages", "held-pages", "discards/kop", "B/op", "allocs/op"}
	for _, unit := range units {
		rows := 0
		for _, name := range names {
			if _, ok := res.Trees[0].Samples[name][unit]; ok {
				rows++
			}
		}
		if rows == 0 || ((unit == "B/op" || unit == "allocs/op") && r.allZero(unit)) {
			continue
		}
		r.printf("### %s\n\n| Benchmark |", unit)
		for index, tree := range res.Trees {
			r.printf(" %s |", tree.Name)
			if index > 0 {
				r.printf(" Δ |")
			}
		}
		r.printf("\n|---|")
		for index := range res.Trees {
			r.printf("---:|")
			if index > 0 {
				r.printf("---:|")
			}
		}
		r.printf("\n")
		for _, name := range names {
			base, ok := res.Trees[0].Samples[name][unit]
			if !ok {
				continue
			}
			baseMedian, _ := summarize(base)
			r.printf("| %s |", strings.TrimPrefix(name, "Benchmark"))
			for index, tree := range res.Trees {
				median, spread := summarize(tree.Samples[name][unit])
				r.printf(" %s [%.0f%%] |", formatValue(median), spread*100)
				if index > 0 {
					r.printf(" %s |", delta(baseMedian, median))
				}
			}
			r.printf("\n")
		}
		r.printf("\n")
	}
	for _, tree := range res.Trees {
		if tree.Revision != "" {
			r.printf("Tree `%s` is revision `%s`.\n\n", tree.Name, tree.Revision)
		}
		if tree.Classes != "" {
			r.printf("Tree `%s` is the working tree with the classes %s.\n\n", tree.Name, tree.Classes)
		}
	}
}

func (r *report) allZero(unit string) bool {
	for _, tree := range r.bench.Trees {
		for _, metrics := range tree.Samples {
			for _, value := range metrics[unit] {
				if value != 0 {
					return false
				}
			}
		}
	}
	return true
}

func delta(base, value float64) string {
	if base == 0 || math.IsNaN(base) || math.IsNaN(value) {
		if base == value {
			return "0%"
		}
		return "-"
	}
	return fmt.Sprintf("%+.1f%%", (value-base)/base*100)
}

func (r *report) renderGuests() {
	r.printf("## Guest measurements\n\n")
	r.printf("Each column is one boot of `tests/boot/allocator.sh` under QEMU TCG. ")
	r.printf("Page counts are what the kernel charges; times are guest clock nanoseconds under emulation ")
	r.printf("and compare only with other TCG runs on the same host.\n\n")
	type row struct {
		label string
		value func(g *Guest) (uint64, bool)
	}
	line := func(name, key string) func(g *Guest) (uint64, bool) {
		return func(g *Guest) (uint64, bool) {
			value, ok := g.Lines[name][key]
			return value, ok
		}
	}
	profile := func(name, key string) func(g *Guest) (uint64, bool) {
		return func(g *Guest) (uint64, bool) {
			if p := g.Profiles[name]; p != nil {
				value, ok := p.Fields[key]
				return value, ok
			}
			return 0, false
		}
	}
	rows := []row{
		{"memreclaim: pages before the page test", line("MEMRECLAIM_PAGES_BEFORE", "value")},
		{"memreclaim: pages at its peak", line("MEMRECLAIM_PAGES_PEAK", "value")},
		{"memreclaim: pages after collecting", line("MEMRECLAIM_PAGES_AFTER", "value")},
		{"memreclaim: 1000 cycles, pages", line("MEMRECLAIM_CYCLES", "pages")},
		{"memreclaim: automatic collections in 3000 rounds", line("MEMRECLAIM_AUTO", "collections")},
		{"memreclaim: longest pause ns", line("MEMRECLAIM_PAUSE", "longest_ns")},
		{"memreclaim: total pause ns", line("MEMRECLAIM_PAUSE", "total_ns")},
		{"burst: pages before", line("MEMRECLAIM_BURST", "before")},
		{"burst: pages at peak", line("MEMRECLAIM_BURST", "peak")},
		{"burst: pages after idle", line("MEMRECLAIM_BURST", "idle")},
		{"burst: pages after a second idle wait", line("MEMRECLAIM_BURST", "quiet")},
		{"workload: rounds", line("MEMRECLAIM_WORKLOAD", "rounds")},
		{"workload: ns", line("MEMRECLAIM_WORKLOAD", "ns")},
		{"workload: collections", line("MEMRECLAIM_WORKLOAD", "collections")},
		{"workload: collection ns", line("MEMRECLAIM_WORKLOAD", "pause_ns")},
		{"workload: highest pages", line("MEMRECLAIM_WORKLOAD_PAGES", "highest")},
		{"workload: pages at the end", line("MEMRECLAIM_WORKLOAD_PAGES", "end")},
		{"workload: discards", profile("workload", "discards")},
		{"workload: pages recommitted", profile("workload", "recommitted")},
		{"workload: bytes cleared on reuse", profile("workload", "cleared")},
		{"VFS: pages at the last report", func(g *Guest) (uint64, bool) { v, ok := g.VFS["pages"]; return v, ok }},
		{"VFS: live bytes at the last report", func(g *Guest) (uint64, bool) { v, ok := g.VFS["live"]; return v, ok }},
		{"VFS: requests at the last report", func(g *Guest) (uint64, bool) { v, ok := g.VFS["requests"]; return v, ok }},
		{"VFS: discards", profile("vfs", "discards")},
		{"VFS: pages recommitted", profile("vfs", "recommitted")},
		{"VFS: metadata pages", profile("vfs", "meta_pages")},
	}
	r.printf("| Measurement |")
	for index, g := range r.guests {
		r.printf(" %s (`%s`) |", g.Label, g.Revision)
		if index > 0 {
			r.printf(" Δ |")
		}
	}
	r.printf("\n|---|")
	for index := range r.guests {
		r.printf("---:|")
		if index > 0 {
			r.printf("---:|")
		}
	}
	r.printf("\n")
	for _, row := range rows {
		r.printf("| %s |", row.label)
		base, baseOK := row.value(r.guests[0])
		for index, g := range r.guests {
			value, ok := row.value(g)
			if ok {
				r.printf(" %d |", value)
			} else {
				r.printf(" - |")
			}
			if index > 0 {
				if ok && baseOK {
					r.printf(" %s |", delta(float64(base), float64(value)))
				} else {
					r.printf(" - |")
				}
			}
		}
		r.printf("\n")
	}
	r.printf("\n### Pages after each round of shell work\n\n")
	r.printf("Kernel and process pages from `mem -p` after each round and two idle seconds.\n\n| Round |")
	for _, g := range r.guests {
		r.printf(" %s kernel | %s processes |", g.Label, g.Label)
	}
	r.printf("\n|---:|")
	for range r.guests {
		r.printf("---:|---:|")
	}
	r.printf("\n")
	most := 0
	for _, g := range r.guests {
		most = max(most, len(g.Rounds))
	}
	for index := 0; index < most; index++ {
		r.printf("| %d |", index+1)
		for _, g := range r.guests {
			if index < len(g.Rounds) {
				r.printf(" %d | %d |", g.Rounds[index].Kernel, g.Rounds[index].Processes)
			} else {
				r.printf(" - | - |")
			}
		}
		r.printf("\n")
	}
	r.printf("\n")
	for _, g := range r.guests {
		r.renderProfiles(g)
	}
}

func (r *report) renderProfiles(g *Guest) {
	r.printf("### Allocator profiles: %s\n\n", g.Label)
	r.printf("Counters are cumulative for each process; frontier, metadata, slab, and free-run figures are a snapshot at the report.\n\n")
	keys := []string{"large", "large_pages", "new_slabs", "from_runs", "from_fresh", "recommitted", "cleared", "to_full", "to_partial",
		"searches", "candidates", "discards", "discard_failures", "discarded", "trims", "frontier", "meta_pages", "slab_chunks",
		"free_runs", "free_pages", "longest_free", "dirty_free", "run_pages"}
	r.printf("| Counter |")
	for _, name := range g.Order {
		r.printf(" %s |", name)
	}
	r.printf("\n|---|")
	for range g.Order {
		r.printf("---:|")
	}
	r.printf("\n| small requests |")
	for _, name := range g.Order {
		r.printf(" %d |", r.tables[0].fit(g.Profiles[name].Sizes).Requests)
	}
	r.printf("\n")
	for _, key := range keys {
		r.printf("| %s |", key)
		for _, name := range g.Order {
			r.printf(" %d |", g.Profiles[name].Fields[key])
		}
		r.printf("\n")
	}
	r.printf("\nSlabs at the report, per class: partial / full / free slots in the partial ones.\n\n| Class |")
	for _, name := range g.Order {
		r.printf(" %s |", name)
	}
	r.printf("\n|---:|")
	for range g.Order {
		r.printf("---:|")
	}
	r.printf("\n")
	classes := map[int]bool{}
	for _, name := range g.Order {
		for class := range g.Profiles[name].Slabs {
			classes[class] = true
		}
	}
	var sorted []int
	for class := range classes {
		sorted = append(sorted, class)
	}
	sort.Ints(sorted)
	for _, class := range sorted {
		r.printf("| %d |", class)
		for _, name := range g.Order {
			if slabs, ok := g.Profiles[name].Slabs[class]; ok {
				r.printf(" %d / %d / %d |", slabs[0], slabs[1], slabs[2])
			} else {
				r.printf(" |")
			}
		}
		r.printf("\n")
	}
	r.printf("\n")
}

// renderClasses fits each process's request histogram from the first guest
// to every candidate table.
func (r *report) renderClasses() {
	g := r.guests[0]
	r.printf("## Size classes\n\n")
	r.printf("Request histograms of `%s` fitted to each table. Internal waste is the share of slot bytes not asked for; ", g.Label)
	r.printf("utilization is the share of the slab pages those slots fill that the requests asked for, ")
	r.printf("which counts a page's unusable tail as well. Both weight every request ever made, not the live heap.\n\n")
	r.printf("| Process | Table | Requests | Internal waste | Utilization | Slab pages per 1000 requests |\n|---|---|---:|---:|---:|---:|\n")
	total := map[int][2]uint64{}
	for _, name := range g.Order {
		for bucket, entry := range g.Profiles[name].Sizes {
			sum := total[bucket]
			total[bucket] = [2]uint64{sum[0] + entry[0], sum[1] + entry[1]}
		}
	}
	names := append(append([]string(nil), g.Order...), "all")
	for _, name := range names {
		sizes := total
		if name != "all" {
			sizes = g.Profiles[name].Sizes
		}
		for _, t := range r.tables {
			f := t.fit(sizes)
			if f.Requests == 0 {
				continue
			}
			r.printf("| %s | %s | %d | %.1f%% | %.1f%% | %.2f |\n", name, t.Name, f.Requests, f.Internal()*100, f.Utilization()*100, f.Pages*1000/float64(f.Requests))
		}
	}
	r.printf("\nTables:\n\n")
	for _, t := range r.tables {
		var sizes []string
		for _, size := range t.Sizes {
			sizes = append(sizes, fmt.Sprintf("%d (%d)", size, pageBytes/size))
		}
		r.printf("- `%s`: %s\n", t.Name, strings.Join(sizes, ", "))
	}
	r.printf("\nThe number after each size is its slots per page.\n\n")
	r.printf("Where requests fall, all processes of `%s` together (16-byte buckets, requests ≥ 0.5%% of the total):\n\n| Bytes | Requests | Share |\n|---|---:|---:|\n", g.Label)
	var all uint64
	for _, entry := range total {
		all += entry[0]
	}
	var buckets []int
	for bucket := range total {
		buckets = append(buckets, bucket)
	}
	sort.Ints(buckets)
	for _, bucket := range buckets {
		if share := ratio(total[bucket][0], all); share >= 0.005 {
			r.printf("| %d-%d | %d | %.1f%% |\n", 16*bucket+1, 16*(bucket+1), total[bucket][0], share*100)
		}
	}
	r.printf("\n")
}
