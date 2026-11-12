package main

import "testing"

func TestExt4BenchParsers(t *testing.T) {
	df := "Filesystem  Mount       Size      Used      Free      Flags\next4        /mnt/bench  1.0G      50.6M     973.4M    rw\n"
	if free, ok := dfFreeMiB(df, "/mnt/bench"); !ok || free != 973 {
		t.Fatalf("free %d %v", free, ok)
	}
	if _, ok := dfFreeMiB(df, "/mnt/other"); ok {
		t.Fatal("found a mount df does not list")
	}
	if filled, ok := fillMiB([]string{"bench start dir=x fill", "bench fill mib=969 us=5"}); !ok || filled != 969 {
		t.Fatalf("filled %d %v", filled, ok)
	}
}
