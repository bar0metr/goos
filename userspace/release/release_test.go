package release

import (
	"bytes"
	"goos/internal/version"
	"strings"
	"testing"
)

func TestCurrentRecordRoundTrips(t *testing.T) {
	current := Current()
	encoded := Encode(&current)
	parsed, problem := Parse(encoded)
	if problem != OK || Check(&parsed, &current) != OK {
		t.Fatalf("round trip of %q = %v", encoded, problem)
	}
	if string(parsed.ReleaseText()) != version.Release || string(parsed.ArchText()) != version.Machine {
		t.Fatalf("parsed %q %q", parsed.ReleaseText(), parsed.ArchText())
	}
	if !bytes.HasPrefix(encoded, []byte("format 1\n")) {
		t.Fatalf("record does not start with its format: %q", encoded)
	}
}

func TestParseIsStrict(t *testing.T) {
	current := Current()
	valid := string(Encode(&current))
	cases := map[string]Problem{
		"":                                                                 ProblemMissing,
		strings.Repeat("x", MaxSize+1):                                     ProblemTooLarge,
		strings.TrimSuffix(valid, "\n"):                                    ProblemMalformed,
		"arch x86_64\n" + valid:                                            ProblemMissing,
		"format 2\narch anything at all here\n":                            ProblemFormat,
		"format 01\n":                                                      ProblemMalformed,
		valid + "arch x86_64\n":                                            ProblemDuplicate,
		valid + "colour blue\n":                                            ProblemUnknownKey,
		strings.Replace(valid, "arch ", "arch  ", 1):                       ProblemMalformed,
		strings.Replace(valid, "\n", "\r\n", 1):                            ProblemMalformed,
		strings.Replace(valid, "startup ", "startup -", 1):                 ProblemMalformed,
		strings.Replace(valid, "image ", "image 99999999999", 1):           ProblemMalformed,
		"format 1\narch x86_64\nrelease 0.2.2\nstartup 4\nimage 2\n":       ProblemMissing,
		"format 1\narch x86_64\nrelease " + strings.Repeat("9", 33) + "\n": ProblemMalformed,
		"format 1\nArch x86_64\n":                                          ProblemMalformed,
		"format 1\n\n":                                                     ProblemMalformed,
	}
	for input, want := range cases {
		if _, problem := Parse([]byte(input)); problem != want {
			t.Errorf("Parse(%q) = %v, want %v", input, problem, want)
		}
	}
}

func TestCheckNamesTheFirstMismatch(t *testing.T) {
	required := Current()
	cases := []struct {
		edit func(*Record)
		want Problem
	}{
		{func(record *Record) { record.ArchSize = uint8(copy(record.Arch[:], "aarch64")) }, ProblemArch},
		{func(record *Record) { record.ReleaseSize = uint8(copy(record.Release[:], "0.0.1")) }, ProblemRelease},
		{func(record *Record) { record.Startup++ }, ProblemStartup},
		{func(record *Record) { record.Image++ }, ProblemImage},
		{func(record *Record) { record.Index++ }, ProblemIndex},
	}
	for _, test := range cases {
		installed := Current()
		test.edit(&installed)
		encoded := Encode(&installed)
		parsed, problem := Parse(encoded)
		if problem != OK {
			t.Fatalf("Parse(%q) = %v", encoded, problem)
		}
		if got := Check(&parsed, &required); got != test.want {
			t.Errorf("Check(%q) = %v, want %v", encoded, got, test.want)
		}
	}
}
