package fat32

import (
	"fmt"
	"goos/kernel/ramfs"
	"math/rand"
	"testing"
	"time"
)

// torture does everything a client could to a mounted volume: it walks every
// directory it can reach, stats, reads and lists everything, then tries to
// write, grow, shrink, create, rename and remove. It returns how many
// operations failed with a typed error. It must return — a hang or a panic
// fails the test — and every error must be a defined status.
func torture(t *testing.T, device *memoryDevice) (failures int) {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		var fs FileSystem
		root, ok := fs.Mount(device)
		if !ok {
			done <- 1
			return
		}
		count := 0
		note := func(status ramfs.Status) {
			if status > ramfs.LastStatus {
				panic(fmt.Sprintf("undefined status %d", status))
			}
			if status != ramfs.StatusOK {
				count++
			}
		}
		buffer := make([]byte, 64<<10)
		var walk func(dir ramfs.NodeID, depth int)
		walk = func(dir ramfs.NodeID, depth int) {
			if depth > 8 {
				return
			}
			var children []ramfs.Entry
			for cursor := 0; len(children) < 200; {
				entry, next, status := fs.ReadDir(dir, cursor)
				if status != ramfs.StatusOK {
					if status != ramfs.StatusNotFound {
						note(status)
					}
					break
				}
				if next <= cursor {
					panic("a listing cursor went backwards")
				}
				cursor = next
				children = append(children, entry)
			}
			for _, child := range children {
				name, ok := ramfs.MakeName(child.Name[:child.Size])
				if !ok {
					panic(fmt.Sprintf("listed a name that is not a name: %q", child.Name[:child.Size]))
				}
				id, status := fs.OpenName(dir, name)
				note(status)
				if status != ramfs.StatusOK {
					continue
				}
				_, status = fs.Stat(id)
				note(status)
				_, status = fs.Metadata(id)
				note(status)
				if child.Stat.Kind == ramfs.KindDirectory {
					walk(id, depth+1)
					continue
				}
				for offset := uint64(0); offset < child.Stat.Size && offset < 1<<20; offset += uint64(len(buffer)) {
					_, status = fs.Read(id, offset, buffer)
					note(status)
				}
				_, status = fs.Write(id, child.Stat.Size, []byte("appended"))
				note(status)
				note(fs.Truncate(id, child.Stat.Size/2))
				note(fs.Truncate(id, child.Stat.Size+5000))
			}
			created, status := fs.CreateName(dir, mustNameOf("created by torture.txt"))
			note(status)
			if status == ramfs.StatusOK {
				_, status = fs.Write(created, 0, buffer[:3000])
				note(status)
			}
			if len(children) != 0 {
				name, _ := ramfs.MakeName(children[0].Name[:children[0].Size])
				if id, status := fs.OpenName(dir, name); status == ramfs.StatusOK {
					note(fs.RenameName(id, root, mustNameOf(fmt.Sprintf("moved %d", depth))))
				}
			}
			for _, child := range children {
				name, _ := ramfs.MakeName(child.Name[:child.Size])
				note(fs.UnlinkName(dir, name))
			}
		}
		walk(root, 0)
		_, status := fs.FreeClusters()
		note(status)
		_, status = fs.CompareFATs()
		if status != ramfs.StatusUnsupported {
			note(status)
		}
		fs.Flush()
		done <- count
	}()
	select {
	case failures = <-done:
		return failures
	case <-time.After(60 * time.Second):
		t.Fatal("the volume hung the filesystem")
	}
	return 0
}

func mustNameOf(value string) ramfs.Name {
	name, ok := ramfs.MakeName([]byte(value))
	if !ok {
		panic(value)
	}
	return name
}

// populated returns a volume holding a small tree: nested directories, long
// and short names, a multi-cluster file, and the IDs a corruption needs.
type fixture struct {
	device        *memoryDevice
	fs            *FileSystem
	root, dir     ramfs.NodeID
	big, small    ramfs.NodeID
	sub           ramfs.NodeID
	bigFirst      uint32
	smallFirst    uint32
	subCluster    uint32
	dirCluster    uint32
	secondOfBig   uint32
	lastOfBig     uint32
	longNameIndex int
}

