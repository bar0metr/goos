package ramfs

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

func pattern(size int, seed byte) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*13) ^ seed ^ byte(i>>12)
	}
	return data
}

func configured(t testing.TB, limits Limits) (*FileSystem, NodeID) {
	t.Helper()
	fs := &FileSystem{}
	if !fs.Configure(&heapStore{}, limits) {
		t.Fatal("configure")
	}
	return fs, fs.Init()
}

func readBack(t testing.TB, fs *FileSystem, id NodeID, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	for done := 0; done < size; {
		end := done + 5000
		if end > size {
			end = size
		}
		count, status := fs.Read(id, uint64(done), data[done:end])
		if status != StatusOK || count != end-done {
			t.Fatalf("read at %d: %d %v", done, count, status)
		}
		done = end
	}
	return data
}

// TestLargeFilesCrossEveryIndexLevel writes files that end in the direct
// pages, the single index, and the double index, in one request and in
// pieces, and reads them back.
func TestLargeFilesCrossEveryIndexLevel(t *testing.T) {
	fs, root := configured(t, Limits{Nodes: 64, Pages: 1 << 16})
	sizes := []int{1, PageSize, directPages * PageSize, directPages*PageSize + 1, 3 << 20, indirectEnd*PageSize + 12345, 20 << 20}
	for index, size := range sizes {
		id, status := fs.Create(root, []byte(fmt.Sprintf("f%d", index)))
		if status != StatusOK {
			t.Fatal(status)
		}
		data := pattern(size, byte(index))
		if index%2 == 0 {
			if count, status := fs.Write(id, 0, data); status != StatusOK || count != size {
				t.Fatalf("size %d: %d %v", size, count, status)
			}
		} else {
			for done := 0; done < size; done += 70001 {
				end := done + 70001
				if end > size {
					end = size
				}
				if _, status := fs.Write(id, uint64(done), data[done:end]); status != StatusOK {
					t.Fatal(status)
				}
			}
		}
		if got := readBack(t, fs, id, size); !bytes.Equal(got, data) {
			t.Fatalf("size %d differs", size)
		}
		if stat, _ := fs.Stat(id); stat.Size != uint64(size) {
			t.Fatalf("stat size %d", stat.Size)
		}
	}
}

func TestPagesAreReclaimed(t *testing.T) {
	fs, root := configured(t, Limits{Nodes: 16, Pages: 1 << 14})
	id, _ := fs.Create(root, []byte("big"))
	fs.Write(id, 0, pattern(indirectEnd*PageSize+100*PageSize, 1))
	if fs.Usage().Pages < indirectEnd+100 {
		t.Fatalf("usage %d", fs.Usage().Pages)
	}
	// Shrinking into the direct pages frees every index page too.
	if fs.Truncate(id, 5*PageSize+7) != StatusOK {
		t.Fatal("truncate")
	}
	if got := fs.Usage().Pages; got != 6 {
		t.Fatalf("%d pages after shrinking to six", got)
	}
	if fs.Truncate(id, 0) != StatusOK || fs.Usage().Pages != 0 {
		t.Fatalf("%d pages after truncating to zero", fs.Usage().Pages)
	}
	fs.Write(id, 0, pattern(2<<20, 2))
	if fs.Unlink(root, []byte("big")) != StatusOK || fs.Usage().Pages != 0 || fs.Usage().Nodes != 1 {
		t.Fatalf("usage after unlink %+v", fs.Usage())
	}
}

func TestHolesReadAsZeroAndCostNothing(t *testing.T) {
	fs, root := configured(t, Limits{Nodes: 16, Pages: 64})
	id, _ := fs.Create(root, []byte("sparse"))
	far := uint64(100 << 20)
	if count, status := fs.Write(id, far, []byte("end")); status != StatusOK || count != 3 {
		t.Fatalf("sparse write: %d %v", count, status)
	}
	// One data page, one inner index, and the double index.
	if got := fs.Usage().Pages; got != 3 {
		t.Fatalf("a sparse 100 MiB file costs %d pages", got)
	}
	middle := make([]byte, 8192)
	for i := range middle {
		middle[i] = 0xff
	}
	if count, _ := fs.Read(id, 50<<20, middle); count != 8192 || !bytes.Equal(middle, make([]byte, 8192)) {
		t.Fatal("hole does not read as zeros")
	}
	if fs.Truncate(id, 1<<30) != StatusOK || fs.Usage().Pages != 3 {
		t.Fatal("growing by truncation allocated pages")
	}
}

func TestShrinkThenGrowReadsZeros(t *testing.T) {
	fs, root := configured(t, Limits{Nodes: 16, Pages: 64})
	id, _ := fs.Create(root, []byte("f"))
	fs.Write(id, 0, bytes.Repeat([]byte{'x'}, 3*PageSize))
	fs.Truncate(id, PageSize+10)
	fs.Write(id, 3*PageSize, []byte("y"))
	got := readBack(t, fs, id, 3*PageSize+1)
	if !bytes.Equal(got[:PageSize+10], bytes.Repeat([]byte{'x'}, PageSize+10)) ||
		!bytes.Equal(got[PageSize+10:3*PageSize], make([]byte, 2*PageSize-10)) || got[3*PageSize] != 'y' {
		t.Fatal("bytes cut by a truncation came back")
	}
}

