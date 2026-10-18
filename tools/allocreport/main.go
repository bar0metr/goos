// allocreport measures the native userspace allocator and writes the
// allocator report (docs/development/testing.md).
//
//	allocreport bench -baseline REV [-count N] [-benchtime T] [-bench RE]
//	    [-variant NAME=SIZES]... -out FILE.json
//	allocreport report [-bench FILE.json] [-guest FILE]... [-title TEXT] -out FILE.md
//
// bench runs the host benchmarks of userspace/runtime/arena for the working
// tree, for REV, and for each variant, a size-class table given as
// comma-separated slot sizes and applied to the working tree. It never
// changes a file: the other revisions and tables reach the compiler through
// go test -overlay, and every revision runs the working tree's
// bench_test.go. Runs alternate between the trees, so drift on the host
// spreads over all of them.
//
// report turns benchmark results and the guest profiles that
// tests/boot/allocator.sh writes into one Markdown report.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "bench":
		err = benchCommand(os.Args[2:])
	case "report":
		err = reportCommand(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "allocreport:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: allocreport bench -baseline REV [-count N] [-benchtime T] [-bench RE] [-variant NAME=SIZES]... -out FILE.json")
	fmt.Fprintln(os.Stderr, "       allocreport report [-bench FILE.json] [-guest FILE]... [-title TEXT] -out FILE.md")
	os.Exit(2)
}

// listFlag collects a repeated flag.
type listFlag []string

func (l *listFlag) String() string     { return fmt.Sprint(*l) }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }
