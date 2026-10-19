// allocreport measures the native userspace allocator and writes the
// allocator report (docs/development/testing.md).
//
//	allocreport bench -baseline REV [-count N] [-benchtime T] [-bench RE]
//	    [-variant NAME=SIZES]... -out FILE.json
//	allocreport report [-bench FILE.json] [-guest FILE]... [-title TEXT] -out FILE.md
//	allocreport count [-sizes] -name NAME FILE
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
//
// count prints how many complete allocator profiles process NAME left in
// FILE, a guest log or report: profiles whose every line and counter
// arrived, and with -sizes only those that carry a request histogram.
package main

import (
	"flag"
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
	case "count":
		err = countCommand(os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "       allocreport count [-sizes] -name NAME FILE")
	os.Exit(2)
}

func countCommand(args []string) error {
	flags := flag.NewFlagSet("count", flag.ExitOnError)
	name := flags.String("name", "", "process name")
	sizes := flags.Bool("sizes", false, "count only profiles with a request histogram")
	flags.Parse(args)
	if *name == "" || flags.NArg() != 1 {
		usage()
	}
	g, err := readGuestProfiles(flags.Arg(0), true)
	if err != nil {
		return err
	}
	count := g.Complete[*name]
	if *sizes && (g.Profiles[*name] == nil || len(g.Profiles[*name].Sizes) == 0) {
		count = 0
	}
	fmt.Println(count)
	return nil
}

// listFlag collects a repeated flag.
type listFlag []string

func (l *listFlag) String() string     { return fmt.Sprint(*l) }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }
