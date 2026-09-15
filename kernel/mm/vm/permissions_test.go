package vm

import "testing"

func TestPageEntryEnforcesWriteXorExecute(t *testing.T) {
	if _, ok := pageEntry(0x2000, Permissions{Writable: true, Executable: true}); ok {
		t.Fatal("accepted writable executable page")
	}
	entry, ok := pageEntry(0x2000, Permissions{Executable: true})
	if !ok || entry&entryNX != 0 || entry&entryWritable != 0 {
		t.Fatalf("invalid executable entry: %#x", entry)
	}
	entry, ok = pageEntry(0x3000, Permissions{Writable: true})
	if !ok || entry&entryNX == 0 || entry&entryWritable == 0 {
		t.Fatalf("invalid writable entry: %#x", entry)
	}
}

func TestPageEntryRejectsUnalignedAddress(t *testing.T) {
	if _, ok := pageEntry(0x2001, Permissions{}); ok {
		t.Fatal("accepted unaligned physical address")
	}
}

func TestUserPagePermissions(t *testing.T) {
	entry, ok := pageEntry(0x5000, Permissions{User: true, Writable: true})
	if !ok || entry&entryUser == 0 || entry&entryWritable == 0 || entry&entryNX == 0 {
		t.Fatalf("invalid user data entry: %#x", entry)
	}
	if _, ok := pageEntry(0x5000, Permissions{User: true, Writable: true, Executable: true}); ok {
		t.Fatal("accepted W+X user page")
	}
}

func TestValidateUserRange(t *testing.T) {
	tests := []struct {
		name    string
		address uint64
		length  uint64
		valid   bool
	}{
		{name: "one byte", address: IdentityMapLimit, length: 1, valid: true},
		{name: "last byte", address: UserAddressLimit - 1, length: 1, valid: true},
		{name: "zero length", address: IdentityMapLimit, valid: true},
		{name: "kernel address", address: IdentityMapLimit - 1, length: 1},
		{name: "past limit", address: UserAddressLimit, length: 1},
		{name: "crosses limit", address: UserAddressLimit - 1, length: 2},
		{name: "overflow", address: ^uint64(0) - 1, length: 4},
	}
	for _, test := range tests {
		if got := ValidateUserRange(test.address, test.length); got != test.valid {
			t.Errorf("%s: ValidateUserRange(%#x, %d) = %v", test.name, test.address, test.length, got)
		}
	}
}
