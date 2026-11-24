package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"
)

func keyFromByte(b byte) Key {
	var k Key
	for i := range k {
		k[i] = b + byte(i)
	}
	return k
}

func epochFromByte(b byte) [16]byte {
	var e [16]byte
	e[0] = b
	return e
}

func testAccount(id uint64, uid uint32, name string, primary uint64) Account {
	a := Account{ID: id, UID: uid, PrimaryGID: primary, Enabled: true}
	a.NameLen = uint8(len(name))
	copy(a.Name[:], name)
	return a
}

func testGroup(id uint64, gid uint32, name string) Group {
	g := Group{ID: id, GID: gid}
	g.NameLen = uint8(len(name))
	copy(g.Name[:], name)
	return g
}

func mustAccount(t *testing.T, s *Store, name string) *Account {
	t.Helper()
	a, err := s.CreateAccount(name)
	if err != nil {
		t.Fatalf("CreateAccount(%q): %v", name, err)
	}
	return a
}

// buildStore returns a store with two accounts, a supplementary group, keys,
// and a home set, exercising the whole surface.
func buildStore(t *testing.T) *Store {
	t.Helper()
	s := NewStoreWithEpoch(epochFromByte(0x01))
	mustAccount(t, s, "alice")
	mustAccount(t, s, "bob")
	if err := s.SetHome("alice", "/home/alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHome("bob", "/home/bob"); err != nil {
		t.Fatal(err)
	}
	wheel, err := s.CreateGroup("wheel")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroups("alice", []uint32{wheel.GID}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey("alice", keyFromByte(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey("alice", keyFromByte(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey("bob", keyFromByte(3)); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotRoundTripByteIdentical(t *testing.T) {
	s := buildStore(t)
	data, err := Encode(s)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(data) > MaxSnapshotBytes {
		t.Fatalf("snapshot %d exceeds cap %d", len(data), MaxSnapshotBytes)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	again, err := Encode(decoded)
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if !bytes.Equal(data, again) {
		t.Fatal("round-trip is not byte-identical")
	}
	if decoded.Sequence() != s.Sequence() {
		t.Fatalf("sequence %d != %d", decoded.Sequence(), s.Sequence())
	}
	if decoded.Epoch() != s.Epoch() {
		t.Fatalf("epoch %x != %x", decoded.Epoch(), s.Epoch())
	}
}

func TestEmptySnapshotRoundTrip(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x02))
	data, err := Encode(s)
	if err != nil {
		t.Fatalf("Encode empty: %v", err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode empty: %v", err)
	}
	if err := Validate(data); err != nil {
		t.Fatalf("Validate empty: %v", err)
	}
	if len(decoded.Accounts()) != 0 || len(decoded.Groups()) != 0 {
		t.Fatal("empty store decoded non-empty")
	}
}

func TestDecodeRejectsTruncation(t *testing.T) {
	s := buildStore(t)
	data, _ := Encode(s)
	for n := 0; n < len(data); n++ {
		if _, err := Decode(data[:n]); err == nil {
			t.Fatalf("truncation at %d bytes decoded", n)
		}
	}
}

func TestDecodeRejectsFlippedFields(t *testing.T) {
	s := buildStore(t)
	data, _ := Encode(s)
	cases := []struct {
		name string
		off  int
	}{
		{"magic[0]", 0},
		{"magic[5]", 5},
		{"magic[16]", 16},
		{"version[0]", 17},
		{"version[1]", 18},
		{"sequence[0]", 19},
		{"sequence[4]", 23},
		{"sequence[7]", 26},
		{"digest[0]", len(data) - 32},
		{"digest[31]", len(data) - 1},
		{"epoch[0]", snapshotHeaderSz},
		{"epoch[15]", snapshotHeaderSz + 15},
		{"payload", snapshotHeaderSz + 3},
	}
	for _, c := range cases {
		mut := append([]byte(nil), data...)
		mut[c.off] ^= 0xFF
		if _, err := Decode(mut); err == nil {
			t.Fatalf("flip %s decoded", c.name)
		}
	}
}

func TestDecodeRejectsOversized(t *testing.T) {
	oversized := bytes.Repeat([]byte{0xAA}, MaxSnapshotBytes+1)
	if _, err := Decode(oversized); err == nil {
		t.Fatal("oversized snapshot decoded")
	}
	if err := Validate(oversized); err == nil {
		t.Fatal("oversized snapshot validated")
	}
}

// hostilePayload encodes a deliberately invalid Store (valid digest, invalid
// payload) and asserts Decode rejects it.
func hostilePayload(t *testing.T, s *Store) []byte {
	t.Helper()
	data, err := Encode(s)
	if err != nil {
		t.Fatalf("Encode hostile fixture: %v", err)
	}
	return data
}

// hostileStore returns a structurally valid store carrying a non-zero epoch and
// high-water marks far above any fixture UID/GID, so Decode reaches the specific
// invariant under test instead of failing on the epoch/high-water check.
func hostileStore(accounts []Account, groups []Group, nextAccountID, nextGroupID uint64) *Store {
	return &Store{
		epoch:         epochFromByte(0x01),
		accounts:      accounts,
		groups:        groups,
		nextAccountID: nextAccountID,
		nextGroupID:   nextGroupID,
		nextUID:       0xFFFFFFFF,
		nextGID:       0xFFFFFFFF,
	}
}

func TestDecodeRejectsDuplicateAccountNames(t *testing.T) {
	s := hostileStore(
		[]Account{testAccount(1, 1000, "dup", 1), testAccount(2, 1001, "dup", 2)},
		[]Group{testGroup(1, 1000, "dup"), testGroup(2, 1001, "dup2")},
		3, 3)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("duplicate account names decoded")
	}
}

func TestDecodeRejectsDuplicateGroupNames(t *testing.T) {
	s := hostileStore(
		[]Account{testAccount(1, 1000, "a", 1)},
		[]Group{testGroup(1, 1000, "dup"), testGroup(2, 1001, "dup")},
		2, 3)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("duplicate group names decoded")
	}
}

func TestDecodeRejectsDuplicateAccountIDs(t *testing.T) {
	s := hostileStore(
		[]Account{testAccount(1, 1000, "a", 1), testAccount(1, 1001, "b", 2)},
		[]Group{testGroup(1, 1000, "a"), testGroup(2, 1001, "b")},
		2, 3)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("duplicate account IDs decoded")
	}
}

func TestDecodeRejectsDuplicateGroupIDs(t *testing.T) {
	s := hostileStore(
		[]Account{testAccount(1, 1000, "a", 1)},
		[]Group{testGroup(1, 1000, "g1"), testGroup(1, 1001, "g2")},
		2, 2)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("duplicate group IDs decoded")
	}
}

func TestDecodeRejectsDuplicateUIDs(t *testing.T) {
	s := hostileStore(
		[]Account{testAccount(1, 1000, "a", 1), testAccount(2, 1000, "b", 2)},
		[]Group{testGroup(1, 1000, "a"), testGroup(2, 1001, "b")},
		3, 3)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("duplicate UIDs decoded")
	}
}

func TestDecodeRejectsDuplicateGIDs(t *testing.T) {
	s := hostileStore(
		[]Account{testAccount(1, 1000, "a", 1)},
		[]Group{testGroup(1, 1000, "g1"), testGroup(2, 1000, "g2")},
		2, 3)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("duplicate GIDs decoded")
	}
}

