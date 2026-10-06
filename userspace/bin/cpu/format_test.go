package main

import (
	"strings"
	"testing"

	"goos/kernel/kcall"
)

func TestCompactTable(t *testing.T) {
	records := []kcall.CPURuntime{
		{Index: 0, Online: true, Current: 1<<32 | 13, Ticks: 400, IdleTicks: 328, Switches: 18420, Steals: 31, Migrations: 94},
		{Index: 1, Online: true, Ticks: 400, IdleTicks: 104, Switches: 19310, Steals: 88, Migrations: 121},
		{Index: 2, Online: false},
	}
	got := string(appendTable(nil, records, false))
	want := "CPU STATE   BUSY   IDLE   TASK  SWITCHES  STEALS  MIGRATIONS\n" +
		"0   online  18%    82%    12    18420     31      94\n" +
		"1   online  74%    26%    idle  19310     88      121\n" +
		"2   offline 0%     100%   -     0         0       0\n"
	if got != want {
		t.Fatalf("table:\n%s\nwant:\n%s", got, want)
	}
}

func TestRawTableAndZeroTicks(t *testing.T) {
	records := []kcall.CPURuntime{{Index: 3, Online: true, Reschedules: 7, Shootdowns: 2}}
	got := string(appendTable(nil, records, true))
	if !strings.HasPrefix(got, "CPU STATE   TICKS  IDLE   TASK  SWITCHES  STEALS  MIGRATIONS  RESCHED  SHOOTDOWN\n") {
		t.Fatalf("raw header:\n%s", got)
	}
	if !strings.HasSuffix(got, "3   online  0      0      idle  0         0       0           7        2\n") {
		t.Fatalf("raw row:\n%s", got)
	}
	if compact := string(appendTable(nil, records, false)); !strings.Contains(compact, "0%     100%") {
		t.Fatalf("a processor with no ticks:\n%s", compact)
	}
}
