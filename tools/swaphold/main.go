// Command swaphold checks a goprobe swaphold run against the binary it ran
// (ADR 0204). The guest reports, per region, the offsets from the range's
// start of every RIP a held thread stopped at. This tool disassembles
// goos_g_swap and goos_preempt_entry from the goprobe ELF with `go tool
// objdump` and requires each region's offsets to equal the instruction
// boundaries a thread reaches there: every instruction except UD2, which
// only a failed preemption take reaches. It also requires the guest's
// expected and distinct_rips fields to match that count and lost=0.
//
// Usage: swaphold ELF LOG
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// regions maps a guest region name to the function that holds its range.
var regions = map[string]string{
	"swap":    "goos_g_swap",
	"preempt": "goos_preempt_entry",
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: swaphold ELF LOG")
		os.Exit(2)
	}
	var stdout, stderr bytes.Buffer
	command := exec.Command("go", "tool", "objdump", "-s", "^(goos_g_swap|goos_preempt_entry)$", os.Args[1])
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "swaphold: go tool objdump: %v: %s\n", err, bytes.TrimSpace(stderr.Bytes()))
		os.Exit(1)
	}
	want, err := boundaries(&stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "swaphold: %v\n", err)
		os.Exit(1)
	}
	log, err := os.Open(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "swaphold: %v\n", err)
		os.Exit(1)
	}
	defer log.Close()
	summary, err := check(log, want)
	if err != nil {
		fmt.Fprintf(os.Stderr, "swaphold: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(summary)
}

// boundaries returns, per function, the offsets of the instructions a held
// thread reaches, from `go tool objdump` output.
func boundaries(objdump io.Reader) (map[string][]uint64, error) {
	found := map[string][]uint64{}
	var name string
	var start uint64
	scanner := bufio.NewScanner(objdump)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		switch {
		case strings.TrimSpace(text) == "":
		case strings.HasPrefix(text, "TEXT "):
			header := strings.TrimPrefix(text, "TEXT ")
			end := strings.Index(header, "(SB)")
			if end <= 0 {
				return nil, fmt.Errorf("objdump line %d: malformed header %q", line, text)
			}
			name, start = header[:end], 0
			if _, seen := found[name]; seen {
				return nil, fmt.Errorf("objdump line %d: %s listed twice", line, name)
			}
			found[name] = nil
		default:
			fields := strings.FieldsFunc(text, func(r rune) bool { return r == '\t' })
			if name == "" || len(fields) < 4 {
				return nil, fmt.Errorf("objdump line %d: malformed instruction %q", line, text)
			}
			address, err := strconv.ParseUint(strings.TrimSpace(fields[1]), 0, 64)
			if err != nil {
				return nil, fmt.Errorf("objdump line %d: malformed address %q", line, fields[1])
			}
			if found[name] == nil && start == 0 {
				start = address
			}
			if strings.TrimSpace(fields[3]) == "UD2" {
				continue
			}
			found[name] = append(found[name], address-start)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for _, function := range regions {
		if len(found[function]) == 0 {
			return nil, fmt.Errorf("%s not found in the disassembly", function)
		}
	}
	return found, nil
}

// check compares every GOPROBE_SWAPHOLD region line of log with the
// boundaries and returns a summary.
func check(log io.Reader, want map[string][]uint64) (string, error) {
	seen := map[string]bool{}
	var summary []string
	scanner := bufio.NewScanner(log)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if !strings.HasPrefix(line, "GOPROBE_SWAPHOLD region=") {
			continue
		}
		fields := map[string]string{}
		for _, field := range strings.Fields(strings.TrimPrefix(line, "GOPROBE_SWAPHOLD ")) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				return "", fmt.Errorf("malformed field %q in %q", field, line)
			}
			fields[key] = value
		}
		region := fields["region"]
		function, known := regions[region]
		if !known {
			return "", fmt.Errorf("unknown region in %q", line)
		}
		offsets, err := parseOffsets(fields["offsets"])
		if err != nil {
			return "", fmt.Errorf("%s: %v", region, err)
		}
		expected := want[function]
		if !equal(offsets, expected) {
			return "", fmt.Errorf("%s: held offsets %v, instruction boundaries %v", region, offsets, expected)
		}
		count := strconv.Itoa(len(expected))
		if fields["expected"] != count || fields["distinct_rips"] != count {
			return "", fmt.Errorf("%s: guest expected=%s distinct_rips=%s, disassembly has %s boundaries", region, fields["expected"], fields["distinct_rips"], count)
		}
		if fields["lost"] != "0" {
			return "", fmt.Errorf("%s: lost=%s", region, fields["lost"])
		}
		seen[region] = true
		summary = append(summary, fmt.Sprintf("%s=%s/%s", region, fields["distinct_rips"], count))
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	for region := range regions {
		if !seen[region] {
			return "", fmt.Errorf("no %s region line", region)
		}
	}
	sort.Strings(summary)
	return "swaphold: boundaries held " + strings.Join(summary, " "), nil
}

func parseOffsets(text string) ([]uint64, error) {
	if text == "" {
		return nil, fmt.Errorf("no offsets")
	}
	var offsets []uint64
	for _, part := range strings.Split(text, ",") {
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed offset %q", part)
		}
		offsets = append(offsets, value)
	}
	sort.Slice(offsets, func(left, right int) bool { return offsets[left] < offsets[right] })
	return offsets, nil
}

func equal(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
