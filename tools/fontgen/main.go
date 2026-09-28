package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const glyphHeight = 24

func main() {
	input := flag.String("input", "", "Spleen 12x24 BDF input")
	output := flag.String("output", "", "generated Go output")
	commit := flag.String("commit", "", "upstream Spleen commit")
	flag.Parse()
	if *input == "" || *output == "" || *commit == "" {
		fatal("-input, -output, and -commit are required")
	}

	glyphs, err := parseBDF(*input)
	if err != nil {
		fatal("parse BDF: %v", err)
	}
	wanted := requiredScalars()
	var generated strings.Builder
	fmt.Fprintln(&generated, "// Code generated from Spleen 12x24; DO NOT EDIT.")
	fmt.Fprintln(&generated, "// Copyright (c) 2018-2026, Frederic Cambus.")
	fmt.Fprintln(&generated, "// Source: https://github.com/fcambus/spleen")
	fmt.Fprintf(&generated, "// Upstream commit: %s.\n", *commit)
	fmt.Fprintln(&generated, "// SPDX-License-Identifier: BSD-2-Clause")
	fmt.Fprintln(&generated)
	fmt.Fprintln(&generated, "package framebuffer")
	fmt.Fprintln(&generated)
	fmt.Fprintf(&generated, "var fontData = [%d][GlyphHeight]uint16{\n", len(wanted))
	for _, scalar := range wanted {
		rows, ok := glyphs[scalar]
		if !ok {
			fatal("BDF has no glyph for U+%04X", scalar)
		}
		fmt.Fprintf(&generated, "\t// U+%04X\n\t{", scalar)
		for index, row := range rows {
			if index != 0 {
				fmt.Fprint(&generated, ", ")
			}
			fmt.Fprintf(&generated, "0x%04x", row)
		}
		fmt.Fprintln(&generated, "},")
	}
	fmt.Fprintln(&generated, "}")
	if err := os.WriteFile(*output, []byte(generated.String()), 0o644); err != nil {
		fatal("write output: %v", err)
	}
}

func parseBDF(path string) (map[rune][]uint16, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	glyphs := make(map[rune][]uint16)
	var encoding rune = -1
	var rows []uint16
	inBitmap := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "ENCODING "):
			value, parseErr := strconv.ParseInt(strings.TrimPrefix(line, "ENCODING "), 10, 32)
			if parseErr != nil {
				return nil, parseErr
			}
			encoding = rune(value)
		case line == "BITMAP":
			rows = rows[:0]
			inBitmap = true
		case line == "ENDCHAR":
			if inBitmap && encoding >= 0 {
				if len(rows) != glyphHeight {
					return nil, fmt.Errorf("U+%04X has %d rows, want %d", encoding, len(rows), glyphHeight)
				}
				glyphs[encoding] = append([]uint16(nil), rows...)
			}
			inBitmap = false
			encoding = -1
		case inBitmap:
			value, parseErr := strconv.ParseUint(line, 16, 16)
			if parseErr != nil {
				return nil, parseErr
			}
			rows = append(rows, uint16(value))
		}
	}
	return glyphs, scanner.Err()
}

func requiredScalars() []rune {
	scalars := make([]rune, 0, 162)
	for scalar := rune(0x20); scalar <= 0x7e; scalar++ {
		scalars = append(scalars, scalar)
	}
	for scalar := rune(0x0410); scalar <= 0x044f; scalar++ {
		scalars = append(scalars, scalar)
	}
	return append(scalars, 0x0401, 0x0451, 0x25a1)
}

func fatal(format string, values ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}
