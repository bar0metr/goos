package identityd

import (
	"bytes"
	"testing"

	"goos/userspace/identity"
	"goos/userspace/identityproto"
)

func request(op identityproto.Opcode) *identityproto.Request {
	return &identityproto.Request{Opcode: op}
}

func named(op identityproto.Opcode, name string) *identityproto.Request {
	return &identityproto.Request{Opcode: op, Name: []byte(name)}
}

func key(n byte) []byte {
	k := make([]byte, identity.KeySize)
	k[0] = n
	return k
}

func TestNewUnprovisioned(t *testing.T) {
	service := New()
	if service.State() != identityproto.StateUnprovisioned {
		t.Fatalf("new service state = %v, want Unprovisioned", service.State())
	}
	accounts, groups, keys := service.Counts()
	if accounts != 0 || groups != 0 || keys != 0 {
		t.Fatalf("new service counts = %d,%d,%d, want zeros", accounts, groups, keys)
	}
}

func TestLoadTransitions(t *testing.T) {
	service := New()
	if err := service.Load(nil); err != nil {
		t.Fatalf("Load(nil) = %v, want nil", err)
	}
	if service.State() != identityproto.StateUnprovisioned {
		t.Fatalf("Load(nil) state = %v, want Unprovisioned", service.State())
	}
	if err := service.Load([]byte("not a snapshot")); err == nil {
		t.Fatal("Load(corrupt) = nil, want error")
	}
	if service.State() != identityproto.StateUnavailable {
		t.Fatalf("Load(corrupt) state = %v, want Unavailable", service.State())
	}
	empty := identity.NewStore()
	data, err := identity.Encode(empty)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Load(data); err != nil {
		t.Fatalf("Load(valid) = %v, want nil", err)
	}
	if service.State() != identityproto.StateAvailable {
		t.Fatalf("Load(valid) state = %v, want Available", service.State())
	}
}

func TestProvisionAndReset(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	if service.State() != identityproto.StateAvailable {
		t.Fatalf("Provision state = %v, want Available", service.State())
	}
	if _, err := service.Snapshot(); err != nil {
		t.Fatalf("Provision Snapshot = %v, want nil", err)
	}

	// Add an account, then reset destructively.
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v, want ResultOK", reply.Result)
	}
	service.Reset()
	if service.State() != identityproto.StateAvailable {
		t.Fatalf("Reset state = %v, want Available", service.State())
	}
	accounts, groups, keys := service.Counts()
	if accounts != 0 || groups != 0 || keys != 0 {
		t.Fatalf("Reset counts = %d,%d,%d, want zeros", accounts, groups, keys)
	}
}

func TestAvailabilityEnforcement(t *testing.T) {
	service := New()
	reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice"))
	if reply.Result != identityproto.ResultUnprovisioned {
		t.Fatalf("Unprovisioned AddAccount = %v, want ResultUnprovisioned", reply.Result)
	}
	if authorized, result := service.AuthorizeKey("alice", key(1)); authorized || result != identityproto.ResultUnprovisioned {
		t.Fatalf("Unprovisioned AuthorizeKey = %v,%v want false,ResultUnprovisioned", authorized, result)
	}
	status := service.HandleRequest(request(identityproto.AdminStatus))
	if status.Result != identityproto.ResultOK || status.State != identityproto.StateUnprovisioned {
		t.Fatalf("Unprovisioned Status = %v/%v, want OK/Unprovisioned", status.Result, status.State)
	}

	// Provision makes data operations work.
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("Available AddAccount = %v, want ResultOK", reply.Result)
	}

	// Corruption makes data operations unavailable but admin endpoints work.
	if err := service.Load([]byte("corrupt")); err == nil {
		t.Fatal("Load(corrupt) = nil, want error")
	}
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "bob")); reply.Result != identityproto.ResultUnavailable {
		t.Fatalf("Unavailable AddAccount = %v, want ResultUnavailable", reply.Result)
	}
	if authorized, result := service.AuthorizeKey("alice", key(1)); authorized || result != identityproto.ResultUnavailable {
		t.Fatalf("Unavailable AuthorizeKey = %v,%v want false,ResultUnavailable", authorized, result)
	}
	status = service.HandleRequest(request(identityproto.AdminStatus))
	if status.Result != identityproto.ResultOK || status.State != identityproto.StateUnavailable {
		t.Fatalf("Unavailable Status = %v/%v, want OK/Unavailable", status.Result, status.State)
	}
	if reply := service.HandleRequest(request(identityproto.AdminProvision)); reply.Result != identityproto.ResultOK {
		t.Fatalf("Unavailable Provision = %v, want ResultOK", reply.Result)
	}
	if service.State() != identityproto.StateAvailable {
		t.Fatalf("Provision after corrupt state = %v, want Available", service.State())
	}
}

