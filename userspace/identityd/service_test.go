package identityd

import (
	"bytes"
	"errors"
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

func memFilesOf(s *Service) *memFiles { return s.persist.Files.(*memFiles) }

func epochSource(e [16]byte) EpochSource {
	return func() ([16]byte, error) { return e, nil }
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustProvision(t *testing.T, s *Service) {
	t.Helper()
	if r := s.HandleRequest(request(identityproto.AdminProvision)); r.Result != identityproto.ResultOK {
		t.Fatalf("Provision = %v, want ResultOK", r.Result)
	}
}

func TestNewUnprovisioned(t *testing.T) {
	service := newTestService()
	if service.State() != identityproto.StateUnprovisioned {
		t.Fatalf("new service state = %v, want Unprovisioned", service.State())
	}
	accounts, groups, keys := service.Counts()
	if accounts != 0 || groups != 0 || keys != 0 {
		t.Fatalf("new service counts = %d,%d,%d, want zeros", accounts, groups, keys)
	}
}

func TestOpenTransitions(t *testing.T) {
	// Missing db + no temp -> Unprovisioned.
	service := newTestService()
	if err := service.Open(); err == nil {
		t.Fatal("Open(missing) = nil, want error")
	}
	if service.State() != identityproto.StateUnprovisioned {
		t.Fatalf("missing state = %v, want Unprovisioned", service.State())
	}

	// Corrupt db -> Unavailable.
	files := newMemFiles()
	files.files[testPath] = []byte("not a snapshot")
	service = New(files, testPath, fixedEpoch)
	if err := service.Open(); err == nil {
		t.Fatal("Open(corrupt) = nil, want error")
	}
	if service.State() != identityproto.StateUnavailable {
		t.Fatalf("corrupt state = %v, want Unavailable", service.State())
	}

	// Valid db -> Available.
	files = newMemFiles()
	files.files[testPath] = snapshotFor(t, "alice")
	service = New(files, testPath, fixedEpoch)
	if err := service.Open(); err != nil {
		t.Fatalf("Open(valid) = %v", err)
	}
	if service.State() != identityproto.StateAvailable {
		t.Fatalf("valid state = %v, want Available", service.State())
	}
}

func TestLifecycleUnprovisioned(t *testing.T) {
	s := newTestService()
	// Status reports the actual state and zero counts.
	r := s.HandleRequest(request(identityproto.AdminStatus))
	if r.Result != identityproto.ResultOK || r.State != identityproto.StateUnprovisioned {
		t.Fatalf("Unprovisioned Status = %v/%v, want OK/Unprovisioned", r.Result, r.State)
	}
	if r.Accounts != 0 || r.Groups != 0 || r.Keys != 0 {
		t.Fatalf("Unprovisioned Status counts = %d,%d,%d, want zeros", r.Accounts, r.Groups, r.Keys)
	}
	// Provision commits an empty DB and becomes Available.
	r = s.HandleRequest(request(identityproto.AdminProvision))
	if r.Result != identityproto.ResultOK || r.State != identityproto.StateAvailable {
		t.Fatalf("Provision = %v/%v, want OK/Available", r.Result, r.State)
	}
}

func TestLifecycleResetFromUnprovisionedIsConflict(t *testing.T) {
	s := newTestService()
	r := s.HandleRequest(request(identityproto.AdminReset))
	if r.Result != identityproto.ResultConflict || r.State != identityproto.StateUnprovisioned {
		t.Fatalf("Unprovisioned Reset = %v/%v, want Conflict/Unprovisioned", r.Result, r.State)
	}
	if len(memFilesOf(s).files) != 0 {
		t.Fatal("Unprovisioned Reset wrote files")
	}
}

func TestLifecycleAvailable(t *testing.T) {
	s := newTestService()
	mustProvision(t, s)
	if r := s.HandleRequest(named(identityproto.AdminAddAccount, "alice")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", r.Result)
	}
	// Status reports counts when available.
	r := s.HandleRequest(request(identityproto.AdminStatus))
	if r.Result != identityproto.ResultOK || r.State != identityproto.StateAvailable {
		t.Fatalf("Available Status = %v/%v", r.Result, r.State)
	}
	if r.Accounts != 1 || r.Groups != 1 || r.Keys != 0 {
		t.Fatalf("Available Status counts = %d,%d,%d, want 1,1,0", r.Accounts, r.Groups, r.Keys)
	}
	// Provision over a valid store is a Conflict and leaves it unchanged.
	before := s.store.Sequence()
	r = s.HandleRequest(request(identityproto.AdminProvision))
	if r.Result != identityproto.ResultConflict || r.State != identityproto.StateAvailable {
		t.Fatalf("Available Provision = %v/%v, want Conflict/Available", r.Result, r.State)
	}
	if s.store.Sequence() != before {
		t.Fatal("Provision over valid store changed the committed store")
	}
	// Reset from Available is a valid reset: empty, still Available.
	r = s.HandleRequest(request(identityproto.AdminReset))
	if r.Result != identityproto.ResultOK || r.State != identityproto.StateAvailable {
		t.Fatalf("Available Reset = %v/%v, want OK/Available", r.Result, r.State)
	}
	if accounts, groups, keys := s.Counts(); accounts != 0 || groups != 0 || keys != 0 {
		t.Fatalf("Reset counts = %d,%d,%d, want zeros", accounts, groups, keys)
	}
}

func TestLifecycleUnavailable(t *testing.T) {
	files := newMemFiles()
	files.files[testPath] = []byte("corrupt")
	s := New(files, testPath, fixedEpoch)
	if err := s.Open(); err == nil {
		t.Fatal("Open(corrupt) = nil")
	}
	if s.State() != identityproto.StateUnavailable {
		t.Fatalf("corrupt state = %v", s.State())
	}
	// Status reports Unavailable.
	r := s.HandleRequest(request(identityproto.AdminStatus))
	if r.Result != identityproto.ResultOK || r.State != identityproto.StateUnavailable {
		t.Fatalf("Unavailable Status = %v/%v, want OK/Unavailable", r.Result, r.State)
	}
	// Provision over corrupt data is denied and unchanged.
	r = s.HandleRequest(request(identityproto.AdminProvision))
	if r.Result != identityproto.ResultUnavailable || r.State != identityproto.StateUnavailable {
		t.Fatalf("Unavailable Provision = %v/%v, want Unavailable/Unavailable", r.Result, r.State)
	}
	// Reset is deliberate recovery: fresh epoch, Available.
	r = s.HandleRequest(request(identityproto.AdminReset))
	if r.Result != identityproto.ResultOK || r.State != identityproto.StateAvailable {
		t.Fatalf("Unavailable Reset = %v/%v, want OK/Available", r.Result, r.State)
	}
}

func TestNoImplicitProvision(t *testing.T) {
	s := newTestService()
	for _, op := range []identityproto.Opcode{identityproto.AdminAddAccount, identityproto.AdminAddGroup} {
		r := s.HandleRequest(named(op, "x"))
		if r.Result != identityproto.ResultUnprovisioned {
			t.Fatalf("Unprovisioned %v = %v, want ResultUnprovisioned", op, r.Result)
		}
	}
	if s.State() != identityproto.StateUnprovisioned {
		t.Fatalf("state = %v, want Unprovisioned", s.State())
	}
	if len(memFilesOf(s).files) != 0 {
		t.Fatal("add implicitly provisioned (wrote files)")
	}
}

func TestResetPreservesEpochAndCounters(t *testing.T) {
	var epochA = [16]byte{0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf}
	s := New(newMemFiles(), testPath, epochSource(epochA))
	mustProvision(t, s)
	if r := s.HandleRequest(named(identityproto.AdminAddAccount, "alice")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", r.Result)
	}
	if s.store.Epoch() != epochA {
		t.Fatalf("provision epoch = %x, want epochA", s.store.Epoch())
	}
	// A valid reset must preserve the epoch without generating a new one.
	s.epoch = func() ([16]byte, error) {
		t.Fatal("epoch source called during valid reset")
		return [16]byte{}, nil
	}
	if r := s.HandleRequest(request(identityproto.AdminReset)); r.Result != identityproto.ResultOK {
		t.Fatalf("Reset = %v", r.Result)
	}
	if s.store.Epoch() != epochA {
		t.Fatalf("reset epoch = %x, want epochA preserved", s.store.Epoch())
	}
	// High-water marks survive: a new account does not reuse UID or AccountID.
	if r := s.HandleRequest(named(identityproto.AdminAddAccount, "bob")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount bob = %v", r.Result)
	}
	bob, err := s.store.AccountByName("bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.UID != 1001 || bob.ID != 2 {
		t.Fatalf("post-reset account UID/ID = %d/%d, want 1001/2 (high-water preserved)", bob.UID, bob.ID)
	}
}

func TestResetNewEpochOnCorruptRecovery(t *testing.T) {
	var epochB = [16]byte{0xb0, 0xb1, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xbb, 0xbc, 0xbd, 0xbe, 0xbf}
	files := newMemFiles()
	files.files[testPath] = []byte("corrupt")
	s := New(files, testPath, epochSource(epochB))
	if err := s.Open(); err == nil {
		t.Fatal("Open(corrupt) = nil")
	}
	if r := s.HandleRequest(request(identityproto.AdminReset)); r.Result != identityproto.ResultOK {
		t.Fatalf("Reset = %v", r.Result)
	}
	if s.store.Epoch() != epochB {
		t.Fatalf("recovery epoch = %x, want epochB", s.store.Epoch())
	}
	// Recovery starts counters fresh.
	if r := s.HandleRequest(named(identityproto.AdminAddAccount, "bob")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", r.Result)
	}
	bob, err := s.store.AccountByName("bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.UID != 1000 || bob.ID != 1 {
		t.Fatalf("recovered account UID/ID = %d/%d, want 1000/1 (fresh counters)", bob.UID, bob.ID)
	}
}

func TestCandidateCommitNotPublishedOnFailure(t *testing.T) {
	s := newTestService()
	mustProvision(t, s)
	if r := s.HandleRequest(named(identityproto.AdminAddAccount, "alice")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount alice = %v", r.Result)
	}

	// Fail the next save before the replace: a definite pre-replace failure.
	s.persist.Faults.BeforeReplace = func() error { return errors.New("fail") }
	r := s.HandleRequest(named(identityproto.AdminAddAccount, "bob"))
	if r.Result != identityproto.ResultUnavailable || r.State != identityproto.StateAvailable {
		t.Fatalf("failed mutate = %v/%v, want Unavailable/Available", r.Result, r.State)
	}
	if accounts, _, _ := s.Counts(); accounts != 1 {
		t.Fatalf("accounts after failed mutate = %d, want 1", accounts)
	}
	if _, err := s.store.AccountByName("bob"); err == nil {
		t.Fatal("bob published despite failed persist")
	}

	// The same mutation commits once persistence succeeds again.
	s.persist.Faults.BeforeReplace = nil
	r = s.HandleRequest(named(identityproto.AdminAddAccount, "bob"))
	if r.Result != identityproto.ResultOK {
		t.Fatalf("retry AddAccount bob = %v", r.Result)
	}
	if accounts, _, _ := s.Counts(); accounts != 2 {
		t.Fatalf("accounts after retry = %d, want 2", accounts)
	}
}

func TestUncertainReplaceFailsClosed(t *testing.T) {
	s := newTestService()
	mustProvision(t, s)
	if r := s.HandleRequest(named(identityproto.AdminAddAccount, "alice")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", r.Result)
	}
	s.persist.Faults.ReplaceErr = func() error { return ErrUncertain }
	r := s.HandleRequest(named(identityproto.AdminAddAccount, "bob"))
	if r.Result != identityproto.ResultUnavailable || r.State != identityproto.StateUnavailable {
		t.Fatalf("uncertain mutate = %v/%v, want Unavailable/Unavailable", r.Result, r.State)
	}
	if s.State() != identityproto.StateUnavailable {
		t.Fatalf("state = %v, want Unavailable", s.State())
	}
	if accounts, _, _ := s.Counts(); accounts != 0 {
		t.Fatalf("counts after uncertain = %d, want 0", accounts)
	}
	if ok, res := s.AuthorizeKey("alice", key(1)); ok || res != identityproto.ResultUnavailable {
		t.Fatalf("AuthorizeKey after uncertain = %v,%v, want false,Unavailable", ok, res)
	}
}

func TestAfterReplaceReconcileOnRestart(t *testing.T) {
	files := newMemFiles()
	s := New(files, testPath, fixedEpoch)
	mustProvision(t, s)
	if r := s.HandleRequest(named(identityproto.AdminAddAccount, "alice")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount alice = %v", r.Result)
	}
	// The replace commits but the reply is lost: fail closed.
	s.persist.Faults.AfterReplace = func() error { return errors.New("lost reply") }
	r := s.HandleRequest(named(identityproto.AdminAddAccount, "bob"))
	if r.Result != identityproto.ResultUnavailable || r.State != identityproto.StateUnavailable {
		t.Fatalf("lost-reply mutate = %v/%v, want Unavailable/Unavailable", r.Result, r.State)
	}

	// Restart loads the durable snapshot: the committed mutation is present,
	// and never replayed.
	s2 := New(files, testPath, fixedEpoch)
	if err := s2.Open(); err != nil {
		t.Fatalf("restart Open = %v", err)
	}
	if s2.State() != identityproto.StateAvailable {
		t.Fatalf("restart state = %v, want Available", s2.State())
	}
	if accounts, _, _ := s2.Counts(); accounts != 2 {
		t.Fatalf("restart accounts = %d, want 2 (alice+bob, not replayed)", accounts)
	}
	if _, err := s2.store.AccountByName("bob"); err != nil {
		t.Fatal("bob missing after restart reconcile")
	}
}

func TestLookupRecordCarriesEpochRevisionGroups(t *testing.T) {
	s := newTestService()
	mustProvision(t, s)
	for _, op := range []struct {
		opcode identityproto.Opcode
		name   string
	}{
		{identityproto.AdminAddAccount, "alice"},
		{identityproto.AdminAddGroup, "wheel"},
		{identityproto.AdminAddGroup, "staff"},
	} {
		if r := s.HandleRequest(named(op.opcode, op.name)); r.Result != identityproto.ResultOK {
			t.Fatalf("%v %s = %v", op.opcode, op.name, r.Result)
		}
	}
	// wheel GID 1001, staff GID 1002; supply them unsorted to prove sorting.
	if r := s.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminSetGroups, Name: []byte("alice"), IDs: []uint32{1002, 1001}}); r.Result != identityproto.ResultOK {
		t.Fatalf("SetGroups = %v", r.Result)
	}

	for _, op := range []identityproto.Opcode{identityproto.AdminGetAccount, identityproto.LookupGetAccount} {
		r := s.HandleRequest(named(op, "alice"))
		if r.Result != identityproto.ResultOK || len(r.Records) != 1 {
			t.Fatalf("%v = %v (%d records)", op, r.Result, len(r.Records))
		}
		record := r.Records[0]
		if record.Kind != identityproto.RecordAccount || record.AccountID != 1 || record.UID != 1000 || record.GID != 1000 {
			t.Fatalf("%v record = %+v", op, record)
		}
		if record.Epoch != testEpoch {
			t.Fatalf("%v record epoch = %x, want %x", op, record.Epoch, testEpoch)
		}
		if record.Revision != s.store.Sequence() {
			t.Fatalf("%v record revision = %d, want %d", op, record.Revision, s.store.Sequence())
		}
		if !equalU32(record.Groups, []uint32{1001, 1002}) {
			t.Fatalf("%v record groups = %v, want [1001 1002] sorted", op, record.Groups)
		}
	}
}