func populated(t *testing.T) fixture {
	t.Helper()
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	f := fixture{device: device, fs: fs, root: root}
	f.dir = mkdir(t, fs, root, "Каталог с длинным именем")
	f.sub = mkdir(t, fs, f.dir, "sub")
	f.big = writeFile(t, fs, f.dir, "big file.bin", string(pattern(20000, 4)))
	f.small = writeFile(t, fs, root, "small.txt", "tiny")
	writeFile(t, fs, f.sub, "deep.txt", "deep")
	f.bigFirst = entryClusterOf(t, fs, f.big)
	f.smallFirst = entryClusterOf(t, fs, f.small)
	f.subCluster = entryClusterOf(t, fs, f.sub)
	f.dirCluster = entryClusterOf(t, fs, f.dir)
	f.secondOfBig, _, _ = fs.next(f.bigFirst)
	f.lastOfBig, _, _ = fs.last(f.bigFirst)
	return f
}

// setBoth writes a FAT entry into both copies, as a consistent corruption.
func (f fixture) setBoth(cluster, value uint32) {
	for copy := uint64(0); copy < 2; copy++ {
		put32(f.device.data[f.fs.fatStart+copy*f.fs.fatSize+uint64(cluster)*4:], value)
	}
}

// entry rewrites the short entry of id.
func (f fixture) entry(t *testing.T, id ramfs.NodeID, change func(entry *[32]byte)) {
	t.Helper()
	var fs FileSystem
	fs.Mount(f.device)
	dir, index, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	change(&entry)
	fs.writeEntry(&dir, index, entry)
}

