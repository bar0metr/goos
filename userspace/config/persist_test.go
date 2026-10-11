package config

import (
	"bytes"
	"goos/internal/limits"
	"goos/kernel/vfs"
	"testing"
)

type persistenceFiles struct {
	files         map[string][]byte
	open          string
	fail          string
	short         bool
	ambiguous     vfs.Status
	commitOnError bool
}

func (files *persistenceFiles) Create(path []byte) vfs.Status {
	if files.fail == "create" {
		return vfs.StatusPermissionDenied
	}
	if _, found := files.files[string(path)]; found {
		return vfs.StatusAlreadyExists
	}
	files.files[string(path)] = nil
	return vfs.StatusOK
}
func (files *persistenceFiles) Unlink(path []byte) vfs.Status {
	delete(files.files, string(path))
	return vfs.StatusOK
}
func (files *persistenceFiles) OpenWrite(path []byte) (vfs.Handle, vfs.Status) {
	if files.fail == "open" {
		return 0, vfs.StatusPermissionDenied
	}
	files.open = string(path)
	return 1, vfs.StatusOK
}
func (files *persistenceFiles) Write(_ vfs.Handle, data []byte) (int, vfs.Status) {
	if files.fail == "write" {
		return 0, vfs.StatusNoSpace
	}
	if files.short {
		return 0, vfs.StatusOK
	}
	files.files[files.open] = append(files.files[files.open], data...)
	return len(data), vfs.StatusOK
}
func (files *persistenceFiles) Close(vfs.Handle) vfs.Status {
	if files.fail == "close" {
		return vfs.StatusPeerDied
	}
	return vfs.StatusOK
}
func (files *persistenceFiles) Replace(source, target []byte) vfs.Status {
	if files.fail == "replace" {
		return vfs.StatusPermissionDenied
	}
	if files.ambiguous != vfs.StatusOK && !files.commitOnError {
		return files.ambiguous
	}
	files.files[string(target)] = files.files[string(source)]
	delete(files.files, string(source))
	return files.ambiguous
}

func TestAcknowledgedDurableStoreFailuresKeepPreviousState(t *testing.T) {
	for _, failure := range []string{"create", "open", "write", "close", "replace", "short", ""} {
		t.Run(failure, func(t *testing.T) {
			var store Store
			initial, _ := Text([]byte("goos"))
			replacement, _ := Text([]byte("saved-9"))
			store.Declare(HostnameKey, initial, false)
			store.Declare("system.readonly", Uint(9), true)
			scope, _ := MakeExactScope(HostnameKey)
			old := []byte("system.hostname text goos\n")
			files := &persistenceFiles{files: map[string][]byte{"/state/config": old}, fail: failure, short: failure == "short"}
			var temporary [limits.PathMax]byte
			var buffer [7]byte // Force exports across many fixed-size writes.
			before, _ := store.Revision(scope, Read)
			reply := store.ServeDurable(scope, Read|DurableWrite, Request{Operation: OperationSetDurable, Key: []byte(HostnameKey), Value: replacement}, func(candidate *Store) Status {
				status, stage := PersistStore(files, []byte("/state/config"), candidate, buffer[:], &temporary)
				return PersistenceStatus(status, stage)
			})
			got, _ := store.Get(scope, Read, []byte(HostnameKey))
			after, _ := store.Revision(scope, Read)
			if failure != "" {
				if reply.Status != StatusPersistFailed || got != initial || before != after || !bytes.Equal(files.files["/state/config"], old) {
					t.Fatal("failed persistence changed committed state", reply, got, files.files)
				}
			} else {
				if reply.Status != StatusOK || got != replacement || after <= before {
					t.Fatal("commit", reply, got)
				}
				if string(files.files["/state/config"]) != "system.hostname text saved-9\n" {
					t.Fatal("export", files.files)
				}
				var restarted Store
				restarted.Declare(HostnameKey, initial, false)
				key, value, ok := ParseEntry(bytes.TrimSuffix(files.files["/state/config"], []byte{'\n'}))
				root, _ := MakeScope("")
				if !ok || restarted.Set(root, Write, key, value) != StatusOK {
					t.Fatal("restart restore")
				}
				got, _ = restarted.Get(scope, Read, []byte(HostnameKey))
				if got != replacement {
					t.Fatal("restart hostname")
				}
			}
		})
	}
}