func TestRevisionAdvancesAfterMutation(t *testing.T) {
	s := newTestService()
	mustProvision(t, s)
	if r := s.HandleRequest(named(identityproto.AdminAddAccount, "alice")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", r.Result)
	}
	before := s.store.Sequence()
	if r := s.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminSetHome, Name: []byte("alice"), Home: []byte("/home/alice")}); r.Result != identityproto.ResultOK {
		t.Fatalf("SetHome = %v", r.Result)
	}
	r := s.HandleRequest(named(identityproto.LookupGetAccount, "alice"))
	if r.Result != identityproto.ResultOK || len(r.Records) != 1 {
		t.Fatalf("GetAccount = %v", r.Result)
	}
	if r.Records[0].Revision != before+1 {
		t.Fatalf("revision = %d, want %d", r.Records[0].Revision, before+1)
	}
	if string(r.Records[0].Home) != "/home/alice" {
		t.Fatalf("home = %q", r.Records[0].Home)
	}
}

func TestAvailabilityEnforcement(t *testing.T) {
	service := newTestService()
	reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice"))
	if reply.Result != identityproto.ResultUnprovisioned {
		t.Fatalf("Unprovisioned AddAccount = %v, want ResultUnprovisioned", reply.Result)
	}
	if authorized, result := service.AuthorizeKey("alice", key(1)); authorized || result != identityproto.ResultUnprovisioned {
		t.Fatalf("Unprovisioned AuthorizeKey = %v,%v want false,ResultUnprovisioned", authorized, result)
	}

	mustProvision(t, service)
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("Available AddAccount = %v, want ResultOK", reply.Result)
	}

	// Corruption makes data operations unavailable.
	files := newMemFiles()
	files.files[testPath] = []byte("corrupt")
	unavailable := New(files, testPath, fixedEpoch)
	if err := unavailable.Open(); err == nil {
		t.Fatal("Open(corrupt) = nil")
	}
	if reply := unavailable.HandleRequest(named(identityproto.AdminAddAccount, "bob")); reply.Result != identityproto.ResultUnavailable {
		t.Fatalf("Unavailable AddAccount = %v, want ResultUnavailable", reply.Result)
	}
	if authorized, result := unavailable.AuthorizeKey("alice", key(1)); authorized || result != identityproto.ResultUnavailable {
		t.Fatalf("Unavailable AuthorizeKey = %v,%v want false,ResultUnavailable", authorized, result)
	}
}

