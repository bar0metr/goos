package main

import (
	"strings"
	"testing"
)

func TestNativeRecoveryRequiresNewMarkersWithoutVFSRestart(t *testing.T) {
	before := "VFS_SERVICE_OK\nSYSTEM_REATTACHED_OK\n"
	events := "FAT32_STALLED\nVFS_CONTROL_REPLACED_OK\nFAT32_STALL_REPLACING\nFILE_CACHE_RECOVERED_OK\nFAT32_RESTARTED_OK\nSYSTEM_REATTACHED_OK\n"
	if err := nativeRecoveryMarkers(before, before+events); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{"FAT32_STALLED", "VFS_CONTROL_REPLACED_OK", "FAT32_STALL_REPLACING", "FILE_CACHE_RECOVERED_OK", "FAT32_RESTARTED_OK"} {
		if err := nativeRecoveryMarkers(before, before+strings.ReplaceAll(events, missing+"\n", "")); err == nil {
			t.Fatalf("accepted missing %s", missing)
		}
	}
	for _, bad := range []string{"FAT32_STALL_FAILED", "FILE_CACHE_RECOVERY_LIMIT", "VFS_SERVICE_OK", "FILE_CACHE_RECOVERED_OK"} {
		if err := nativeRecoveryMarkers(before, before+events+bad+"\n"); err == nil {
			t.Fatalf("accepted extra %s", bad)
		}
	}
	if err := nativeRecoveryMarkers(events, events); err == nil {
		t.Fatal("accepted old markers as a new recovery")
	}
}

func TestNativeRegionRequiresCompletedRendezvousCounts(t *testing.T) {
	valid := "THREADPROBE_REGION_WINDOW_OK rounds=16 rejected=64 region_retries=256 collections=48"
	if err := nativeRegionMarker(valid+"\r\n", 16); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"", "prefix " + valid, valid + "\n" + valid,
		strings.ReplaceAll(valid, "rounds=16", "rounds=24"),
		strings.ReplaceAll(valid, "rejected=64", "rejected=0"),
		strings.ReplaceAll(valid, "region_retries=256", "region_retries=63"),
		strings.ReplaceAll(valid, "collections=48", "collections=0"),
		strings.ReplaceAll(valid, "collections=48", ""),
		valid + " rounds=16", valid + " malformed", valid + " collections=oops",
		"THREADPROBE_FAIL roots\n" + valid,
	} {
		if err := nativeRegionMarker(bad, 16); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
