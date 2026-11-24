package ext4

import (
	"testing"
	"time"
)

func sameSetTraversalFS() (*FS, []byte) {
	const blockSize = 1024
	const blocks = 256
	device := &raceDevice{data: make([]byte, blockSize*blocks)}
	fs := &FS{sb: &Superblock{BlocksCount: blocks, FirstDataBlock: 1}, device: device, blockSize: blockSize}
	fs.cache.init(device, blockSize, blocks)
	return fs, device.data
}

func requireTraversalMapping(t *testing.T, run func() (Mapping, error), want Mapping) {
	t.Helper()
	type result struct {
		mapping Mapping
		err     error
	}
	done := make(chan result, 1)
	go func() {
		mapping, err := run()
		done <- result{mapping, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.mapping != want {
			t.Fatalf("mapping = %+v, error = %v; want %+v", got.mapping, got.err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("traversal blocked: parent cache borrow must end before same-set descent")
	}
}

func TestMapExtentReleasesSameSetBorrowBeforeDescending(t *testing.T) {
	fs, disk := sameSetTraversalFS()
	parent, child := uint32(3), uint32(3+cacheSets)
	inode := &Inode{Flags: inodeFlagExtents}
	header := func(node []byte, depth uint16) {
		putLE16(node, extentMagic)
		putLE16(node[2:], 1)
		putLE16(node[4:], uint16((len(node)-12)/12))
		putLE16(node[6:], depth)
	}
	root := inode.blockArea[:]
	header(root, 2)
	putLE32(root[12:], 7)
	putLE32(root[16:], parent)
	index := disk[int(parent)*int(fs.blockSize) : int(parent+1)*int(fs.blockSize)]
	header(index, 1)
	putLE32(index[12:], 7)
	putLE32(index[16:], child)
	leaf := disk[int(child)*int(fs.blockSize) : int(child+1)*int(fs.blockSize)]
	header(leaf, 0)
	putLE32(leaf[12:], 7)
	putLE16(leaf[16:], 5)
	putLE32(leaf[20:], 200)
	for _, label := range []string{"cold", "cached"} {
		t.Run(label, func(t *testing.T) {
			requireTraversalMapping(t, func() (Mapping, error) { return fs.mapExtent(inode, 9) },
				Mapping{Logical: 9, Physical: 202, Length: 3})
		})
	}
}

func TestMapIndirectReleasesSameSetBorrowBeforeDescending(t *testing.T) {
	for _, depth := range []int{2, 3} {
		t.Run(map[int]string{2: "double", 3: "triple"}[depth], func(t *testing.T) {
			fs, disk := sameSetTraversalFS()
			inode := &Inode{}
			perBlock := fs.blockSize / 4
			logical := uint32(12) + perBlock + 2*perBlock + 5
			path := []uint32{2, 5}
			if depth == 3 {
				logical = 12 + perBlock + perBlock*perBlock + perBlock*perBlock + 2*perBlock + 5
				path = []uint32{1, 2, 5}
			}
			first := uint32(3)
			putLE32(inode.blockArea[(11+depth)*4:], first)
			for level, slot := range path {
				block := first + uint32(level*cacheSets)
				next := block + cacheSets
				if level == len(path)-1 {
					next = 220
				}
				putLE32(disk[block*fs.blockSize+slot*4:], next)
			}
			for _, label := range []string{"cold", "cached"} {
				t.Run(label, func(t *testing.T) {
					requireTraversalMapping(t, func() (Mapping, error) { return fs.mapIndirect(inode, logical) },
						Mapping{Logical: uint64(logical), Physical: 220, Length: 1})
				})
			}
		})
	}
}
