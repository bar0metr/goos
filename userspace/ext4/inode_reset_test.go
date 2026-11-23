package ext4

import "testing"

// TestDecodeInodeIntoResetsDestination decodes one inode with optional state
// (a raw alias, a creation time) and then a second inode without those into
// the same destination, requiring the destination to be fully reset: the raw
// alias, HasCreate and Create must not leak between reused slots (ADR 0231).
func TestDecodeInodeIntoResetsDestination(t *testing.T) {
	// The legacy fixture has no metadata checksum, so a minimal synthetic
	// inode without extra fields decodes without a checksum check.
	fs := mount(t, fixture(t, "legacy"))
	dir := resolve(t, fs, "/")

	real := lookupInode(t, fs, dir, "hello.txt")
	if !real.HasCreate {
		t.Fatal("fixture inode lacks a creation time; test assumes extra_isize")
	}

	var dst Inode
	if err := fs.decodeInodeInto(&dst, real.Number, real.raw, true); err != nil {
		t.Fatal(err)
	}
	if dst.raw == nil || !dst.HasCreate || dst.Create == (Timestamp{}) {
		t.Fatal("first decode did not set optional state")
	}

	// A 128-byte inode with no extra fields: no creation time, no raw.
	minimal := make([]byte, goodOldIsize)
	putLE16(minimal[0x0:], ModeRegular|0o644)
	putLE16(minimal[0x1A:], 1)
	if err := fs.decodeInodeInto(&dst, 12345, minimal, false); err != nil {
		t.Fatal(err)
	}
	if dst.raw != nil {
		t.Fatal("raw alias survived the reset")
	}
	if dst.HasCreate {
		t.Fatal("HasCreate survived the reset")
	}
	if dst.Create != (Timestamp{}) {
		t.Fatal("Create survived the reset")
	}
	if dst.Number != 12345 || dst.Mode != ModeRegular|0o644 || dst.Links != 1 || dst.Size != 0 {
		t.Fatalf("scalar fields are not the second inode's: %+v", dst)
	}
}
