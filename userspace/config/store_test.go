package config

import "testing"

func seeded(t testing.TB) *Store {
	t.Helper()
	store := &Store{}
	version, _ := Text([]byte("0.1.0-dev"))
	name, _ := Text([]byte("goos"))
	for _, declaration := range []struct {
		key      string
		value    Value
		readOnly bool
	}{
		{"system.build.version", version, true},
		{"system.log.persist", Bool(true), false},
		{"system.shell.history", Uint(32), false},
		{"user.name", name, false},
	} {
		if status := store.Declare(declaration.key, declaration.value, declaration.readOnly); status != StatusOK {
			t.Fatalf("declare %s: %d", declaration.key, status)
		}
	}
	return store
}

func fullScope(t testing.TB) Scope {
	t.Helper()
	scope, ok := MakeScope("")
	if !ok {
		t.Fatal("empty scope rejected")
	}
	return scope
}

func TestTypedValuesAreCheckedAgainstTheirDeclaration(t *testing.T) {
	store, scope := seeded(t), fullScope(t)
	if value, status := store.Get(scope, Read, []byte("system.shell.history")); status != StatusOK || value.Kind != KindUint || value.Uint != 32 {
		t.Fatalf("get: %d %+v", status, value)
	}
	if status := store.Set(scope, Write, []byte("system.shell.history"), Uint(64)); status != StatusOK {
		t.Fatalf("set: %d", status)
	}
	if value, _ := store.Get(scope, Read, []byte("system.shell.history")); value.Uint != 64 {
		t.Fatalf("value not stored: %d", value.Uint)
	}
	// A value of the wrong type is refused rather than coerced.
	if status := store.Set(scope, Write, []byte("system.shell.history"), Bool(true)); status != StatusTypeMismatch {
		t.Fatalf("type mismatch accepted: %d", status)
	}
	// An undeclared key is not a new setting.
	if status := store.Set(scope, Write, []byte("system.shell.unknown"), Uint(1)); status != StatusNotFound {
		t.Fatalf("undeclared key accepted: %d", status)
	}
	if status := store.Set(scope, Write, []byte("system.build.version"), Uint(1)); status != StatusReadOnly && status != StatusTypeMismatch {
		t.Fatalf("read-only key accepted: %d", status)
	}
	published, _ := Text([]byte("9.9.9"))
	if status := store.Set(scope, Write, []byte("system.build.version"), published); status != StatusReadOnly {
		t.Fatalf("read-only key overwritten: %d", status)
	}
}

func TestScopeConfinesASessionToItsNamespace(t *testing.T) {
	store := seeded(t)
	userScope, ok := MakeScope("user.")
	if !ok {
		t.Fatal("user scope rejected")
	}
	if _, status := store.Get(userScope, Read, []byte("user.name")); status != StatusOK {
		t.Fatalf("own namespace: %d", status)
	}
	// Outside the scope reads as absent, so a confined session learns nothing
	// about what exists elsewhere.
	if _, status := store.Get(userScope, Read, []byte("system.shell.history")); status != StatusNotFound {
		t.Fatalf("out-of-scope read: %d", status)
	}
	if status := store.Set(userScope, Write, []byte("system.shell.history"), Uint(1)); status != StatusNotFound {
		t.Fatalf("out-of-scope write: %d", status)
	}
	// A scope matches whole components.
	if !userScope.covers([]byte("user.name")) || userScope.covers([]byte("username.x")) {
		t.Fatal("scope matched a partial component")
	}
	// Listing shows only the scope.
	key, _, _, cursor, status := store.At(userScope, Read, 0)
	if status != StatusOK || string(key) != "user.name" {
		t.Fatalf("list: %d %q", status, key)
	}
	if _, _, _, _, status := store.At(userScope, Read, cursor); status != StatusNotFound {
		t.Fatalf("list continued past the scope: %d", status)
	}
}

func TestRightsAreCheckedSeparatelyFromScope(t *testing.T) {
	store, scope := seeded(t), fullScope(t)
	if _, status := store.Get(scope, Write, []byte("user.name")); status != StatusPermissionDenied {
		t.Fatalf("read without read rights: %d", status)
	}
	if status := store.Set(scope, Read, []byte("user.name"), Bool(true)); status != StatusPermissionDenied {
		t.Fatalf("write without write rights: %d", status)
	}
	if _, _, _, _, status := store.At(scope, Write, 0); status != StatusPermissionDenied {
		t.Fatalf("list without read rights: %d", status)
	}
}

