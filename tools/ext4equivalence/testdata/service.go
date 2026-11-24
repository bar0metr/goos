package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync/atomic"
	"time"

	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	host "goos/tools/ext4equivalence/host"
	"goos/userspace/ext4"
	"goos/userspace/ext4vol"
	"goos/userspace/fsproto"
)

const requestWait = 100000
const commitInterval = 100000
const exitBlockGone = 3
const exitVFSGone = 2

var vfsHandle handle.ID = host.VFS
var blockHandle handle.ID = host.Block
var wire [ipc.WireMaxSize]byte
var requestMessage, replyMessage ipc.Message
var requestedName, listedName [ramfs.MaxName]byte
var cache = make([]byte, fsproto.CacheSize)
var volume observedVolume

func ticks() uint64 { return 0 }
func logCounters()  {}
func exitIfFailed() {
	if volume.Failed() {
		panic("volume failed")
	}
}
func fatal(message string) { panic(message) }

// observedVolume holds the first worker batch at entry until all four workers
// arrive. The native volume still performs each operation and creates each reply.
type observedVolume struct {
	ext4vol.Volume
	gate    atomic.Bool
	active  atomic.Int32
	arrived atomic.Int32
	barrier chan struct{}
}

func (v *observedVolume) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	if v.gate.Load() {
		v.active.Add(1)
		defer v.active.Add(-1)
		if v.arrived.Add(1) == 4 {
			close(v.barrier)
		}
		select {
		case <-v.barrier:
		case <-time.After(5 * time.Second):
			panic("four read workers did not overlap")
		}
	}
	return v.Volume.Read(id, offset, target)
}

type device struct{ data []byte }

func (d *device) ReadAt(offset uint64, target []byte) bool {
	end := offset + uint64(len(target))
	if end < offset || end > uint64(len(d.data)) {
		return false
	}
	copy(target, d.data[offset:end])
	return true
}
func (d *device) WriteAt(offset uint64, data []byte) bool {
	end := offset + uint64(len(data))
	if end < offset || end > uint64(len(d.data)) {
		return false
	}
	copy(d.data[offset:end], data)
	return true
}
func (d *device) Flush() bool      { return true }
func (d *device) Capacity() uint64 { return uint64(len(d.data)) }

// The transcript retains every decoded reply field and returned data byte.
// Labels replace transport correlation IDs; sorting removes completion order.
// The fixture and mutation clock fix inode metadata across both processes.
type logical struct {
	Label string
	Reply fsproto.Reply
	Data  []byte
}

var records []logical
var next uint64

