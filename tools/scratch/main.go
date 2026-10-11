package main

import (
	"fmt"
	"os"

	"goos/internal/repobuild"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s PREFIX\n", os.Args[0])
		os.Exit(2)
	}
	root, err := repobuild.FindRoot("")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	directory, err := repobuild.NewScratch(root, os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(directory)
}
