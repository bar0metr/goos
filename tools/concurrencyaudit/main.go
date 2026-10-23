// concurrencyaudit checks that every package-level variable under userspace/
// has a row in the native concurrency audit
// (docs/development/native-concurrency-audit.md).
//
//	concurrencyaudit [-root DIR] [-audit FILE] [-list]
//
// It parses every non-test Go file under DIR/userspace, whatever its build
// tags, and collects each package-level var by package path and name. With
// -list it prints them as "package<TAB>name<TAB>file:line" and exits. Without
// it, it compares them with the audit table FILE and exits 1 when a variable
// has no row, a row names a variable that no longer exists, or the table is
// malformed.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	root := flag.String("root", ".", "repository root")
	audit := flag.String("audit", "docs/development/native-concurrency-audit.tsv", "audit table, relative to the root")
	list := flag.Bool("list", false, "print the package-level variables and exit")
	flag.Parse()
	os.Exit(run(*root, *audit, *list, os.Stdout, os.Stderr))
}

func run(root, auditPath string, list bool, stdout, stderr io.Writer) int {
	vars, err := scan(root)
	if err != nil {
		fmt.Fprintln(stderr, "concurrencyaudit:", err)
		return 2
	}
	if list {
		for _, v := range vars {
			fmt.Fprintf(stdout, "%s\t%s\t%s:%d\n", v.Package, v.Name, v.File, v.Line)
		}
		return 0
	}
	if !filepath.IsAbs(auditPath) {
		auditPath = filepath.Join(root, auditPath)
	}
	file, err := os.Open(auditPath)
	if err != nil {
		fmt.Fprintln(stderr, "concurrencyaudit:", err)
		return 2
	}
	rows, problems := parseAudit(file)
	file.Close()
	problems = append(problems, compare(vars, rows)...)
	for _, p := range problems {
		fmt.Fprintln(stderr, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "concurrencyaudit: %d problem(s); see docs/development/native-concurrency-audit.md\n", len(problems))
		return 1
	}
	fmt.Fprintf(stdout, "concurrencyaudit: %d package-level variables, all have inventory rows\n", len(vars))
	return 0
}