func TestErrorMapping(t *testing.T) {
	service := newTestService()
	mustProvision(t, service)
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
	service := newTestService()
	mustProvision(t, service)
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
	service := newTestService()
	mustProvision(t, service)
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
	want := []string{"bob", "alice", "carol"}
	for i, record := range reply.Records {
		if record.Kind != identityproto.RecordAccount || string(record.Name) != want[i] {
			t.Fatalf("record %d = %v %q, want account %q", i, record.Kind, record.Name, want[i])
		}
	}
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

func TestKeyIsolation(t *testing.T) {
	service := newTestService()
	mustProvision(t, service)
	for _, name := range []string{"alice", "bob"} {
		if reply := service.HandleRequest(named(identityproto.AdminAddAccount, name)); reply.Result != identityproto.ResultOK {
			t.Fatalf("AddAccount %s = %v", name, reply.Result)
		}
	}
	aliceKey := key(7)
	if reply := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminAddKey, Name: []byte("alice"), Key: aliceKey}); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddKey = %v", reply.Result)
	}
	if authorized, result := service.AuthorizeKey("alice", aliceKey); !authorized || result != identityproto.ResultOK {
		t.Fatalf("AuthorizeKey(alice) = %v,%v want true,OK", authorized, result)
	}
	if authorized, result := service.AuthorizeKey("bob", aliceKey); authorized || result != identityproto.ResultDenied {
		t.Fatalf("AuthorizeKey(bob) = %v,%v want false,Denied", authorized, result)
	}
	if authorized, result := service.AuthorizeKey("nobody", aliceKey); authorized || result != identityproto.ResultNotFound {
		t.Fatalf("AuthorizeKey(nobody) = %v,%v want false,NotFound", authorized, result)
	}
	if reply := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminSetEnabled, Name: []byte("alice"), Enabled: false}); reply.Result != identityproto.ResultOK {
		t.Fatalf("SetEnabled = %v", reply.Result)
	}
	if authorized, result := service.AuthorizeKey("alice", aliceKey); authorized || result != identityproto.ResultDenied {
		t.Fatalf("AuthorizeKey(disabled) = %v,%v want false,Denied", authorized, result)
	}
	if authorized, result := service.AuthorizeKey("alice", []byte{1}); authorized || result != identityproto.ResultInvalid {
		t.Fatalf("AuthorizeKey(short key) = %v,%v want false,Invalid", authorized, result)
	}
}

