package ext4

import "testing"

// BenchmarkPrefetchDir and BenchmarkPerEntryListing compare the listing
// prefetch with the per-entry walk on the host. The authoritative M2 evidence
// is the Proxmox fsbench run; these are for local regression spotting only.
func BenchmarkPrefetchDir(b *testing.B) {
	fs := mount(b, fixture(b, "canonical"))
	dir := resolve(b, fs, "/big")
	var entries [MaxReaddirPrefetchEntries]DirEntry
	var inodes [MaxReaddirPrefetchEntries]Inode
	b.ReportAllocs()
	b.ResetTimer()
	for round := 0; round < b.N; round++ {
		cursor := uint64(0)
		for {
			n, next, end, err := fs.PrefetchDir(&fs.list, dir, cursor, &entries, &inodes)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(n))
			cursor = next
			if end {
				break
			}
		}
	}
}

func BenchmarkPerEntryListing(b *testing.B) {
	fs := mount(b, fixture(b, "canonical"))
	dir := resolve(b, fs, "/big")
	b.ReportAllocs()
	b.ResetTimer()
	for round := 0; round < b.N; round++ {
		count := 0
		if _, err := fs.ReadDir(dir, 0, func(entry *DirEntry) bool {
			if _, err := fs.Inode(entry.Inode); err != nil {
				b.Fatal(err)
			}
			count++
			return true
		}); err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(count))
	}
}
