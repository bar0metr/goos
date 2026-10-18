package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const arenaDir = "userspace/runtime/arena"

// Results is what bench writes: every sample of every metric, per tree.
type Results struct {
	Started   string    `json:"started"`
	Go        string    `json:"go"`
	Host      string    `json:"host"`
	CPU       string    `json:"cpu"`
	Count     int       `json:"count"`
	BenchTime string    `json:"benchtime"`
	Command   string    `json:"command"`
	Trees     []Tree    `json:"trees"`
	Revisions Revisions `json:"revisions"`
}

// Revisions names what the working tree and the baseline were.
type Revisions struct {
	Baseline string `json:"baseline"`
	Head     string `json:"head"`
	Dirty    bool   `json:"dirty"`
}

// Tree is one configuration of the allocator and its samples.
type Tree struct {
	Name     string `json:"name"`
	Revision string `json:"revision,omitempty"`
	Classes  string `json:"classes,omitempty"`
	// Samples maps a benchmark to its metrics, each a sample per run.
	Samples map[string]map[string][]float64 `json:"samples"`
}

// shim gives a revision older than the benchmarks the two policy hooks they
// call, with the behaviour that revision's runtime.go had.
const shim = `package arena

func (h *heap) freeAt(address uintptr) {
	h.free(address)
	if h.dirtyFree<<pageShift > collectionFloor {
		h.releaseFree()
	}
}

func (h *heap) trimAfterCollection() { h.releaseFree() }
`

func benchCommand(args []string) error {
	flags := flag.NewFlagSet("bench", flag.ExitOnError)
	baseline := flags.String("baseline", "", "baseline revision")
	count := flags.Int("count", 5, "runs per tree")
	benchTime := flags.String("benchtime", "1s", "go test -benchtime")
	pattern := flags.String("bench", ".", "go test -bench")
	out := flags.String("out", "", "output JSON")
	var variants, revisions listFlag
	flags.Var(&variants, "variant", "NAME=SIZES: a size-class table applied to the working tree")
	flags.Var(&revisions, "revision", "NAME=REV: another revision to compare")
	flags.Parse(args)
	if *baseline == "" || *out == "" || *count < 1 {
		usage()
	}
	scratch, err := os.MkdirTemp("", "allocreport")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	type setup struct {
		tree    Tree
		overlay string
	}
	var setups []setup
	base, err := revisionOverlay("baseline", *baseline, scratch)
	if err != nil {
		return err
	}
	setups = append(setups, setup{Tree{Name: "baseline"}, base})
	for _, revision := range revisions {
		name, rev, ok := strings.Cut(revision, "=")
		if !ok {
			return fmt.Errorf("revision %q is not NAME=REV", revision)
		}
		overlay, err := revisionOverlay(name, rev, scratch)
		if err != nil {
			return err
		}
		setups = append(setups, setup{Tree{Name: name, Revision: gitOutput("rev-parse", rev)}, overlay})
	}
	setups = append(setups, setup{Tree{Name: "current"}, ""})
	for _, variant := range variants {
		name, sizes, ok := strings.Cut(variant, "=")
		if !ok {
			return fmt.Errorf("variant %q is not NAME=SIZES", variant)
		}
		overlay, err := classOverlay(name, sizes, scratch)
		if err != nil {
			return err
		}
		setups = append(setups, setup{Tree{Name: name, Classes: sizes}, overlay})
	}

	results := Results{
		Started: time.Now().UTC().Format(time.RFC3339), Go: runtime.Version(),
		Host: runtime.GOOS + "/" + runtime.GOARCH, Count: *count, BenchTime: *benchTime,
		Command: "allocreport bench " + strings.Join(args, " "),
	}
	results.Revisions.Baseline = gitOutput("rev-parse", *baseline)
	results.Revisions.Head = gitOutput("rev-parse", "HEAD")
	results.Revisions.Dirty = gitOutput("status", "--porcelain", "--", arenaDir) != ""
	for index := range setups {
		setups[index].tree.Samples = map[string]map[string][]float64{}
	}
	for run := 0; run < *count; run++ {
		for index := range setups {
			s := &setups[index]
			fmt.Fprintf(os.Stderr, "run %d/%d: %s\n", run+1, *count, s.tree.Name)
			cpu, err := runBenchmarks(s.overlay, *pattern, *benchTime, s.tree.Samples)
			if err != nil {
				return fmt.Errorf("%s: %w", s.tree.Name, err)
			}
			if cpu != "" {
				results.CPU = cpu
			}
		}
	}
	for _, s := range setups {
		results.Trees = append(results.Trees, s.tree)
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, append(data, '\n'), 0o644)
}

