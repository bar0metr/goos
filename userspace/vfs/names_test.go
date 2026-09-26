package vfsservice

import (
	"bytes"
	"goos/internal/limits"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"strings"
	"testing"
)

// TestNativeNameAndPathLimits covers the one naming contract every layer
// shares (ADR 0110): a 255-byte name and a path near 4096 bytes work in RAMFS
// and on the FAT32 volume, and anything longer is refused as too long.
func TestNativeNameAndPathLimits(t *testing.T) {
	service := startedService(t, testFAT32Device())
	for _, root := range []string{"/tmp", "/persist"} {
		dispatch := func(request vfs.Request) vfs.Reply {
			t.Helper()
			return service.Dispatch(testSession, request)
		}
		name := strings.Repeat("n", limits.NameMax)
		path := []byte(root + "/" + name)
		if reply := dispatch(vfs.Request{Operation: vfs.OperationCreate, Path: path}); reply.Status != vfs.StatusOK {
			t.Fatalf("%s: a %d-byte name refused: %d", root, limits.NameMax, reply.Status)
		}
		listed := dispatch(vfs.Request{Operation: vfs.OperationReadDir, Path: []byte(root)})
		found := false
		for cursor := 0; listed.Status == vfs.StatusOK; listed = dispatch(vfs.Request{Operation: vfs.OperationReadDir, Path: []byte(root), Offset: uint64(cursor)}) {
			found = found || string(listed.Name) == name
			cursor = int(listed.Cursor)
		}
		if !found {
			t.Fatalf("%s: the long name is not listed", root)
		}
		target := []byte(root + "/" + strings.Repeat("m", limits.NameMax))
		if reply := dispatch(vfs.Request{Operation: vfs.OperationRename, Path: path, Target: target}); reply.Status != vfs.StatusOK {
			t.Fatalf("%s: rename to a long name: %d", root, reply.Status)
		}
		if reply := dispatch(vfs.Request{Operation: vfs.OperationStat, Path: target}); reply.Status != vfs.StatusOK {
			t.Fatalf("%s: the renamed file is missing: %d", root, reply.Status)
		}
		tooLong := []byte(root + "/" + strings.Repeat("x", limits.NameMax+1))
		if reply := dispatch(vfs.Request{Operation: vfs.OperationCreate, Path: tooLong}); reply.Status != vfs.StatusNameTooLong {
			t.Fatalf("%s: a %d-byte name answered %d", root, limits.NameMax+1, reply.Status)
		}
		// A deep path: directories of 200-byte names down to near PathMax.
		deep := []byte(root)
		for len(deep)+201 <= limits.PathMax-10 {
			deep = append(deep, '/')
			deep = append(deep, bytes.Repeat([]byte{'d'}, 200)...)
			if reply := dispatch(vfs.Request{Operation: vfs.OperationMkdir, Path: deep}); reply.Status != vfs.StatusOK {
				t.Fatalf("%s: mkdir at depth %d bytes: %d", root, len(deep), reply.Status)
			}
		}
		file := append(append([]byte(nil), deep...), "/leaf"...)
		if reply := dispatch(vfs.Request{Operation: vfs.OperationCreate, Path: file}); reply.Status != vfs.StatusOK {
			t.Fatalf("%s: a %d-byte path refused: %d", root, len(file), reply.Status)
		}
		if reply := dispatch(vfs.Request{Operation: vfs.OperationStat, Path: file}); reply.Status != vfs.StatusOK || reply.Kind != ramfs.KindFile {
			t.Fatalf("%s: stat of a %d-byte path: %d", root, len(file), reply.Status)
		}
		over := append(append([]byte(nil), deep...), '/')
		over = append(over, bytes.Repeat([]byte{'o'}, limits.PathMax-len(deep))...)
		if reply := dispatch(vfs.Request{Operation: vfs.OperationCreate, Path: over}); reply.Status != vfs.StatusNameTooLong {
			t.Fatalf("%s: a %d-byte path answered %d", root, len(over), reply.Status)
		}
	}
}