func TestErrorMapping(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	dispatch := service.HandleRequest

	if reply := dispatch(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("first AddAccount = %v", reply.Result)
	}
	if reply := dispatch(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultExists {
		t.Fatalf("duplicate AddAccount = %v, want ResultExists", reply.Result)
	}
	if reply := dispatch(named(identityproto.AdminGetAccount, "nobody")); reply.Result != identityproto.ResultNotFound {
		t.Fatalf("GetAccount missing = %v, want ResultNotFound", reply.Result)
	}
	if reply := dispatch(&identityproto.Request{Opcode: identityproto.AdminSetUID, Name: []byte("alice"), UID: identity.ReservedUID}); reply.Result != identityproto.ResultInvalid {
		t.Fatalf("reserved UID = %v, want ResultInvalid", reply.Result)
	}
	if reply := dispatch(&identityproto.Request{Opcode: identityproto.AdminAddKey, Name: []byte("alice"), Key: key(1)}); reply.Result != identityproto.ResultOK {
		t.Fatalf("first AddKey = %v", reply.Result)
	}
	if reply := dispatch(&identityproto.Request{Opcode: identityproto.AdminAddKey, Name: []byte("alice"), Key: key(1)}); reply.Result != identityproto.ResultExists {
		t.Fatalf("duplicate AddKey = %v, want ResultExists", reply.Result)
	}
	if reply := dispatch(&identityproto.Request{Opcode: identityproto.AdminSetEnabled, Name: []byte("alice"), Enabled: false}); reply.Result != identityproto.ResultOK {
		t.Fatalf("SetEnabled = %v", reply.Result)
	}
	if reply := dispatch(named(identityproto.AdminDeleteGroup, "alice")); reply.Result != identityproto.ResultConflict {
		t.Fatalf("DeleteGroup referenced = %v, want ResultConflict", reply.Result)
	}
}

func TestExhaustedMapping(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	last := identityproto.ResultOK
	for i := 0; i < identity.MaxAccounts; i++ {
		name := "user" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		reply := service.HandleRequest(named(identityproto.AdminAddAccount, name))
		last = reply.Result
		if reply.Result != identityproto.ResultOK {
			t.Fatalf("account %d/%s = %v, want OK", i, name, reply.Result)
		}
	}
	reply := service.HandleRequest(named(identityproto.AdminAddAccount, "overflow"))
	if reply.Result != identityproto.ResultExhausted {
		t.Fatalf("overflow account = %v, want ResultExhausted (last=%v)", reply.Result, last)
	}
}

func TestListOrderingAndRecords(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bob", "alice", "carol"} {
		if reply := service.HandleRequest(named(identityproto.AdminAddAccount, name)); reply.Result != identityproto.ResultOK {
			t.Fatalf("AddAccount %s = %v", name, reply.Result)
		}
	}
	reply := service.HandleRequest(request(identityproto.AdminListAccounts))
	if reply.Result != identityproto.ResultOK {
		t.Fatalf("ListAccounts = %v", reply.Result)
	}
	if len(reply.Records) != 3 {
		t.Fatalf("ListAccounts records = %d, want 3", len(reply.Records))
	}
	// Account IDs ascend in creation order: bob(1), alice(2), carol(3).
	want := []string{"bob", "alice", "carol"}
	for i, record := range reply.Records {
		if record.Kind != identityproto.RecordAccount || string(record.Name) != want[i] {
			t.Fatalf("record %d = %v %q, want account %q", i, record.Kind, record.Name, want[i])
		}
	}

	// A group list is ordered by group ID and carries the Unix GID.
	reply = service.HandleRequest(request(identityproto.AdminListGroups))
	if reply.Result != identityproto.ResultOK || len(reply.Records) != 3 {
		t.Fatalf("ListGroups = %v (%d records)", reply.Result, len(reply.Records))
	}
	for i, record := range reply.Records {
		if record.Kind != identityproto.RecordGroup || string(record.Name) != want[i] || record.GID != uint32(1000+i) {
			t.Fatalf("group record %d = %v %q GID %d", i, record.Kind, record.Name, record.GID)
		}
	}
}

func TestGetRecordFields(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", reply.Result)
	}
	reply := service.HandleRequest(named(identityproto.AdminGetAccount, "alice"))
	if reply.Result != identityproto.ResultOK || len(reply.Records) != 1 {
		t.Fatalf("GetAccount = %v (%d records)", reply.Result, len(reply.Records))
	}
	record := reply.Records[0]
	if record.Kind != identityproto.RecordAccount || record.AccountID != 1 || record.GroupID != 1 || record.UID != 1000 || record.GID != 1000 || !record.Enabled {
		t.Fatalf("account record = %+v", record)
	}
	if string(record.Name) != "alice" || string(record.Home) != "" || record.KeyCount != 0 {
		t.Fatalf("account record fields = %q %q %d", record.Name, record.Home, record.KeyCount)
	}

	accounts := service.store.Accounts()
	gid, ok := service.primaryGroupGID(&accounts[0])
	if !ok || gid != 1000 {
		t.Fatalf("primaryGroupGID = %d,%v want 1000,true", gid, ok)
	}
}