func TestDispatchKeyAuthorizationOpcode(t *testing.T) {
	service := newTestService()
	mustProvision(t, service)
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
	service := newTestService()
	mustProvision(t, service)
	if reply := service.HandleRequest(named(identityproto.AdminAddAccount, "alice")); reply.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", reply.Result)
	}
	data, err := service.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := identity.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	again, err := identity.Encode(restored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatal("snapshot re-encode differs")
	}
}

func TestDispatchImportLegacy(t *testing.T) {
	service := newTestService()
	if r := service.HandleRequest(request(identityproto.AdminStatus)); r.State != identityproto.StateUnprovisioned {
		t.Fatalf("state %v, want Unprovisioned", r.State)
	}
	mustProvision(t, service)
	keys := identityproto.EncodeImportKeys([][]byte{key(1), key(2)})
	importReq := &identityproto.Request{Opcode: identityproto.AdminImportLegacy, Name: []byte("remote-admin"), Home: []byte("/home/user"), Key: keys}
	if r := service.HandleRequest(importReq); r.Result != identityproto.ResultOK {
		t.Fatalf("import = %v, want ResultOK", r.Result)
	}
	if r := service.HandleRequest(importReq); r.Result != identityproto.ResultExists {
		t.Fatalf("second import = %v, want ResultExists", r.Result)
	}
	accounts, _, keysCount := service.Counts()
	if accounts != 1 || keysCount != 2 {
		t.Fatalf("counts accounts=%d keys=%d, want 1 and 2", accounts, keysCount)
	}
}