func TestDecodeRejectsDanglingGroupRef(t *testing.T) {
	s := hostileStore(
		[]Account{testAccount(1, 1000, "a", 999)},
		[]Group{testGroup(1, 1000, "a")},
		2, 2)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("dangling primary group decoded")
	}
}

func TestDecodeRejectsOutOfRangeUIDGID(t *testing.T) {
	s := hostileStore(
		[]Account{testAccount(1, 0, "a", 1)},
		[]Group{testGroup(1, 1000, "a")},
		2, 2)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("reserved UID decoded")
	}
	s = hostileStore(
		[]Account{testAccount(1, 1000, "a", 1)},
		[]Group{testGroup(1, 0xFFFFFFFF, "a")},
		2, 2)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("reserved GID decoded")
	}
}

func TestDecodeRejectsDuplicateKey(t *testing.T) {
	a := testAccount(1, 1000, "a", 1)
	a.Keys[0] = keyFromByte(9)
	a.Keys[1] = keyFromByte(9)
	a.KeyCount = 2
	s := hostileStore([]Account{a}, []Group{testGroup(1, 1000, "a")}, 2, 2)
	if _, err := Decode(hostilePayload(t, s)); err == nil {
		t.Fatal("duplicate key decoded")
	}
}

func TestCreateUpdateDeleteRecreate(t *testing.T) {
	s := NewStore()
	a := mustAccount(t, s, "alice")
	if !a.Enabled {
		t.Fatal("new account not enabled")
	}
	if a.UID < AllocBase || a.UID == 0 || a.UID == 0xFFFFFFFF {
		t.Fatalf("bad UID %d", a.UID)
	}
	if err := s.SetHome("alice", "/home/alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled("alice", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled("alice", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUID("alice", 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey("alice", keyFromByte(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveKey("alice", Fingerprint(keyFromByte(1))); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccountByName("alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted account still resolvable: %v", err)
	}
	// Recreate with the same name.
	if _, err := s.CreateAccount("alice"); err != nil {
		t.Fatalf("recreate account: %v", err)
	}

	g, err := s.CreateGroup("wheel")
	if err != nil {
		t.Fatal(err)
	}
	if g.GID < AllocBase || g.GID == 0 || g.GID == 0xFFFFFFFF {
		t.Fatalf("bad GID %d", g.GID)
	}
	if err := s.DeleteGroup("wheel"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GroupByName("wheel"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted group still resolvable: %v", err)
	}
	if _, err := s.CreateGroup("wheel"); err != nil {
		t.Fatalf("recreate group: %v", err)
	}
}

func TestGenerationSafety(t *testing.T) {
	s := NewStore()
	a1 := mustAccount(t, s, "alice")
	oldAccountID := a1.ID
	oldGroupID := s.Groups()[0].ID
	if err := s.DeleteAccount("alice"); err != nil {
		t.Fatal(err)
	}
	a2 := mustAccount(t, s, "alice")
	if a2.ID == oldAccountID {
		t.Fatal("internal AccountID reused after delete")
	}
	newGroupID := s.Groups()[0].ID
	if newGroupID == oldGroupID {
		t.Fatal("internal GroupID reused after delete")
	}
	for _, a := range s.Accounts() {
		if a.ID == oldAccountID {
			t.Fatal("old AccountID aliases a live account")
		}
	}
}

func TestUIDGIDNoAutoReuse(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	a1 := mustAccount(t, s, "alice")
	uid1, gid1 := a1.UID, s.Groups()[0].GID
	if err := s.DeleteAccount("alice"); err != nil {
		t.Fatal(err)
	}
	a2 := mustAccount(t, s, "alice")
	if a2.UID == uid1 {
		t.Fatalf("UID auto-reused: %d", uid1)
	}
	if a2.UID <= uid1 {
		t.Fatalf("UID did not increase: %d <= %d", a2.UID, uid1)
	}
	if g := s.Groups()[0].GID; g == gid1 {
		t.Fatalf("GID auto-reused: %d", gid1)
	} else if g <= gid1 {
		t.Fatalf("GID did not increase: %d <= %d", g, gid1)
	}
	if a2.ID == a1.ID {
		t.Fatal("internal AccountID reused")
	}
}

func TestExplicitUIDReuseAfterDelete(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	a1 := mustAccount(t, s, "alice")
	uid1 := a1.UID
	if err := s.DeleteAccount("alice"); err != nil {
		t.Fatal(err)
	}
	a2 := mustAccount(t, s, "alice")
	if a2.UID == uid1 {
		t.Fatalf("UID auto-reused before explicit reassignment: %d", uid1)
	}
	// The old UID is free and non-reserved: explicit assignment may reuse it.
	if err := s.SetUID("alice", uid1); err != nil {
		t.Fatalf("explicit reuse of free UID %d: %v", uid1, err)
	}
	if got, _ := s.AccountByName("alice"); got.UID != uid1 {
		t.Fatalf("explicit UID = %d, want %d", got.UID, uid1)
	}
	// Automatic allocation continues above the reused UID.
	a3 := mustAccount(t, s, "bob")
	if a3.UID == uid1 {
		t.Fatalf("automatic allocation collided with explicitly reused UID %d", uid1)
	}
}

func TestExplicitAssignmentAdvancesHighWater(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	mustAccount(t, s, "alice") // UID 1000
	if err := s.SetUID("alice", 5000); err != nil {
		t.Fatal(err)
	}
	a2 := mustAccount(t, s, "bob")
	if a2.UID <= 5000 {
		t.Fatalf("auto UID %d did not exceed explicit high-water 5000", a2.UID)
	}
	// A lower explicit reassignment must not lower the high-water mark.
	if err := s.SetUID("bob", 4000); err != nil {
		t.Fatal(err)
	}
	a3 := mustAccount(t, s, "carol")
	if a3.UID <= 5000 {
		t.Fatalf("auto UID %d fell below explicit high-water 5000", a3.UID)
	}
}

func TestHighWaterPersistsAcrossSaveReload(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	a1 := mustAccount(t, s, "alice")
	uid1, gid1 := a1.UID, s.Groups()[0].GID
	if err := s.DeleteAccount("alice"); err != nil {
		t.Fatal(err)
	}
	data, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	a2 := mustAccount(t, restarted, "alice")
	if a2.UID == uid1 {
		t.Fatalf("UID reused after save/reload: %d", uid1)
	}
	if g := restarted.Groups()[0].GID; g == gid1 {
		t.Fatalf("GID reused after save/reload: %d", gid1)
	}
	if a2.ID == a1.ID {
		t.Fatal("AccountID reused after save/reload")
	}
}

func TestAllocationStartsAtAllocBaseAndSkipsReserved(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	a := mustAccount(t, s, "alice")
	if a.UID != AllocBase {
		t.Fatalf("first UID = %d, want AllocBase %d", a.UID, AllocBase)
	}
	g := s.Groups()[0]
	if g.GID != AllocBase {
		t.Fatalf("first GID = %d, want AllocBase %d", g.GID, AllocBase)
	}
	if reservedUnix(a.UID) || reservedUnix(g.GID) {
		t.Fatal("allocated a reserved UID/GID")
	}
}

func TestUIDGIDNearExhaustion(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	s.nextUID = uint64(maxAllocatableUnix)
	a := mustAccount(t, s, "alice")
	if a.UID != maxAllocatableUnix {
		t.Fatalf("UID = %d, want %d", a.UID, maxAllocatableUnix)
	}
	if _, err := s.CreateAccount("bob"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("UID exhaustion = %v, want ErrExhausted", err)
	}
	if len(s.Accounts()) != 1 {
		t.Fatalf("accounts after failed allocation = %d, want 1", len(s.Accounts()))
	}
}

func TestUIDGIDNoWrap(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	s.nextUID = maxUint64
	if _, err := s.CreateAccount("a"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("UID wrap = %v, want ErrExhausted", err)
	}
	s = NewStoreWithEpoch(epochFromByte(0x01))
	s.nextGID = maxUint64
	if _, err := s.CreateAccount("a"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("GID wrap = %v, want ErrExhausted", err)
	}
}

func TestReferencedGroupDeletionConflict(t *testing.T) {
	s := NewStore()
	mustAccount(t, s, "alice")
	// Primary group is referenced; deletion must conflict.
	if err := s.DeleteGroup("alice"); !errors.Is(err, ErrConflict) {
		t.Fatalf("primary group deletion = %v, want conflict", err)
	}
	wheel, err := s.CreateGroup("wheel")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroups("alice", []uint32{wheel.GID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGroup("wheel"); !errors.Is(err, ErrConflict) {
		t.Fatalf("supplementary group deletion = %v, want conflict", err)
	}
	// Remove the reference, then deletion succeeds.
	if err := s.SetGroups("alice", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGroup("wheel"); err != nil {
		t.Fatalf("unreferenced group deletion: %v", err)
	}
}

func TestKeyIsolation(t *testing.T) {
	s := NewStore()
	mustAccount(t, s, "alice")
	mustAccount(t, s, "bob")
	k := keyFromByte(7)
	if err := s.AddKey("alice", k); err != nil {
		t.Fatal(err)
	}
	ok, err := s.AuthorizeKey("alice", k)
	if err != nil || !ok {
		t.Fatalf("alice should authorize: ok=%v err=%v", ok, err)
	}
	ok, err = s.AuthorizeKey("bob", k)
	if err != nil || ok {
		t.Fatalf("bob must not authorize alice's key: ok=%v err=%v", ok, err)
	}
}

func TestDisabledAccountDenies(t *testing.T) {
	s := NewStore()
	mustAccount(t, s, "alice")
	k := keyFromByte(7)
	if err := s.AddKey("alice", k); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled("alice", false); err != nil {
		t.Fatal(err)
	}
	ok, err := s.AuthorizeKey("alice", k)
	if ok {
		t.Fatal("disabled account authorized")
	}
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled account error = %v, want ErrDisabled", err)
	}
}

func TestRemovedKeyDenies(t *testing.T) {
	s := NewStore()
	mustAccount(t, s, "alice")
	k := keyFromByte(7)
	if err := s.AddKey("alice", k); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveKey("alice", Fingerprint(k)); err != nil {
		t.Fatal(err)
	}
	ok, err := s.AuthorizeKey("alice", k)
	if err != nil || ok {
		t.Fatalf("removed key authorized: ok=%v err=%v", ok, err)
	}
}

func TestMissingAccountDenies(t *testing.T) {
	s := NewStore()
	ok, err := s.AuthorizeKey("ghost", keyFromByte(7))
	if ok {
		t.Fatal("missing account authorized")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account error = %v, want ErrNotFound", err)
	}
}

func TestDeterministicOrdering(t *testing.T) {
	s := NewStore()
	mustAccount(t, s, "zeta")
	mustAccount(t, s, "alpha")
	mustAccount(t, s, "mid")
	accounts := s.Accounts()
	for i := 1; i < len(accounts); i++ {
		if accounts[i-1].ID >= accounts[i].ID {
			t.Fatal("accounts not in AccountID ascending order")
		}
	}
	groups := s.Groups()
	for i := 1; i < len(groups); i++ {
		if groups[i-1].ID >= groups[i].ID {
			t.Fatal("groups not in GroupID ascending order")
		}
	}
	// Keys are sorted by fingerprint, not insertion order.
	big, midKey, small := keyFromByte(200), keyFromByte(100), keyFromByte(50)
	for _, k := range []Key{big, small, midKey} {
		if err := s.AddKey("zeta", k); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := s.AccountByName("zeta")
	for i := 1; i < int(a.KeyCount); i++ {
		if !keyLess(a.Keys[i-1], a.Keys[i]) {
			t.Fatal("keys not sorted by fingerprint")
		}
	}
}

func TestExhaustionAccounts(t *testing.T) {
	s := NewStore()
	for i := 0; i < MaxAccounts; i++ {
		name := "u" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if _, err := s.CreateAccount(name); err != nil {
			t.Fatalf("account %d: %v", i, err)
		}
	}
	if _, err := s.CreateAccount("overflow"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("accounts overflow = %v, want ErrExhausted", err)
	}
}

func TestExhaustionGroups(t *testing.T) {
	s := NewStore()
	for i := 0; i < MaxGroups; i++ {
		name := "g" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if _, err := s.CreateGroup(name); err != nil {
			t.Fatalf("group %d: %v", i, err)
		}
	}
	if _, err := s.CreateGroup("overflow"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("groups overflow = %v, want ErrExhausted", err)
	}
}

func TestExhaustionKeysPerAccount(t *testing.T) {
	s := NewStore()
	mustAccount(t, s, "alice")
	for i := 0; i < MaxKeysPerAccount; i++ {
		if err := s.AddKey("alice", keyFromByte(byte(i))); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if err := s.AddKey("alice", keyFromByte(0xFF)); !errors.Is(err, ErrExhausted) {
		t.Fatalf("per-account key overflow = %v, want ErrExhausted", err)
	}
}

func TestExhaustionTotalKeys(t *testing.T) {
	s := NewStore()
	for i := 0; i < MaxTotalKeys/MaxKeysPerAccount+1; i++ {
		name := "u" + string(rune('a'+i))
		if _, err := s.CreateAccount(name); err != nil {
			t.Fatalf("account %d: %v", i, err)
		}
	}
	// Fill 16 accounts to 8 keys each = 128 total; leave the 17th empty.
	for i := 0; i < MaxTotalKeys/MaxKeysPerAccount; i++ {
		name := "u" + string(rune('a'+i))
		for j := 0; j < MaxKeysPerAccount; j++ {
			if err := s.AddKey(name, keyFromByte(byte(i*MaxKeysPerAccount+j))); err != nil {
				t.Fatalf("key %d.%d: %v", i, j, err)
			}
		}
	}
	if got := totalKeys(s); got != MaxTotalKeys {
		t.Fatalf("total keys = %d, want %d", got, MaxTotalKeys)
	}
	// The 17th account has room per-account but the global bound is hit.
	last := "u" + string(rune('a'+MaxTotalKeys/MaxKeysPerAccount))
	if err := s.AddKey(last, keyFromByte(0xEE)); !errors.Is(err, ErrExhausted) {
		t.Fatalf("total-key overflow = %v, want ErrExhausted", err)
	}
}

func TestIDWrapRefused(t *testing.T) {
	s := NewStore()
	s.nextAccountID = maxUint64
	if _, err := s.CreateAccount("a"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("AccountID wrap = %v, want ErrExhausted", err)
	}
	s = NewStore()
	s.nextGroupID = maxUint64
	if _, err := s.CreateGroup("g"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("GroupID wrap = %v, want ErrExhausted", err)
	}
}

func TestSetPrimaryGIDAndSetGroupsByNumber(t *testing.T) {
	s := NewStore()
	mustAccount(t, s, "alice")
	shared, err := s.CreateGroup("shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrimaryGID("alice", shared.GID); err != nil {
		t.Fatal(err)
	}
	a, _ := s.AccountByName("alice")
	if a.PrimaryGID != shared.ID {
		t.Fatalf("primary = %d, want %d", a.PrimaryGID, shared.ID)
	}
	if err := s.SetGroups("alice", []uint32{shared.GID}); err != nil {
		t.Fatal(err)
	}
	a, _ = s.AccountByName("alice")
	if a.GroupCount != 1 || a.Groups[0] != shared.ID {
		t.Fatalf("supplementary = %v", a.Groups[:a.GroupCount])
	}
}

func TestSetMembersByUID(t *testing.T) {
	s := NewStore()
	alice := mustAccount(t, s, "alice")
	bob := mustAccount(t, s, "bob")
	wheel, err := s.CreateGroup("wheel")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembers("wheel", []uint32{alice.UID, bob.UID}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{alice.UID, bob.UID} {
		a, err := s.AccountByUID(uid)
		if err != nil {
			t.Fatal(err)
		}
		if a.GroupCount != 1 || a.Groups[0] != wheel.ID {
			t.Fatalf("account %d membership = %v", uid, a.Groups[:a.GroupCount])
		}
	}
	// Narrow membership to alice only.
	if err := s.SetMembers("wheel", []uint32{alice.UID}); err != nil {
		t.Fatal(err)
	}
	b, _ := s.AccountByUID(bob.UID)
	if b.GroupCount != 0 {
		t.Fatalf("bob membership after narrow = %v", b.Groups[:b.GroupCount])
	}
	// Referenced group deletion still conflicts.
	if err := s.DeleteGroup("wheel"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete referenced group = %v, want conflict", err)
	}
}

func TestAccountByUIDGroupByGID(t *testing.T) {
	s := NewStore()
	a := mustAccount(t, s, "alice")
	got, err := s.AccountByUID(a.UID)
	if err != nil || got.ID != a.ID {
		t.Fatalf("AccountByUID = %v, %v", got, err)
	}
	if _, err := s.AccountByUID(0x1234); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AccountByUID missing = %v, want ErrNotFound", err)
	}
	g := s.Groups()[0]
	gotg, err := s.GroupByGID(g.GID)
	if err != nil || gotg.ID != g.ID {
		t.Fatalf("GroupByGID = %v, %v", gotg, err)
	}
	if _, err := s.GroupByGID(0x1234); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GroupByGID missing = %v, want ErrNotFound", err)
	}
}

func TestNameAndHomeValidation(t *testing.T) {
	s := NewStore()
	for _, bad := range []string{"", "a b", "a/b", "a.b", "a\n", "toolongtoolongtoolongtoolongtool"} {
		if _, err := s.CreateAccount(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("CreateAccount(%q) = %v, want ErrInvalid", bad, err)
		}
		if _, err := s.CreateGroup(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("CreateGroup(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	mustAccount(t, s, "alice")
	if err := s.SetHome("alice", string(bytes.Repeat([]byte{'x'}, MaxHome+1))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overlong home = %v, want ErrInvalid", err)
	}
	if err := s.SetUID("alice", ReservedUID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reserved UID set = %v, want ErrInvalid", err)
	}
	if err := s.SetUID("alice", ReservedGID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reserved GID-as-UID set = %v, want ErrInvalid", err)
	}
}

func TestDuplicateUIDAndKeyRejected(t *testing.T) {
	s := NewStore()
	a := mustAccount(t, s, "alice")
	b := mustAccount(t, s, "bob")
	if err := s.SetUID("bob", a.UID); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate UID = %v, want ErrExists", err)
	}
	k := keyFromByte(5)
	if err := s.AddKey("alice", k); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey("alice", k); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate key = %v, want ErrExists", err)
	}
	_ = b
}

func TestSetGroupsDangling(t *testing.T) {
	s := NewStore()
	mustAccount(t, s, "alice")
	if err := s.SetGroups("alice", []uint32{9999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling supplementary GID = %v, want ErrNotFound", err)
	}
	if err := s.SetPrimaryGID("alice", 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling primary GID = %v, want ErrNotFound", err)
	}
	if err := s.SetMembers("alice", []uint32{9999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling member UID = %v, want ErrNotFound", err)
	}
}

// reDigest recomputes and rewrites the trailing SHA-256 digest so a mutated
// snapshot still has a valid digest.
func reDigest(data []byte) []byte {
	body := data[:len(data)-digestSize]
	sum := sha256.Sum256(body)
	copy(data[len(data)-digestSize:], sum[:])
	return data
}

func TestEpochPersistsAcrossDecodeEncode(t *testing.T) {
	epoch := epochFromByte(0xAB)
	s := NewStoreWithEpoch(epoch)
	mustAccount(t, s, "alice")
	data, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Epoch() != epoch {
		t.Fatalf("epoch after decode = %x, want %x", decoded.Epoch(), epoch)
	}
	again, err := Encode(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatal("epoch changed across decode/encode")
	}
}

func TestDecodeRejectsZeroEpoch(t *testing.T) {
	s := NewStore() // zero epoch: in-memory only
	data, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(data); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero-epoch decode = %v, want ErrInvalid", err)
	}
	if err := Validate(data); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero-epoch validate = %v, want ErrInvalid", err)
	}
}

func TestOldV1FormatRefused(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	data, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	// V1 magic: the magic check fires before the version is read, so only the
	// magic needs to be swapped and re-digested.
	legacy := append(append([]byte(nil), legacyMagic...), data[magicSize:]...)
	legacy = reDigest(legacy)
	if _, err := Decode(legacy); !errors.Is(err, ErrOldFormat) {
		t.Fatalf("V1 magic = %v, want ErrOldFormat", err)
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("V1 magic = %v, want ErrInvalid wrapping", err)
	}

	// V2 magic with V1 version must also be refused as old format.
	v1version := append([]byte(nil), data...)
	binary.LittleEndian.PutUint16(v1version[magicSize:magicSize+versionSize], legacyVersion)
	v1version = reDigest(v1version)
	if _, err := Decode(v1version); !errors.Is(err, ErrOldFormat) {
		t.Fatalf("V1 version = %v, want ErrOldFormat", err)
	}
}

func TestDecodeRejectsBadCounters(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Store)
	}{
		{"nextAccountID zero", func(s *Store) { s.nextAccountID = 0 }},
		{"nextGroupID zero", func(s *Store) { s.nextGroupID = 0 }},
		{"nextUID below base", func(s *Store) { s.nextUID = 0 }},
		{"nextGID below base", func(s *Store) { s.nextGID = 0 }},
		{"nextUID past domain", func(s *Store) { s.nextUID = 0x100000000 }},
		{"nextGID past domain", func(s *Store) { s.nextGID = 0x100000000 }},
		{"nextUID not above issued", func(s *Store) { s.nextUID = 1000 }},
		{"nextGID not above issued", func(s *Store) { s.nextGID = 1000 }},
	}
	for _, c := range cases {
		s := NewStoreWithEpoch(epochFromByte(0x01))
		mustAccount(t, s, "alice") // issues UID 1000, GID 1000
		c.mut(s)
		if _, err := Decode(hostilePayload(t, s)); err == nil {
			t.Fatalf("%s decoded", c.name)
		}
	}
}

func TestCloneIndependence(t *testing.T) {
	s := NewStoreWithEpoch(epochFromByte(0x01))
	mustAccount(t, s, "alice")
	if err := s.SetHome("alice", "/home/alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey("alice", keyFromByte(1)); err != nil {
		t.Fatal(err)
	}
	c := s.Clone()

	// Mutating the clone must not affect the original.
	mustAccount(t, c, "bob")
	if err := c.SetHome("alice", "/changed"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddKey("alice", keyFromByte(9)); err != nil {
		t.Fatal(err)
	}

	orig, _ := s.AccountByName("alice")
	if orig.HomePath() != "/home/alice" {
		t.Fatalf("clone mutated original home: %q", orig.HomePath())
	}
	if orig.KeyCount != 1 {
		t.Fatalf("clone mutated original key count: %d", orig.KeyCount)
	}
	if _, err := s.AccountByName("bob"); !errors.Is(err, ErrNotFound) {
		t.Fatal("clone mutation leaked into original accounts")
	}

	// Mutating the original must not affect the clone.
	if err := s.AddKey("alice", keyFromByte(2)); err != nil {
		t.Fatal(err)
	}
	ca, _ := c.AccountByName("alice")
	if ca.KeyCount != 2 {
		t.Fatalf("original mutation leaked into clone key count: %d", ca.KeyCount)
	}
	if c.Epoch() != s.Epoch() {
		t.Fatal("clone epoch differs from original")
	}
}
