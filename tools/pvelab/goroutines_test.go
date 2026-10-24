package main

import (
	"strings"
	"testing"
)

func TestGoroutineMarkerRequiresOneCompletedFixture(t *testing.T) {
	marker := "GOPROBE_POOL_OK"
	if err := goroutineMarker("GOPROBE_POOL jobs=100 sum=9900\r\n"+marker+"\r\n", marker); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		"", "prefix " + marker, marker + " suffix", marker + "\n" + marker,
		"GOPROBE_FAIL corrupt result\n" + marker,
		"fatal error: deadlock\n" + marker,
	} {
		if err := goroutineMarker(output, marker); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}

// native-concurrency delegates its runtime checks to goroutinesScenario.
func TestGoroutineChecksIncludeCollectorStress(t *testing.T) {
	for _, command := range []string{"goprobe gcstress 1", "goprobe gcstress 8"} {
		found := false
		for _, check := range goroutineChecks {
			if check.command == command {
				found = check.marker == "GOPROBE_GCSTRESS_OK" && len(check.records) == 1 && check.records[0] == "GOPROBE_GCSTRESS"
			}
		}
		if !found {
			t.Errorf("missing collector stress check: %s", command)
		}
	}
}

func TestGoroutineRecordRequiresOneNamedRecord(t *testing.T) {
	name := "GOPROBE_WAITSETS_JOIN"
	if err := goroutineRecord("GOPROBE_WAITSETS polls=3 mask=1\r\n"+name+" spins=40\r\nGOPROBE_WAITSETS_OK\r\n", name); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		"", name, "GOPROBE_WAITSETS_JOINED spins=1", name + " spins=1\n" + name + " spins=2",
	} {
		if err := goroutineRecord(output, name); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}

func TestGoroutineChecksHoldEveryContextSaveInstruction(t *testing.T) {
	found := false
	for _, check := range goroutineChecks {
		if check.command == "goprobe swaphold" {
			found = check.marker == "GOPROBE_SWAPHOLD_OK" && check.verify != nil
		}
	}
	if !found {
		t.Fatal("missing instruction-hold check")
	}
}

