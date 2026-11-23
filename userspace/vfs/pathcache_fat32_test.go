package vfsservice

import (
	"testing"

	"goos/kernel/vfs"
)

// A FAT32 mutation must drop every case/alias spelling of a name, and a
// deleted directory slot must not let an old pathname name a later object.
// This test unlinks through a different spelling ("foo"), then forces "Bar"
// into the freed slot and proves "Foo"/"foo" no longer resolve (ADR 0230,
// FIX 2 §1, §2).
func TestFATCaseFoldedUnlinkThenSlotReuse(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/Foo")})
	// Warm the cache under the spelled "Foo" name and capture its location ID.
	fooStat := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Foo")})
	// Unlink through a different spelling; FAT32 resolves the same entry.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/foo")})
	// Create a different name; it must land in the freed directory slot.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/Bar")})
	barStat := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Bar")})
	if fooStat.Node != barStat.Node {
		t.Fatalf("slot not reused: Foo node %d, Bar node %d", fooStat.Node, barStat.Node)
	}
	// A stale cached spelling must never name the reused slot's new occupant.
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Foo")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stale Foo spelling survived: status %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/foo")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stale foo spelling survived: status %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/Foo")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stale open survived: status %d", reply.Status)
	}
}

// A deleted FAT32 directory slot reused by a different name must never let the
// old pathname resolve to the new object, even though the location-based
// NodeID is identical (ADR 0230, FIX 2 §1).
func TestFATDeletedSlotReuseCannotReviveStaleName(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/Old")})
	oldStat := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Old")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/Old")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/New")})
	newStat := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/New")})
	if oldStat.Node != newStat.Node {
		t.Fatalf("slot not reused: Old node %d, New node %d", oldStat.Node, newStat.Node)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Old")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stale Old resolved to a new object: status %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/New")}); reply.Status != vfs.StatusOK {
		t.Fatalf("New did not resolve: status %d", reply.Status)
	}
}

// Recreating the same name after an unlink resolves afresh (ADR 0230, FIX 2).
func TestFATRecreateAfterUnlinkResolvesFresh(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/Entry")})
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Entry")}); reply.Status != vfs.StatusOK {
		t.Fatalf("warm stat = %d", reply.Status)
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/Entry")})
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Entry")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("unlinked entry still resolves: status %d", reply.Status)
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/Entry")})
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Entry")}); reply.Status != vfs.StatusOK {
		t.Fatalf("recreated entry: status %d", reply.Status)
	}
}
