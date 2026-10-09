// Command generate rewrites the committed host-only TLS test fixtures.
package main

import (
	"flag"
	"fmt"
	"os"

	"goos/tools/tlsfixture/internal/generate"
)

func main() {
	out := flag.String("out", "tools/tlsfixture/testdata", "fixture output directory")
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := generate.Write(*out); err != nil {
		fmt.Fprintln(os.Stderr, "tlsfixture generate:", err)
		os.Exit(1)
	}
}