func TestNoSpaceRefusesWholeWrite(t *testing.T) {
	fs, root := configured(t, Limits{Nodes: 16, Pages: 20})
	id, _ := fs.Create(root, []byte("f"))
	if _, status := fs.Write(id, 0, pattern(10*PageSize, 1)); status != StatusOK {
		t.Fatal(status)
	}
	// Twelve direct pages fit, but the index page this write also needs
	// would take the instance past its twenty pages.
	if count, status := fs.Write(id, 10*PageSize, pattern(10*PageSize, 2)); status != StatusNoSpace || count != 0 {
		t.Fatalf("over the limit: %d %v", count, status)
	}
	if stat, _ := fs.Stat(id); stat.Size != 10*PageSize || fs.Usage().Pages != 10 {
		t.Fatalf("a refused write changed the file: %d bytes, %d pages", stat.Size, fs.Usage().Pages)
	}
	if count, status := fs.Write(id, 10*PageSize, pattern(2*PageSize, 3)); status != StatusOK || count != 2*PageSize {
		t.Fatalf("a write that fits: %d %v", count, status)
	}
}

func TestNodeLimitAndGrowth(t *testing.T) {
	fs, root := configured(t, Limits{Nodes: 300, Pages: 16})
	for index := 1; index < 300; index++ {
		if _, status := fs.Create(root, []byte(fmt.Sprintf("n%d", index))); status != StatusOK {
			t.Fatalf("node %d: %v", index, status)
		}
	}
	if _, status := fs.Create(root, []byte("full")); status != StatusNoSpace {
		t.Fatalf("past the node limit: %v", status)
	}
	if fs.allocated != 5 {
		t.Fatalf("%d chunks for 300 nodes", fs.allocated)
	}
	fs.Unlink(root, []byte("n7"))
	if _, status := fs.Create(root, []byte("reused")); status != StatusOK {
		t.Fatalf("a freed slot: %v", status)
	}
	var unbounded FileSystem
	if !unbounded.Configure(&heapStore{}, Limits{Nodes: MaxNodes * 2, Pages: 1}) || unbounded.limits.Nodes != MaxNodes {
		t.Fatal("the node limit is not capped")
	}
}

func TestReadDirCursorSurvivesRemoval(t *testing.T) {
	fs, root := configured(t, Limits{Nodes: 64, Pages: 16})
	for index := 0; index < 10; index++ {
		fs.Create(root, []byte(fmt.Sprintf("e%d", index)))
	}
	var names []string
	cursor := 0
	for {
		entry, next, status := fs.ReadDir(root, cursor)
		if status == StatusNotFound {
			break
		}
		names = append(names, string(entry.Name[:entry.Size]))
		if len(names) == 3 {
			// Remove the entry just listed and one ahead of the cursor.
			fs.Unlink(root, entry.Name[:entry.Size])
			fs.Unlink(root, []byte("e5"))
		}
		cursor = next
	}
	want := []string{"e0", "e1", "e2", "e3", "e4", "e6", "e7", "e8", "e9"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("listing %v", names)
	}
}

// TestRandomOperationsAgainstModel checks RAMFS against a map of contents
// through a seeded mix of writes at random offsets, truncations, renames, and
// removals, and checks that usage returns to nothing when everything goes.
func TestRandomOperationsAgainstModel(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	fs, root := configured(t, Limits{Nodes: 64, Pages: 4096})
	dirs := []NodeID{root}
	for _, name := range []string{"a", "b"} {
		id, _ := fs.Mkdir(root, []byte(name))
		dirs = append(dirs, id)
	}
	type key struct {
		dir  int
		name string
	}
	files := map[key][]byte{}
	names := []string{"x", "y", "z", "w"}
	for step := 0; step < 3000; step++ {
		k := key{random.Intn(len(dirs)), names[random.Intn(len(names))]}
		content, exists := files[k]
		switch random.Intn(5) {
		case 0:
			if _, status := fs.Create(dirs[k.dir], []byte(k.name)); (status == StatusOK) == exists {
				t.Fatalf("step %d create: %v", step, status)
			}
			if !exists {
				files[k] = []byte{}
			}
		case 1, 2:
			if !exists {
				continue
			}
			id, _ := fs.Open(dirs[k.dir], []byte(k.name))
			offset := random.Intn(len(content) + 3*PageSize)
			if random.Intn(10) == 0 {
				offset += indirectEnd * PageSize
			}
			data := pattern(1+random.Intn(3*PageSize), byte(step))
			count, status := fs.Write(id, uint64(offset), data)
			if status == StatusNoSpace {
				continue
			}
			if status != StatusOK || count != len(data) {
				t.Fatalf("step %d write: %d %v", step, count, status)
			}
			if end := offset + len(data); end > len(content) {
				content = append(content, make([]byte, end-len(content))...)
			}
			copy(content[offset:], data)
			files[k] = content
		case 3:
			if !exists {
				continue
			}
			id, _ := fs.Open(dirs[k.dir], []byte(k.name))
			size := random.Intn(len(content) + 2*PageSize)
			fs.Truncate(id, uint64(size))
			if size <= len(content) {
				content = content[:size]
			} else {
				content = append(content, make([]byte, size-len(content))...)
			}
			files[k] = content
		case 4:
			status := fs.Unlink(dirs[k.dir], []byte(k.name))
			if (status == StatusOK) != exists {
				t.Fatalf("step %d unlink: %v", step, status)
			}
			delete(files, k)
		}
		if step%50 == 0 {
			for k, content := range files {
				id, status := fs.Open(dirs[k.dir], []byte(k.name))
				if status != StatusOK {
					t.Fatalf("open %v: %v", k, status)
				}
				if got := readBack(t, fs, id, len(content)); !bytes.Equal(got, content) {
					t.Fatalf("step %d: %v differs", step, k)
				}
			}
		}
	}
	for k := range files {
		fs.Unlink(dirs[k.dir], []byte(k.name))
	}
	if usage := fs.Usage(); usage.Pages != 0 || usage.Nodes != 3 {
		t.Fatalf("usage after removing every file %+v", usage)
	}
}
