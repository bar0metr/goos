package partition

import "testing"

func TestPartitionIdentitySeparatesDiskImages(t *testing.T) {
	var disk, firstPart, secondPart GUID
	disk[0], firstPart[0], secondPart[0] = 1, 2, 3
	volume := Volume{Start: 1 << 20, Length: 64 << 20, Index: 2}
	table := Table{Scheme: SchemeGPT, DiskGUID: disk, Count: 1}
	table.Partitions[0] = Partition{Index: 2, Start: volume.Start, Length: volume.Length, Unique: firstPart}
	first := IdentityOf(&table, volume)
	if !first.Valid() || !first.Same(first) {
		t.Fatalf("invalid GPT identity: %+v", first)
	}
	table.Partitions[0].Unique = secondPart
	second := IdentityOf(&table, volume)
	if first.Same(second) {
		t.Fatal("a replacement image with the same geometry kept the old partition identity")
	}
}

func TestMBRIdentityRequiresDiskSignature(t *testing.T) {
	volume := Volume{Start: 1 << 20, Length: 64 << 20, Index: 2}
	table := Table{Scheme: SchemeMBR, Count: 1}
	table.Partitions[0] = Partition{Index: 2, Start: volume.Start, Length: volume.Length, MBRType: 0x0c}
	if identity := IdentityOf(&table, volume); identity.Valid() {
		t.Fatal("an unsigned MBR disk was treated as stable across replacement")
	}
	table.MBRSignature = 0x12345678
	if identity := IdentityOf(&table, volume); !identity.Valid() {
		t.Fatalf("signed MBR identity invalid: %+v", identity)
	}
}