func TestAccountRecordPrimaryGID(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", reply.Result)
	}
	if reply := service.HandleRequest(named(identityproto.AdminAddGroup, "wheel")); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddGroup = %v", reply.Result)
	}
	// wheel is the second group: internal ID 2, Unix GID 1001.
	if reply := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminSetGID, Name: []byte("alice"), GID: 1001}); reply.Result != identityproto.ResultOK {
		t.Fatalf("SetGID = %v", reply.Result)
	}

	reply := service.HandleRequest(named(identityproto.AdminGetAccount, "alice"))
	if reply.Result != identityproto.ResultOK || len(reply.Records) != 1 {
		t.Fatalf("GetAccount = %v (%d records)", reply.Result, len(reply.Records))
	}
	record := reply.Records[0]
	// GID carries the primary group's Unix GID (1001), not its internal ID (2).
	if record.GID != 1001 || record.GroupID != 2 {
		t.Fatalf("account record GID/GroupID = %d/%d, want 1001/2", record.GID, record.GroupID)
	}

	// List accounts exposes the same Unix GID.
	list := service.HandleRequest(request(identityproto.AdminListAccounts))
	if list.Result != identityproto.ResultOK || len(list.Records) != 1 || list.Records[0].GID != 1001 {
		t.Fatalf("ListAccounts primary GID = %v/%d", list.Result, list.Records[0].GID)
	}
}

func TestKeyIsolation(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		if reply := service.HandleRequest(named(identityproto.AdminAddAccount, name)); reply.Result != identityproto.ResultOK {
			t.Fatalf("AddAccount %s = %v", name, reply.Result)
		}
	}
	aliceKey := key(7)
	if reply := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminAddKey, Name: []byte("alice"), Key: aliceKey}); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddKey = %v", reply.Result)
	}

	// Alice's key authorizes alice and no one else.
	if authorized, result := service.AuthorizeKey("alice", aliceKey); !authorized || result != identityproto.ResultOK {
		t.Fatalf("AuthorizeKey(alice) = %v,%v want true,OK", authorized, result)
	}
	if authorized, result := service.AuthorizeKey("bob", aliceKey); authorized || result != identityproto.ResultDenied {
		t.Fatalf("AuthorizeKey(bob) = %v,%v want false,Denied", authorized, result)
	}
	if authorized, result := service.AuthorizeKey("nobody", aliceKey); authorized || result != identityproto.ResultNotFound {
		t.Fatalf("AuthorizeKey(nobody) = %v,%v want false,NotFound", authorized, result)
	}

	// A disabled account denies.
	if reply := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminSetEnabled, Name: []byte("alice"), Enabled: false}); reply.Result != identityproto.ResultOK {
		t.Fatalf("SetEnabled = %v", reply.Result)
	}
	if authorized, result := service.AuthorizeKey("alice", aliceKey); authorized || result != identityproto.ResultDenied {
		t.Fatalf("AuthorizeKey(disabled) = %v,%v want false,Denied", authorized, result)
	}

	// A bad key length denies.
	if authorized, result := service.AuthorizeKey("alice", []byte{1}); authorized || result != identityproto.ResultInvalid {
		t.Fatalf("AuthorizeKey(short key) = %v,%v want false,Invalid", authorized, result)
	}
}

func TestDispatchKeyAuthorizationOpcode(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", reply.Result)
	}
	k := key(9)
	if reply := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminAddKey, Name: []byte("alice"), Key: k}); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddKey = %v", reply.Result)
	}
	reply := service.HandleRequest(&identityproto.Request{Opcode: identityproto.LookupAuthorizeKey, Name: []byte("alice"), Key: k})
	if reply.Result != identityproto.ResultOK {
		t.Fatalf("LookupAuthorizeKey = %v, want ResultOK", reply.Result)
	}
	reply = service.HandleRequest(&identityproto.Request{Opcode: identityproto.LookupAuthorizeKey, Name: []byte("alice"), Key: key(10)})
	if reply.Result != identityproto.ResultDenied {
		t.Fatalf("LookupAuthorizeKey wrong key = %v, want ResultDenied", reply.Result)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	service := New()
	if err := service.Provision(); err != nil {
		t.Fatal(err)
	}
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", reply.Result)
	}
	data, err := service.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	if err := restored.Load(data); err != nil {
		t.Fatal(err)
	}
	again, err := restored.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatal("snapshot re-encode differs")
	}
}
