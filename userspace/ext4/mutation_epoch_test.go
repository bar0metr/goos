package ext4

import "testing"

// TestMutationEpochAdvancesOnOrdinaryMutation checks a small mutation folds
// once and advances the generation once (ADR 0231).
func TestMutationEpochAdvancesOnOrdinaryMutation(t *testing.T) {
	fs, _ := scratch(t, "canonical")
	dir := resolve(t, fs, "/big")
	before := fs.MutationEpoch()
	if _, err := fs.Create(dir, []byte("epoch-marker"), ModeRegular|0o644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if after := fs.MutationEpoch(); after != before+1 {
		t.Fatalf("epoch %d -> %d, want %d", before, after, before+1)
	}
}

// TestMutationEpochUnchangedOnPreFoldFailure checks an operation that fails
// before folding leaves the generation alone (ADR 0231).
func TestMutationEpochUnchangedOnPreFoldFailure(t *testing.T) {
	fs, _ := scratch(t, "canonical")
	dir := resolve(t, fs, "/")
	before := fs.MutationEpoch()
	if _, err := fs.Create(dir, []byte("hello.txt"), ModeRegular|0o644, 0, 0); err == nil {
		t.Fatal("expected duplicate create to fail")
	}
	if after := fs.MutationEpoch(); after != before {
		t.Fatalf("epoch %d -> %d, want unchanged", before, after)
	}
}

// TestMutationEpochAdvancesOnPartialWrite fills the volume until a write
// commits a prefix and then fails with KindNoSpace. The committed prefix was
// folded into the pending transaction, so the generation must have advanced
// even though the operation returned an error (ADR 0231).
func TestMutationEpochAdvancesOnPartialWrite(t *testing.T) {
	fs, _ := scratch(t, "canonical")
	file := ok(fs.Create(root(t, fs), []byte("fill"), ModeRegular|0o644, 0, 0))
	chunk := make([]byte, 4<<20)
	offset := uint64(0)
	for {
		epochBefore := fs.MutationEpoch()
		n, err := fs.WriteFile(ok(fs.Inode(file.Number)), offset, chunk)
		offset += uint64(n)
		if err == nil {
			continue
		}
		wantKind(t, err, KindNoSpace)
		if n == 0 {
			t.Fatal("write failed without committing a prefix")
		}
		if after := fs.MutationEpoch(); after <= epochBefore {
			t.Fatalf("partial write committed %d bytes but epoch %d did not advance past %d", n, after, epochBefore)
		}
		return
	}
}