func gitOutput(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// revisionOverlay maps every Go file of the arena package to its content at
// revision, removing files the revision does not have, and keeps the working
// tree's bench_test.go.
func revisionOverlay(name, revision, scratch string) (string, error) {
	listing, err := exec.Command("git", "ls-tree", "--name-only", revision, arenaDir+"/").Output()
	if err != nil {
		return "", fmt.Errorf("listing %s at %s: %w", arenaDir, revision, err)
	}
	replace := map[string]string{}
	old := map[string]bool{}
	for _, path := range strings.Fields(string(listing)) {
		if !strings.HasSuffix(path, ".go") || filepath.Base(path) == "bench_test.go" {
			continue
		}
		content, err := exec.Command("git", "show", revision+":"+path).Output()
		if err != nil {
			return "", err
		}
		target := filepath.Join(scratch, name, filepath.Base(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return "", err
		}
		replace[absolute(path)] = target
		old[filepath.Base(path)] = true
		if filepath.Base(path) == "heap.go" && !strings.Contains(string(content), "func (h *heap) freeAt(") {
			shimPath := filepath.Join(scratch, name, "shim_test.go")
			if err := os.WriteFile(shimPath, []byte(shim), 0o644); err != nil {
				return "", err
			}
			replace[absolute(filepath.Join(arenaDir, "zz_baseline_shim_test.go"))] = shimPath
		}
	}
	current, err := filepath.Glob(filepath.Join(arenaDir, "*.go"))
	if err != nil {
		return "", err
	}
	for _, path := range current {
		if name := filepath.Base(path); !old[name] && name != "bench_test.go" {
			replace[absolute(path)] = ""
		}
	}
	return writeOverlay(replace, filepath.Join(scratch, name+".json"))
}

var classLine = regexp.MustCompile(`(?m)^var classSizes = \[\.\.\.\]uintptr\{[0-9, ]*\}$`)

// classOverlay replaces the working tree's size-class table.
func classOverlay(name, sizes, scratch string) (string, error) {
	var values []string
	for _, field := range strings.Split(sizes, ",") {
		size, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || size <= 0 || size%16 != 0 || size > 2048 {
			return "", fmt.Errorf("variant %s: %q is not a multiple of 16 up to 2048", name, field)
		}
		values = append(values, strconv.Itoa(size))
	}
	if values[len(values)-1] != "2048" {
		return "", fmt.Errorf("variant %s: the largest class must be 2048", name)
	}
	source := filepath.Join(arenaDir, "heap.go")
	content, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	if !classLine.Match(content) {
		return "", errors.New("heap.go: no classSizes table to replace")
	}
	patched := classLine.ReplaceAll(content, []byte("var classSizes = [...]uintptr{"+strings.Join(values, ", ")+"}"))
	target := filepath.Join(scratch, name, "heap.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, patched, 0o644); err != nil {
		return "", err
	}
	return writeOverlay(map[string]string{absolute(source): target}, filepath.Join(scratch, name+".json"))
}

func absolute(path string) string {
	full, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return full
}

func writeOverlay(replace map[string]string, path string) (string, error) {
	data, err := json.Marshal(struct{ Replace map[string]string }{replace})
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o644)
}

var benchLine = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+(\d+)\s+(.*)$`)

// runBenchmarks runs the benchmarks once and adds every metric to samples.
// It returns the CPU go test reports.
func runBenchmarks(overlay, pattern, benchTime string, samples map[string]map[string][]float64) (string, error) {
	args := []string{"test", "-run", "^$", "-bench", pattern, "-benchtime", benchTime, "-count", "1", "-benchmem"}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	command := exec.Command("go", append(args, "./"+arenaDir)...)
	command.Stderr = os.Stderr
	out, err := command.Output()
	if err != nil {
		os.Stderr.Write(out)
		return "", err
	}
	cpu := ""
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "cpu: "); ok {
			cpu = value
		}
		if err := parseBenchLine(line, samples); err != nil {
			return "", err
		}
	}
	return cpu, nil
}

// parseBenchLine adds the metrics of one go test -bench result line.
func parseBenchLine(line string, samples map[string]map[string][]float64) error {
	match := benchLine.FindStringSubmatch(line)
	if match == nil {
		return nil
	}
	fields := strings.Fields(match[3])
	if len(fields)%2 != 0 {
		return fmt.Errorf("odd metric list in %q", line)
	}
	name := match[1]
	if samples[name] == nil {
		samples[name] = map[string][]float64{}
	}
	for index := 0; index < len(fields); index += 2 {
		value, err := strconv.ParseFloat(fields[index], 64)
		if err != nil {
			return fmt.Errorf("metric %q in %q", fields[index], line)
		}
		unit := fields[index+1]
		samples[name][unit] = append(samples[name][unit], value)
	}
	return nil
}