func TestKeysAndValuesAreValidated(t *testing.T) {
	store, scope := seeded(t), fullScope(t)
	for _, key := range []string{"", "system", ".system.x", "system.x.", "system..x", "System.X", "system.x y", "system.x_y"} {
		if status := store.Declare(key, Uint(1), false); status != StatusInvalidArgument {
			t.Fatalf("declared invalid key %q: %d", key, status)
		}
		if _, status := store.Get(scope, Read, []byte(key)); status == StatusOK {
			t.Fatalf("read invalid key %q", key)
		}
	}
	if _, ok := Text(make([]byte, MaxText+1)); ok {
		t.Fatal("oversized text accepted")
	}
	if _, ok := Text([]byte{0x01}); ok {
		t.Fatal("unprintable text accepted")
	}
	// A value whose contents contradict its own kind never enters the store.
	if status := store.Set(scope, Write, []byte("system.log.persist"), Value{Kind: KindBool, Uint: 2}); status != StatusInvalidArgument {
		t.Fatalf("malformed bool accepted: %d", status)
	}
	if status := store.Set(scope, Write, []byte("user.name"), Value{Kind: KindText, Uint: 1, Size: 1}); status != StatusInvalidArgument {
		t.Fatalf("malformed text accepted: %d", status)
	}
	if _, ok := MakeScope("user"); ok {
		t.Fatal("scope without a separator accepted")
	}
	if _, ok := MakeScope("."); ok {
		t.Fatal("bare separator accepted as a scope")
	}
}

func TestStoreIsBounded(t *testing.T) {
	store := &Store{}
	for index := 0; index < MaxEntries; index++ {
		key := "user.k" + string(rune('a'+index%26)) + string(rune('a'+index/26))
		if status := store.Declare(key, Uint(uint64(index)), false); status != StatusOK {
			t.Fatalf("declare %d: %d", index, status)
		}
	}
	if status := store.Declare("user.overflow", Uint(1), false); status != StatusResourceExhausted {
		t.Fatalf("store grew past its bound: %d", status)
	}
	// Re-declaring replaces rather than consuming another slot.
	if status := store.Declare("user.kaa", Uint(7), false); status != StatusOK {
		t.Fatalf("redeclare: %d", status)
	}
}

func TestExportedFormRoundTrips(t *testing.T) {
	store, scope := seeded(t), fullScope(t)
	var buffer []byte
	for cursor := 0; ; {
		key, value, readOnly, next, status := store.At(scope, Read, cursor)
		if status == StatusNotFound {
			break
		}
		if status != StatusOK {
			t.Fatalf("list: %d", status)
		}
		cursor = next
		if readOnly {
			// Read-only entries come from the running image, so exporting them
			// would create a second source of truth.
			continue
		}
		var ok bool
		if buffer, ok = AppendEntry(buffer, key, value); !ok {
			t.Fatalf("append %q", key)
		}
	}
	expected := "system.log.persist bool true\nsystem.shell.history uint 32\nuser.name text goos\n"
	if string(buffer) != expected {
		t.Fatalf("exported form:\n%q\nwanted:\n%q", buffer, expected)
	}

	restored := seeded(t)
	if status := restored.Set(scope, Write, []byte("system.shell.history"), Uint(1)); status != StatusOK {
		t.Fatalf("perturb: %d", status)
	}
	start := 0
	for index := 0; index < len(buffer); index++ {
		if buffer[index] != '\n' {
			continue
		}
		key, value, ok := ParseEntry(buffer[start:index])
		if !ok {
			t.Fatalf("parse %q", buffer[start:index])
		}
		if status := restored.Set(scope, Write, key, value); status != StatusOK {
			t.Fatalf("restore %q: %d", key, status)
		}
		start = index + 1
	}
	if value, _ := restored.Get(scope, Read, []byte("system.shell.history")); value.Uint != 32 {
		t.Fatalf("value not restored: %d", value.Uint)
	}
}

func TestExportedFormRejectsMalformedLines(t *testing.T) {
	for _, line := range []string{"", "key", "system.x", "system.x bool", "system.x bool maybe",
		"system.x uint twelve", "system.x uint 99999999999999999999999", "system.x float 1", ".x bool true"} {
		if _, _, ok := ParseEntry([]byte(line)); ok {
			t.Fatalf("parsed malformed line %q", line)
		}
	}
	// A text value may contain spaces; a key and a type may not.
	key, value, ok := ParseEntry([]byte("user.greeting text hello there"))
	if !ok || string(key) != "user.greeting" || string(value.Bytes()) != "hello there" {
		t.Fatalf("text with spaces: %v %q %q", ok, key, value.Bytes())
	}
}
