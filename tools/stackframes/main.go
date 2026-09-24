// Command stackframes checks at build time that no function in a GoOS binary
// can move the stack pointer past the unmapped guard below its stack
// (ADR 0088).
//
// Neither kernel nor userspace code has working stack probes, so a guard only
// catches an overflow if every frame is smaller than the guard. For a
// userspace ELF the tool disassembles the binary with `go tool objdump`,
// computes each function's static stack depth, and rejects a depth larger than
// loader.StackGuardSize less the margin below, as well as any change to the
// stack pointer it cannot bound. For the kernel's PE image it finds every
// frame LLVM allocates, with or without a ___chkstk_ms call, and bounds it by
// the kernel stack guard.
//
// Usage: stackframes [-v] BINARY...
//
// -v lists every frame, largest first.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
)

func main() {
	verbose := flag.Bool("v", false, "list every frame")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: stackframes [-v] BINARY...")
		os.Exit(2)
	}
	failed := false
	for _, path := range flag.Args() {
		report, err := check(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stackframes: %s: %v\n", path, err)
			failed = true
			continue
		}
		if *verbose {
			sort.SliceStable(report.frames, func(left, right int) bool { return report.frames[left].size > report.frames[right].size })
			for _, frame := range report.frames {
				fmt.Printf("%8d  %s\n", frame.size, frame.name)
			}
		}
		if len(report.problems) != 0 {
			failed = true
			for _, problem := range report.problems {
				fmt.Fprintf(os.Stderr, "stackframes: %s: %s\n", path, problem)
			}
			fmt.Fprintf(os.Stderr, "stackframes: %s: FAILED: %s\n", path, report.summary())
			continue
		}
		fmt.Printf("stackframes: %s: %s\n", path, report.summary())
	}
	if failed {
		os.Exit(1)
	}
}

// report is the outcome of checking one binary.
type report struct {
	largest  int64
	function string
	limit    int64
	problems []string
	frames   []namedFrame
}

type namedFrame struct {
	name string
	size int64
}

func (r report) summary() string {
	if r.function == "" {
		return fmt.Sprintf("no stack frames (limit %d bytes)", r.limit)
	}
	return fmt.Sprintf("largest frame %d bytes in %s (limit %d bytes)", r.largest, r.function, r.limit)
}

func check(path string) (report, error) {
	file, err := os.Open(path)
	if err != nil {
		return report{}, err
	}
	magic := make([]byte, 4)
	_, err = io.ReadFull(file, magic)
	file.Close()
	if err != nil {
		return report{}, fmt.Errorf("reading header: %w", err)
	}
	switch {
	case bytes.Equal(magic, []byte("\x7fELF")):
		return checkUserspace(path)
	case bytes.Equal(magic[:2], []byte("MZ")):
		return checkKernel(path)
	}
	return report{}, fmt.Errorf("neither an ELF nor a PE image")
}

func checkUserspace(path string) (report, error) {
	var stdout, stderr bytes.Buffer
	command := exec.Command("go", "tool", "objdump", path)
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return report{}, fmt.Errorf("go tool objdump: %v: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	functions, err := parseObjdump(&stdout)
	if err != nil {
		return report{}, err
	}
	if len(functions) == 0 {
		return report{}, fmt.Errorf("go tool objdump listed no functions")
	}
	return checkFunctions(functions, userFrameLimit), nil
}