func TestDispatchImportLegacyRejectsBadKeys(t *testing.T) {
	service := newTestService()
	mustProvision(t, service)
	r := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminImportLegacy, Name: []byte("remote-admin"), Home: []byte("/home/user"), Key: []byte{3, 0}})
	if r.Result != identityproto.ResultInvalid {
		t.Fatalf("malformed keys = %v, want ResultInvalid", r.Result)
	}
}

func TestDispatchLookupAuthorizeAndResolve(t *testing.T) {
	service := newTestService()
	mustProvision(t, service)
	if r := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminAddAccount, Name: []byte("alice")}); r.Result != identityproto.ResultOK {
		t.Fatalf("add = %v", r.Result)
	}
	if r := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminSetHome, Name: []byte("alice"), Home: []byte("/home/alice")}); r.Result != identityproto.ResultOK {
		t.Fatalf("set home = %v", r.Result)
	}
	if r := service.HandleRequest(&identityproto.Request{Opcode: identityproto.AdminAddKey, Name: []byte("alice"), Key: key(7)}); r.Result != identityproto.ResultOK {
		t.Fatalf("add key = %v", r.Result)
	}
	r := service.HandleRequest(&identityproto.Request{Opcode: identityproto.LookupAuthorizeAndResolve, Name: []byte("alice"), Key: key(7)})
	if r.Result != identityproto.ResultOK || len(r.Records) != 1 {
		t.Fatalf("resolve = %v records=%d, want OK and one record", r.Result, len(r.Records))
	}
	record := r.Records[0]
	if record.Kind != identityproto.RecordAccount || record.AccountID == 0 || record.UID == 0 || record.GID == 0 ||
		len(record.Name) == 0 || len(record.Home) == 0 || record.Epoch == ([16]byte{}) {
		t.Fatalf("record = %+v", record)
	}
	// A wrong key denies and resolves nothing.
	denied := service.HandleRequest(&identityproto.Request{Opcode: identityproto.LookupAuthorizeAndResolve, Name: []byte("alice"), Key: key(9)})
	if denied.Result == identityproto.ResultOK || len(denied.Records) != 0 {
		t.Fatalf("wrong key = %v records=%d, want denial", denied.Result, len(denied.Records))
	}
}