func batch(labels []string, requests []fsproto.Request) []fsproto.Reply {
	if len(labels) != len(requests) {
		panic("labels")
	}
	start := next + 1
	host.BeginBatch()
	for i, r := range requests {
		switch r.Operation {
		case fsproto.OperationMount, fsproto.OperationOpen, fsproto.OperationStat, fsproto.OperationRead, fsproto.OperationFilesystemInfo:
		default:
			panic("mutation in read-only equivalence workload: " + labels[i])
		}
		next++
		var message ipc.Message
		if !fsproto.EncodeRequest(r, next, &message, cache) {
			panic(fmt.Sprintf("encode %s", labels[i]))
		}
		var data [ipc.WireMaxSize]byte
		n, ok := ipc.EncodeFrom(&message, data[:])
		if !ok {
			panic("wire encode")
		}
		host.Submit(data[:n])
	}
	host.EndBatch()
	results := make([]fsproto.Reply, len(requests))
	seen := make([]bool, len(requests))
	for range requests {
		select {
		case data := <-host.Replies:
			var message ipc.Message
			if !ipc.DecodeInto(data, &message) {
				panic("wire reply")
			}
			index := int(message.Correlation - start)
			if index < 0 || index >= len(requests) || seen[index] {
				panic("unexpected or duplicate correlation")
			}
			seen[index] = true
			var name [ramfs.MaxName]byte
			r, ok := fsproto.DecodeReply(&message, cache, &name)
			if !ok {
				panic("decode reply")
			}
			r.Name = append([]byte(nil), r.Name...)
			if r.Operation != requests[index].Operation {
				panic("operation mismatch")
			}
			results[index] = r
			record := logical{Label: labels[index], Reply: r}
			if r.Operation == fsproto.OperationRead {
				target, ok := fsproto.SlotBytes(cache, requests[index])
				if !ok || r.Value > uint64(len(target)) {
					panic("read bounds")
				}
				record.Data = append([]byte{}, target[:r.Value]...)
			}
			records = append(records, record)
		case <-time.After(15 * time.Second):
			panic("service reply timeout")
		}
	}
	return results
}
func one(label string, r fsproto.Request) fsproto.Reply {
	return batch([]string{label}, []fsproto.Request{r})[0]
}
func require(ok bool, message string) {
	if !ok {
		panic(message)
	}
}
func open(root ramfs.NodeID, name string) fsproto.Reply {
	r := one("open-"+name, fsproto.Request{Operation: fsproto.OperationOpen, Parent: root, Name: []byte(name)})
	require(r.Status == ramfs.StatusOK, "open "+name)
	return r
}
func read(label string, id ramfs.NodeID, offset uint64, size uint32, want []byte) {
	r := one(label, fsproto.Request{Operation: fsproto.OperationRead, Node: id, Offset: offset, Size: size})
	require(r.Status == ramfs.StatusOK && r.Value == uint64(len(want)), "read status/length "+label)
	require(bytes.Equal(records[len(records)-1].Data, want), "read bytes "+label)
}
func main() {
	input, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer input.Close()
	reader, err := gzip.NewReader(input)
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		panic(err)
	}
	reader.Close()
	require(volume.MountWritable(&device{data}, func() ext4.Timestamp { return ext4.Timestamp{Seconds: 1767225600} }), "mount refused "+volume.Refusal())
	// Prepare the unwritten-extent fixture before either service loop starts.
	// The compared protocol workload contains only read-only operations.
	fixtureRoot, fixtureStatus := volume.Root()
	require(fixtureStatus == ramfs.StatusOK, "fixture root")
	fixtureName, validName := ramfs.MakeName([]byte("empty"))
	require(validName, "fixture name")
	fixtureEmpty, fixtureStatus := volume.OpenName(fixtureRoot, fixtureName)
	require(fixtureStatus == ramfs.StatusOK, "fixture empty inode")
	require(volume.Truncate(fixtureEmpty, 32768) == ramfs.StatusOK, "extend unwritten fixture")
	require(volume.Flush(), "flush unwritten fixture")
	host.Reset()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				if code, ok := p.(host.ExitCode); !ok || code != host.ExitCode(exitBlockGone) {
					panic(p)
				}
			}
		}()
		runService()
	}()
	root := one("mount", fsproto.Request{Operation: fsproto.OperationMount})
	require(root.Status == ramfs.StatusOK && uint32(root.Node) == 2, "fixed root inode identity")
	hello := open(root.Node, "hello.txt")
	link := open(root.Node, "link-to-hello")
	dir := open(root.Node, "dir")
	sparse := open(root.Node, "sparse")
	fragmented := open(root.Node, "fragmented")
	empty := open(root.Node, "empty")
	require(hello.Node == link.Node && hello.Value == 11 && hello.Unix.Links == 2 && hello.Unix.Mode&ramfs.ModeTypeMask == ramfs.ModeRegular, "hardlink identity/metadata")
	require(hello.Modified.Available && hello.Modified.Seconds == 1767225600, "fixed modification time")
	require(dir.Kind == ramfs.KindDirectory, "directory kind")
	missing := one("missing", fsproto.Request{Operation: fsproto.OperationOpen, Parent: root.Node, Name: []byte("absent-equivalence")})
	require(missing.Status == ramfs.StatusNotFound, "missing lookup status")
	for repeat := 0; repeat < 3; repeat++ {
		opened := batch([]string{fmt.Sprintf("cached-open-file-%d", repeat), fmt.Sprintf("cached-open-dir-%d", repeat), fmt.Sprintf("cached-open-sparse-%d", repeat), fmt.Sprintf("cached-open-link-%d", repeat)}, []fsproto.Request{
			{Operation: fsproto.OperationOpen, Parent: root.Node, Name: []byte("hello.txt")},
			{Operation: fsproto.OperationOpen, Parent: root.Node, Name: []byte("dir")},
			{Operation: fsproto.OperationOpen, Parent: root.Node, Name: []byte("sparse")},
			{Operation: fsproto.OperationOpen, Parent: root.Node, Name: []byte("link-to-hello")},
		})
		require(opened[0].Node == hello.Node && opened[1].Node == dir.Node && opened[2].Node == sparse.Node && opened[3].Node == hello.Node, "cached lookup identity")
		labels := []string{fmt.Sprintf("stat-file-%d", repeat), fmt.Sprintf("stat-dir-%d", repeat), fmt.Sprintf("stat-sparse-%d", repeat), fmt.Sprintf("info-%d", repeat)}
		replies := batch(labels, []fsproto.Request{{Operation: fsproto.OperationStat, Node: hello.Node}, {Operation: fsproto.OperationStat, Node: dir.Node}, {Operation: fsproto.OperationStat, Node: sparse.Node}, {Operation: fsproto.OperationFilesystemInfo}})
		require(replies[0].Node == hello.Node && replies[0].Value == 11 && replies[0].Unix.Links == 2, "regular stat")
		require(replies[1].Kind == ramfs.KindDirectory && replies[2].Value == 5368709121, "directory/sparse stat")
		require(replies[3].Status == ramfs.StatusOK && replies[3].Filesystem.TotalBytes > 0 && replies[3].Filesystem.CapacityAvailable && !replies[3].Filesystem.ReadOnly, "filesystem info")
	}
	// Queue four independent slots before the owner starts admitting this batch.
	if os.Args[2] == "goroutines" {
		volume.barrier = make(chan struct{})
		volume.gate.Store(true)
	}
	replies := batch([]string{"overlap-hello", "overlap-sparse", "overlap-fragmented", "overlap-unwritten"}, []fsproto.Request{{Operation: fsproto.OperationRead, Node: hello.Node, Size: 11, Slot: 0}, {Operation: fsproto.OperationRead, Node: sparse.Node, Size: 16, Slot: 1}, {Operation: fsproto.OperationRead, Node: fragmented.Node, Size: 16, Slot: 2}, {Operation: fsproto.OperationRead, Node: empty.Node, Offset: 12288, Size: 16, Slot: 3}})
	for _, r := range replies {
		require(r.Status == ramfs.StatusOK, "independent file read")
	}
	volume.gate.Store(false)
	if os.Args[2] == "goroutines" {
		require(volume.arrived.Load() == 4, "four worker admissions")
	}
	read("beginning", hello.Node, 0, 5, []byte("hello"))
	read("middle", hello.Node, 3, 5, []byte("lo ex"))
	read("end", hello.Node, 9, 10, []byte("4\n"))
	read("eof", hello.Node, 11, 32, []byte{})
	read("past-eof", hello.Node, 100, 32, []byte{})
	read("sparse-hole", sparse.Node, 4096, 4096, make([]byte, 4096))
	read("sparse-data-boundary", sparse.Node, 1048575, 3, []byte{0, 'B', 0})
	read("sparse-tail", sparse.Node, 5368709120, 10, []byte{'C'})
	read("unwritten", empty.Node, 12288, 4096, make([]byte, 4096))
	stale := one("stale-inode", fsproto.Request{Operation: fsproto.OperationStat, Node: hello.Node ^ (1 << 40)})
	require(stale.Status == ramfs.StatusNotFound, "stale inode status")
	host.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		panic("service shutdown timeout")
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Label < records[j].Label })
	encoder := json.NewEncoder(os.Stdout)
	for _, r := range records {
		if err := encoder.Encode(r); err != nil {
			panic(err)
		}
	}
}