// TestCorruptionCorpus damages a volume in each way the specification lists
// and requires every one to end in a typed error or a usable volume — never a
// panic, a hang, or a write outside the device.
func TestCorruptionCorpus(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, f fixture){
		"FAT loop":              func(t *testing.T, f fixture) { f.setBoth(f.lastOfBig, f.bigFirst) },
		"self loop":             func(t *testing.T, f fixture) { f.setBoth(f.bigFirst, f.bigFirst) },
		"out-of-range link":     func(t *testing.T, f fixture) { f.setBoth(f.bigFirst, f.fs.clusters+2) },
		"link to cluster 1":     func(t *testing.T, f fixture) { f.setBoth(f.bigFirst, 1) },
		"reserved link value":   func(t *testing.T, f fixture) { f.setBoth(f.bigFirst, 0x0ffffff3) },
		"bad cluster in chain":  func(t *testing.T, f fixture) { f.setBoth(f.secondOfBig, badCluster) },
		"free cluster in chain": func(t *testing.T, f fixture) { f.setBoth(f.secondOfBig, 0) },
		"truncated chain":       func(t *testing.T, f fixture) { f.setBoth(f.secondOfBig, endOfChainMark) },
		"overlong chain": func(t *testing.T, f fixture) {
			f.entry(t, f.big, func(entry *[32]byte) { put32(entry[28:], 10) })
		},
		"cross-linked chains": func(t *testing.T, f fixture) {
			f.entry(t, f.small, func(entry *[32]byte) { setEntryCluster(entry, f.secondOfBig) })
		},
		"lost cluster": func(t *testing.T, f fixture) { f.setBoth(f.fs.clusters, endOfChainMark) },
		"mismatched FAT copies": func(t *testing.T, f fixture) {
			put32(f.device.data[f.fs.fatStart+f.fs.fatSize+uint64(f.bigFirst)*4:], 7)
		},
		"file starting at cluster 1": func(t *testing.T, f fixture) {
			f.entry(t, f.big, func(entry *[32]byte) { setEntryCluster(entry, 1) })
		},
		"file with bytes but no cluster": func(t *testing.T, f fixture) {
			f.entry(t, f.small, func(entry *[32]byte) { setEntryCluster(entry, 0) })
		},
		"directory without a cluster": func(t *testing.T, f fixture) {
			f.entry(t, f.dir, func(entry *[32]byte) { setEntryCluster(entry, 0) })
		},
		"directory naming a file's cluster": func(t *testing.T, f fixture) {
			f.entry(t, f.sub, func(entry *[32]byte) { setEntryCluster(entry, f.bigFirst) })
		},
		"directory whose dot is wrong": func(t *testing.T, f fixture) {
			offset := f.fs.clusterOffset(f.subCluster)
			put16(f.device.data[offset+26:], 99)
		},
		"dot-dot cycle": func(t *testing.T, f fixture) {
			// dir's ".." names sub, and sub's ".." names dir.
			offset := f.fs.clusterOffset(f.dirCluster) + entrySize
			put16(f.device.data[offset+26:], uint16(f.subCluster))
		},
		"directory chain loop": func(t *testing.T, f fixture) { f.setBoth(f.dirCluster, f.dirCluster) },
		"root chain loop":      func(t *testing.T, f fixture) { f.setBoth(f.fs.rootCluster, f.fs.rootCluster) },
		"size past 4 GiB boundary": func(t *testing.T, f fixture) {
			f.entry(t, f.big, func(entry *[32]byte) { put32(entry[28:], 0xffffffff) })
		},
		"orphan long entries": func(t *testing.T, f fixture) {
			// Delete the short entry of the long-named directory only.
			var fs FileSystem
			fs.Mount(f.device)
			dir, index, entry, _ := fs.node(f.dir)
			entry[0] = entryDeleted
			fs.writeEntry(&dir, index, entry)
		},
		"broken long-name sequence": func(t *testing.T, f fixture) {
			var fs FileSystem
			fs.Mount(f.device)
			dir, _ := fs.openDirectory(0)
			entry, _ := fs.readEntry(&dir, 0)
			entry[0] = longLast | 9
			fs.writeEntry(&dir, 0, entry)
		},
		"wrong long-name checksum": func(t *testing.T, f fixture) {
			var fs FileSystem
			fs.Mount(f.device)
			dir, _ := fs.openDirectory(0)
			entry, _ := fs.readEntry(&dir, 1)
			entry[13] ^= 0xff
			fs.writeEntry(&dir, 1, entry)
		},
		"invalid FSInfo":   func(t *testing.T, f fixture) { put32(f.device.data[512+fsInfoNextFree:], 0xdeadbeef) },
		"corrupt backup":   func(t *testing.T, f fixture) { f.device.data[6*512+13] = 3 },
		"corrupt primary":  func(t *testing.T, f fixture) { f.device.data[13] = 3 },
		"garbage in FAT 0": func(t *testing.T, f fixture) { put32(f.device.data[f.fs.fatStart:], 0) },
	} {
		t.Run(name, func(t *testing.T) {
			f := populated(t)
			damage(t, f)
			size := len(f.device.data)
			torture(t, f.device)
			if len(f.device.data) != size {
				t.Fatal("the device changed size")
			}
		})
	}
}

// TestRandomMetadataCorruption flips random bytes in the reserved region, the
// FATs, and the directory clusters of a populated volume, many times over,
// and tortures each result.
func TestRandomMetadataCorruption(t *testing.T) {
	seeds := 150
	if stress() {
		seeds = 3000
	}
	pristine := populated(t)
	image := append([]byte(nil), pristine.device.data...)
	metadataEnd := int(pristine.fs.dataStart)
	directories := []uint32{pristine.fs.rootCluster, pristine.dirCluster, pristine.subCluster}
	for seed := 0; seed < seeds; seed++ {
		random := rand.New(rand.NewSource(int64(seed)))
		device := &memoryDevice{data: append([]byte(nil), image...)}
		for flips := 1 + random.Intn(12); flips > 0; flips-- {
			var offset int
			if random.Intn(2) == 0 {
				offset = random.Intn(metadataEnd)
			} else {
				cluster := directories[random.Intn(len(directories))]
				offset = int(pristine.fs.clusterOffset(cluster)) + random.Intn(512)
			}
			device.data[offset] = byte(random.Intn(256))
		}
		t.Run(fmt.Sprint(seed), func(t *testing.T) { torture(t, device) })
	}
}