func TestSwapHoldRecordsRequireEveryBoundaryOfBothRegions(t *testing.T) {
	swap := "0,2,4,6,8,9,10,13,16,17,18,20,22,24,26"
	preempt := "0,2,3,4,5,6,7,8,9,10,12,14,16,18,20,22,24,26,29,36,40,45,52,57,59,62,64,65,70,75,78,80,82,84,86,88,90,92,94,95,96,97,98,99,100,101,102"
	good := "GOPROBE_SWAPHOLD region=swap held=45 distinct_rips=15 expected=15 collections=45 retried=0 lost=0 offsets=" + swap +
		" offsets_out=" + swap + " offsets_in=" + swap + "\r\n" +
		"GOPROBE_SWAPHOLD region=preempt held=94 distinct_rips=47 expected=47 collections=94 retried=0 lost=0 offsets=" + preempt + "\r\n" +
		"GOPROBE_SWAPHOLD_OK\r\n"
	if err := swapHoldRecords(good); err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{
		"missed":         strings.Replace(good, "distinct_rips=47", "distinct_rips=46", 1),
		"lost":           strings.Replace(good, "lost=0", "lost=2", 1),
		"offset missing": strings.Replace(good, "offsets=0,2,3,", "offsets=0,3,", 1),
		"offset doubled": strings.Replace(good, "offsets=0,2,3,", "offsets=0,0,3,", 1),
		"direction":      strings.Replace(good, "offsets_in=0,2,4,", "offsets_in=0,4,", 1),
		"no direction":   strings.Replace(good, " offsets_out=", " other=", 1),
		"no offsets":     strings.Replace(good, " offsets="+preempt, "", 1),
		"no preempt":     strings.Split(good, "\r\n")[0],
		"twice":          good + strings.Split(good, "\r\n")[0],
		"region":         strings.Replace(good, "region=swap", "region=other", 1),
	} {
		if err := swapHoldRecords(output); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
func TestRegRootsRecordsRequireEveryClassRegisterAndSituation(t *testing.T) {
	records := []string{
		"GOPROBE_REGROOTS class=xmm situation=thread regs=16 collections=80 lost=0 controls=4 reclaimed=4",
		"GOPROBE_REGROOTS class=xmm situation=goroutine regs=16 collections=128 lost=0 running=64 switched=64 permits=3 " +
			"running_visits=90 running_preemptions=0 entry_regs=16 switched_preemptions=64 controls=4 reclaimed=4",
		"GOPROBE_REGROOTS class=gpr situation=thread regs=15 collections=76 lost=0 controls=4 reclaimed=4",
		"GOPROBE_REGROOTS class=gpr situation=goroutine regs=15 collections=120 lost=0 running=60 switched=60 permits=3 " +
			"running_visits=80 running_preemptions=0 entry_regs=15 switched_preemptions=60 controls=4 reclaimed=4",
	}
	good := strings.Join(records, "\r\n") + "\r\nGOPROBE_REGROOTS_OK\r\n"
	if err := regRootsRecords(good); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		"",
		records[0] + "\n" + records[1],
		strings.Join(records[:3], "\n"),
		good + records[0] + "\n",
		strings.Replace(good, "regs=16", "regs=15", 1),
		strings.Replace(good, "regs=15", "regs=16", 1),
		strings.Replace(good, "lost=0", "lost=1", 1),
		strings.Replace(good, "collections=76", "collections=59", 1),
		strings.Replace(good, "reclaimed=4", "reclaimed=3", 1),
		strings.Replace(good, "switched=60", "switched=10", 1),
		strings.Replace(good, "switched_preemptions=64", "switched_preemptions=0", 1),
		strings.Replace(good, "entry_regs=15", "entry_regs=0", 1),
		strings.Replace(good, "regs=16", "regs=x", 1),
		strings.Replace(good, "class=gpr situation=thread", "class=other situation=thread", 1),
		good + "GOPROBE_REGROOTS_LOST class=gpr situation=thread reg=7 collections=4 entered=0 kept=1 bad_cell=1\n",
		good + "GOPROBE_REGROOTS_KEPT class=xmm situation=thread collections=4\n",
	} {
		if err := regRootsRecords(output); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}

func TestGoroutineChecksIncludeRegRoots(t *testing.T) {
	for _, check := range goroutineChecks {
		if check.command == "goprobe regroots" && check.marker == "GOPROBE_REGROOTS_OK" && check.verify != nil {
			return
		}
	}
	t.Fatal("missing goprobe regroots check")
}

// Each check's records and verifier must accept the fixture output they
// were written for: a command that prints a record per region or register
// class cannot also list that record as one that appears once.
func TestGoroutineChecksAcceptTheirFixtureOutput(t *testing.T) {
	swap := "0,2,4,6,8,9,10,13,16,17,18,20,22,24,26"
	preempt := "0,2,3,4,5,6,7,8,9,10,12,14,16,18,20,22,24,26,29,36,40,45,52,57,59,62,64,65,70,75,78,80,82,84,86,88,90,92,94,95,96,97,98,99,100,101,102"
	outputs := map[string]string{
		"goprobe swaphold": "GOPROBE_SWAPHOLD region=swap held=45 distinct_rips=15 expected=15 collections=45 retried=0 lost=0 offsets=" + swap +
			" offsets_out=" + swap + " offsets_in=" + swap + "\n" +
			"GOPROBE_SWAPHOLD region=preempt held=94 distinct_rips=47 expected=47 collections=94 retried=0 lost=0 offsets=" + preempt + "\n" +
			"GOPROBE_SWAPHOLD_OK\n",
	}
	for _, check := range goroutineChecks {
		out, found := outputs[check.command]
		if !found {
			continue
		}
		if err := goroutineMarker(out, check.marker); err != nil {
			t.Fatalf("%s marker: %v", check.command, err)
		}
		for _, name := range check.records {
			if err := goroutineRecord(out, name); err != nil {
				t.Fatalf("%s record %s: %v", check.command, name, err)
			}
		}
		if check.verify != nil {
			if err := check.verify(out); err != nil {
				t.Fatalf("%s verify: %v", check.command, err)
			}
		}
	}
}
