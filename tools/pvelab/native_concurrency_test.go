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

func TestDriverIRQRequiresParkedWaitsWithPeerProgress(t *testing.T) {
	valid := "VIRTIO_BLK_G_IRQ_OK waits=4 tries=6 peer=120"
	for _, boot := range []string{valid + "\r\n", valid + "\n" + valid + "\n"} {
		if err := driverIRQMarker(boot); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{
		"", "prefix " + valid, "VIRTIO_BLK_IRQ_OK",
		strings.ReplaceAll(valid, "waits=4", "waits=3"),
		strings.ReplaceAll(valid, "tries=6", "tries=3"),
		strings.ReplaceAll(valid, "peer=120", "peer=0"),
		strings.ReplaceAll(valid, " peer=120", ""),
		valid + " peer=1", valid + " extra=1", valid + " malformed",
		valid + "\n" + strings.ReplaceAll(valid, "peer=120", "peer=0"),
	} {
		if err := driverIRQMarker(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
