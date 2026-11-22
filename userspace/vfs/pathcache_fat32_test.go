package vfsservice

import (
	"testing"

	"goos/kernel/vfs"
)

// A FAT32 mutation must drop every case/alias spelling of a name: unlinking
// through "foo" must not leave a warm "Foo" cache entry that outlives the
// directory entry it named (ADR 0230, FIX §1).
func TestFATCaseFoldedMutationDropsStaleSpelling(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/Foo")})
	// Warm the cache under the spelled "Foo" name.
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Foo")}); reply.Status != vfs.StatusOK {
		t.Fatalf("warm stat = %d", reply.Status)
	}
	// Unlink through a different spelling; FAT32 resolves the same entry.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/foo")})
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/Foo")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stale spelling survived: status %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/Foo")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stale open: status %d", reply.Status)
	}
}

// Recreating a name after an unlink resolves afresh: a reused FAT32 directory
// slot can never revive a stale cached node, because every FAT32 mutation
// flushes the component cache (ADR 0230, FIX §1).
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
