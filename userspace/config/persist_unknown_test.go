package config

import (
	"bytes"
	"goos/internal/limits"
	"goos/kernel/vfs"
	"goos/userspace/safesave"
	"testing"
)

func TestDurableAmbiguousReplacementAndForwardRetry(t *testing.T) {
	for _, status := range []vfs.Status{vfs.StatusPeerDied, vfs.StatusTimeout, vfs.StatusBackendError, vfs.StatusCorrupt, vfs.StatusNoSpace} {
		for _, committed := range []bool{false, true} {
			var store Store
			initial, _ := Text([]byte("goos"))
			candidate, _ := Text([]byte("saved-9"))
			store.Declare(HostnameKey, initial, false)
			store.Declare("user.name", initial, false)
			scope, _ := MakeExactScope(HostnameKey)
			root, _ := MakeScope("")
			files := &persistenceFiles{files: map[string][]byte{"/state/config": []byte("system.hostname text goos\nuser.name text goos\n")}, ambiguous: status, commitOnError: committed}
			var temporary [limits.PathMax]byte
			var buffer [vfs.MaxProtocolData]byte
			persist := func(candidate *Store) Status {
				status, stage := PersistStore(files, []byte("/state/config"), candidate, buffer[:], &temporary)
				return PersistenceStatus(status, stage)
			}
			before, _ := store.Revision(scope, Read)
			request := Request{Operation: OperationSetDurable, Key: []byte(HostnameKey), Value: candidate}
			reply := store.ServeDurable(scope, Read|DurableWrite, request, persist)
			got, _ := store.Get(scope, Read, request.Key)
			after, _ := store.Revision(scope, Read)
			if reply.Status != StatusPersistUnknown || got != candidate || after <= before {
				t.Fatal("unknown replaced with rollback/success", status, committed, reply, got)
			}
			if committed {
				var restarted Store
				restarted.Declare(HostnameKey, initial, false)
				first, _, _ := bytes.Cut(files.files["/state/config"], []byte{'\n'})
				key, value, ok := ParseEntry(first)
				if !ok || restarted.Set(root, Write, key, value) != StatusOK {
					t.Fatal("restart parse")
				}
				got, _ = restarted.Get(scope, Read, request.Key)
				if got != candidate {
					t.Fatal("restart/Get disagree after committed but unacknowledged replacement")
				}
			}
			// A later ordinary mutation remains volatile-success and must not
			// export the old hostname over a replacement that may have committed.
			user, _ := Text([]byte("operator"))
			if store.Serve(root, Write, Request{Operation: OperationSet, Key: []byte("user.name"), Value: user}).Status != StatusOK {
				t.Fatal("ordinary Set changed")
			}
			files.ambiguous = vfs.StatusOK
			// Session renewal or repeating the same request reconciles forward.
			if reply = store.ServeDurable(scope, Read|DurableWrite, request, persist); reply.Status != StatusOK {
				t.Fatal("retry not acknowledged", reply)
			}
			revision, _ := store.Revision(scope, Read)
			if revision != after {
				t.Fatal("idempotent retry changed hostname revision")
			}
			if !bytes.Contains(files.files["/state/config"], []byte("system.hostname text saved-9\n")) || !bytes.Contains(files.files["/state/config"], []byte("user.name text operator\n")) {
				t.Fatal("retry lost candidate or intervening Set", files.files)
			}
		}
	}
}

func TestPersistenceOutcomeClassification(t *testing.T) {
	for _, status := range []vfs.Status{vfs.StatusPeerDied, vfs.StatusTimeout, vfs.StatusBackendError, vfs.StatusCorrupt, vfs.StatusNoSpace} {
		if PersistenceStatus(status, safesave.StageReplace) != StatusPersistUnknown {
			t.Fatal("replacement ambiguity")
		}
		if PersistenceStatus(status, safesave.StageWrite) != StatusPersistFailed {
			t.Fatal("staging error reported ambiguous commit")
		}
	}
	if PersistenceStatus(vfs.StatusPermissionDenied, safesave.StageReplace) != StatusPersistFailed || PersistenceStatus(vfs.StatusOK, 0) != StatusOK {
		t.Fatal("classification")
	}
	message, ok := EncodeReply(Reply{Operation: OperationSetDurable, Status: StatusPersistUnknown}, 1)
	if !ok {
		t.Fatal("unknown encode")
	}
	if reply, ok := DecodeReply(message); !ok || reply.Status != StatusPersistUnknown {
		t.Fatal("unknown decode")
	}
}
