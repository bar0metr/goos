package main

import "testing"

func TestNetdDiagLinesBecomeCounters(t *testing.T) {
	out := "goos> net status\r\nlo:\r\n  frames: rx 2 tx 7 drop 0 malformed 0\r\n" +
		"[2026-11-05T11:09:22.603174Z] WARN  netd[16]: netd loop loops=11960 advance-us=540499 deliver-us=1024591\r\n" +
		"[2026-11-05T11:09:22.619485Z] WARN  netd[16]: netd wait prepare-us=921233 wait-us=26717342 service-us=2531982\r\n" +
		"[2026-11-05T11:09:22.572148Z] WARN  netd[16]: tcp lookups match=88162 listener=96 gtcp3-req=12029 gtcp3-rep=8029\r\n" +
		"[2026-11-05T11:09:22.6Z] WARN  netd[16]: tcp endpoint id=5 owner=1\r\n"
	counters := parseDiagCounters("DIAG begin tick=1\n" + netdDiagLines(out))
	if counters["netd-loop"]["advance-us"] != 540499 || counters["netd-wait"]["service-us"] != 2531982 {
		t.Fatalf("stages %v", counters)
	}
	if counters["tcp-lookups"]["gtcp3-req"] != 12029 {
		t.Fatalf("lookups %v", counters)
	}
	if counters["frames"]["tx"] != 7 || counters["frames"]["rx"] != 2 {
		t.Fatalf("frames %v", counters["frames"])
	}
	if _, found := counters["tcp-endpoint"]; found {
		t.Fatal("an endpoint record became a counter")
	}
}
