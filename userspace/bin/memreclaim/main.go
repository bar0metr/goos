//go:build goos_userspace

// Command memreclaim checks the reclaimable heap from inside the running
// system (ADR 0201). Without an argument it checks that roots on the stack,
// in globals, and in regions keep their objects, that interior pointers keep
// the allocation they point into, that unreachable cycles and deep graphs are
// collected, that reused memory reads as zero, that explicit frees are reused,
// that collected pages go back to the kernel, and that a thousand allocation
// cycles settle. "service COUNT" drives COUNT filesystem requests and samples
// the VFS service's pages; "double-free" and "oom" commit a failure the
// process must not survive. It is built only into validation images.
package main

import (
	"encoding/binary"
	"runtime"

	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/runtime/arena"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "memreclaim [service COUNT|double-free|oom]",
	Summary: "heap reclamation checks",
	MaxArgs: 2,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	if data.ArgCount() >= 1 {
		mode, _ := data.Arg(1)
		switch {
		case utility.Equal(mode, "service") && data.ArgCount() == 2:
			count, _ := data.Arg(2)
			checkService(parse(count))
		case utility.Equal(mode, "double-free"):
			doubleFree()
		case utility.Equal(mode, "oom"):
			exhaust()
		default:
			utility.Usage()
		}
		return
	}
	checkLayoutContract()
	checkStackRoot()
	checkGlobalRoot()
	checkInteriorPointers()
	checkCycle()
	checkDeepAndWideGraphs()
	checkZeroOnReuse()
	checkBoundarySizes()
	checkExplicitFree()
	checkRegions()
	checkPageReturn()
	checkCycles()
	stats := arena.HeapStats()
	last, longest := arena.Pauses()
	report("MEMRECLAIM_STATS reserved=", stats.Reserved, " live=", stats.Live, " objects=", stats.Objects, " committed=", stats.Committed)
	report("MEMRECLAIM_STATS peak_live=", stats.PeakLive, " peak_committed=", stats.PeakCommitted, " mallocs=", stats.Mallocs, " frees=", stats.Frees)
	report("MEMRECLAIM_STATS collections=", uint64(stats.Collections), "", 0, "", 0, "", 0)
	report("MEMRECLAIM_PAUSE last_ns=", last, " longest_ns=", longest, "", 0, "", 0)
	marker("MEMRECLAIM_OK")
}

// sink makes an allocation escape, so it is a heap allocation rather than a
// stack one.
var sink []byte

//go:noinline
func allocate(size int) []byte {
	data := make([]byte, size)
	sink = data
	sink = nil
	return data
}

// collect runs a collection. Objects a check expects to be collected were
// only ever held by frames that have returned, which lie below the stack
// pointer and are not scanned.
func collect() { runtime.GC() }

func live() uint64 { return arena.HeapStats().Live }

func expect(ok bool, what string) {
	if !ok {
		utility.PrintText("memreclaim: ")
		utility.PrintText(what)
		utility.PrintText(" failed")
		utility.EndLine()
		utility.Exit(1)
	}
}

func marker(text string) {
	utility.PrintText(text)
	utility.EndLine()
}

func report(first string, a uint64, second string, b uint64, third string, c uint64, fourth string, d uint64) {
	utility.PrintText(first)
	utility.PrintUint(a)
	for _, pair := range [...]struct {
		label string
		value uint64
	}{{second, b}, {third, c}, {fourth, d}} {
		if pair.label != "" {
			utility.PrintText(pair.label)
			utility.PrintUint(pair.value)
		}
	}
	utility.EndLine()
}

// churn allocates and drops count objects of each given size, so a slot or
// page freed by mistake would be handed out and overwritten.
//
//go:noinline
func churn(count int, sizes ...int) {
	for index := 0; index < count; index++ {
		for _, size := range sizes {
			data := allocate(size)
			for offset := 0; offset < len(data); offset += 64 {
				data[offset] = 0xee
			}
		}
	}
}

type block struct {
	words [8]uint64
}

type page struct {
	words [512]uint64
}

func (b *block) fill(seed uint64) {
	for index := range b.words {
		b.words[index] = seed + uint64(index)
	}
}

func (b *block) holds(seed uint64) bool {
	for index := range b.words {
		if b.words[index] != seed+uint64(index) {
			return false
		}
	}
	return true
}

func (p *page) fill(seed uint64) {
	for index := range p.words {
		p.words[index] = seed ^ uint64(index)
	}
}

func (p *page) holds(seed uint64) bool {
	for index := range p.words {
		if p.words[index] != seed^uint64(index) {
			return false
		}
	}
	return true
}

//go:noinline
func newBlock(seed uint64) *block {
	b := new(block)
	b.fill(seed)
	return b
}

//go:noinline
func newPage(seed uint64) *page {
	p := new(page)
	p.fill(seed)
	return p
}

//go:noinline
func pointerSlice() []*block { return make([]*block, 600) }

//go:noinline
func newHolder() *holder { return &holder{} }

// checkLayoutContract checks the layouts the compiler passes: byte buffers
// are allocated pointer-free, so the collector never reads them, and
// anything that holds a pointer is scanned.
func checkLayoutContract() {
	small, large := allocate(64), allocate(5000)
	pointers := pointerSlice()
	expect(arena.PointerFree(&small[0]) && arena.PointerFree(&large[0]), "byte buffers are pointer-free")
	value := newHolder()
	expect(arena.InHeap(&pointers[0]) && arena.InHeap(value), "pointer values come from the heap")
	expect(!arena.PointerFree(&pointers[0]) && !arena.PointerFree(value), "pointer values are scanned")
	marker("MEMRECLAIM_LAYOUT_OK")
}

// checkStackRoot keeps two objects only in this frame across a collection
// and then hands their sizes out again: a swept object would be overwritten.
//
//go:noinline
func checkStackRoot() {
	small, large := newBlock(0x51), newPage(0x52)
	expect(arena.InHeap(small) && arena.InHeap(large), "stack objects come from the heap")
	churn(500, 64, 4096)
	collect()
	churn(500, 64, 4096)
	expect(small.holds(0x51) && large.holds(0x52), "stack root")
	runtime.KeepAlive(small)
	runtime.KeepAlive(large)
	marker("MEMRECLAIM_STACK_ROOT_OK")
}

var globalPages *[64]page

//go:noinline
func storeGlobal() {
	pages := new([64]page)
	for index := range pages {
		pages[index].fill(uint64(index) << 20)
	}
	globalPages = pages
}

// The checks that expect an object to be collected read it only in helpers
// like this one. A helper's frame is below the stack pointer once it returns,
// so no copy of the pointer is left where the collector scans.
//
//go:noinline
func globalHolds() bool {
	for index := range globalPages {
		if !globalPages[index].holds(uint64(index) << 20) {
			return false
		}
	}
	return true
}

//go:noinline
func globalToken() uint64 { return arena.Token(globalPages) }

func checkGlobalRoot() {
	storeGlobal()
	collect()
	churn(8, 256<<10)
	collect()
	expect(globalHolds(), "global root")
	held := live()
	arena.Watch(globalToken())
	globalPages = nil
	collect()
	if held < live()+256<<10 {
		phase, at, below, above := arena.Watched()
		report("MEMRECLAIM_DEBUG phase=", uint64(phase), " at=", uint64(at), " below=", uint64(below), " above=", uint64(above))
	}
	expect(held >= live()+256<<10, "a cleared global releases its object")
	marker("MEMRECLAIM_GLOBAL_ROOT_OK")
}

//go:noinline
func interiorSlice() []uint64 {
	words := make([]uint64, 16<<10)
	for index := range words {
		words[index] = uint64(index) * 3
	}
	return words[5000:5008]
}

type pair struct {
	header [5]uint64
	field  uint64
}

//go:noinline
func interiorField() *uint64 {
	p := new(pair)
	p.field = 0xfeed
	return &p.field
}

func checkInteriorPointers() {
	words := interiorSlice()
	field := interiorField()
	collect()
	churn(64, 48, 128<<10)
	collect()
	for index, value := range words {
		expect(value == uint64(5000+index)*3, "interior slice")
	}
	expect(*field == 0xfeed, "interior field")
	runtime.KeepAlive(words)
	runtime.KeepAlive(field)
	marker("MEMRECLAIM_INTERIOR_ROOT_OK")
}

type ring struct {
	next    *ring
	payload [8191]uint64
}

var cycle *ring

//go:noinline
func makeCycle() {
	a, b, c := new(ring), new(ring), new(ring)
	a.next, b.next, c.next = b, c, a
	cycle = a
}

//go:noinline
func cycleSurvives() bool { return cycle.next.next.next == cycle }

func checkCycle() {
	makeCycle()
	collect()
	held := live()
	expect(cycleSurvives(), "rooted cycle survives")
	cycle = nil
	collect()
	expect(held >= live()+3*64<<10, "unreachable cycle collected")
	marker("MEMRECLAIM_CYCLE_OK")
}

type link struct {
	next  *link
	value uint64
}

// deepLength is far deeper than a recursive marker could go on a 1 MiB stack.
const deepLength = 300000

// wideCount exceeds the mark stack, so marking takes the overflow path.
const wideCount = 100000

var (
	deep *link
	wide []*link
)

//go:noinline
func buildGraphs() {
	var head *link
	for index := 0; index < deepLength; index++ {
		head = &link{next: head, value: uint64(index)}
	}
	deep = head
	wide = make([]*link, wideCount)
	for index := range wide {
		wide[index] = &link{value: uint64(index) * 7}
	}
}

//go:noinline
func deepHolds() bool {
	count, expected := 0, uint64(deepLength-1)
	for node := deep; node != nil; node = node.next {
		if node.value != expected {
			return false
		}
		expected--
		count++
	}
	return count == deepLength
}

//go:noinline
func wideHolds() bool {
	for index, node := range wide {
		if node.value != uint64(index)*7 {
			return false
		}
	}
	return len(wide) == wideCount
}

func checkDeepAndWideGraphs() {
	before := live()
	buildGraphs()
	collect()
	churn(2000, 16)
	expect(deepHolds(), "deep graph")
	expect(wideHolds(), "wide graph")
	deep, wide = nil, nil
	collect()
	expect(live() <= before+64<<10, "deep and wide graphs collected")
	marker("MEMRECLAIM_DEEP_GRAPH_OK")
}

//go:noinline
func dirty(count, size int) {
	for index := 0; index < count; index++ {
		data := allocate(size)
		for offset := range data {
			data[offset] = 0xff
		}
	}
}

func zeroed(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func checkZeroOnReuse() {
	sizes := [...]int{24, 200, 2000, 5000, 70000}
	for _, size := range sizes {
		dirty(64, size)
	}
	collect()
	for _, size := range sizes {
		for index := 0; index < 64; index++ {
			expect(zeroed(allocate(size)), "collected memory reads as zero")
		}
	}
	first := allocate(300)
	for index := range first {
		first[index] = 0xab
	}
	arena.FreeBytes(first)
	found := false
	for index := 0; index < 64; index++ {
		again := allocate(300)
		expect(zeroed(again), "freed memory reads as zero")
		found = found || &again[0] == &first[0]
	}
	expect(found, "a freed slot is handed out again")
	marker("MEMRECLAIM_ZERO_REUSE_OK")
}

func checkBoundarySizes() {
	sizes := [...]int{1, 15, 16, 17, 31, 32, 33, 64, 128, 512, 2048, 4095, 4096, 4097, 64 << 10, 1 << 20}
	var held [len(sizes)][]byte
	for index, size := range sizes {
		data := allocate(size)
		expect(len(data) == size && zeroed(data) && arena.InHeap(&data[0]), "boundary allocation")
		for offset := range data {
			data[offset] = byte(index + 1)
		}
		held[index] = data
	}
	churn(4, 16, 48, 4096)
	collect()
	for index, data := range held {
		for _, value := range data {
			expect(value == byte(index+1), "boundary allocations keep their bytes")
		}
	}
	marker("MEMRECLAIM_BOUNDARY_OK")
}

func checkExplicitFree() {
	// A fresh threshold keeps an automatic collection out of the counts.
	collect()
	arena.FreeBytes(nil)
	for _, size := range [...]int{100, 64 << 10} {
		before := arena.HeapStats()
		data := allocate(size)
		held := arena.HeapStats()
		arena.FreeBytes(data)
		after := arena.HeapStats()
		expect(after.Frees == before.Frees+1 && after.Objects == before.Objects, "explicit free is counted")
		allocate(size)
		again := arena.HeapStats()
		// The same slot or pages serve the new allocation: nothing new is
		// taken from the reservation.
		expect(again.InUse == held.InUse && again.Metadata == held.Metadata, "explicit free is reused")
	}
	marker("MEMRECLAIM_FREE_OK")
}

type holder struct {
	target *page
}

var regionHolder *holder

//go:noinline
func holdFromRegion(region *arena.Region) {
	target := newPage(0x77)
	scope := region.Enter()
	scratch := allocate(4000)
	expect(!arena.InHeap(&scratch[0]), "region allocation stays out of the heap")
	for index := range scratch {
		scratch[index] = 0xcc
	}
	before := arena.HeapStats().Collections
	runtime.GC()
	expect(arena.HeapStats().Collections == before, "no collection inside a region")
	regionHolder = &holder{target: target}
	expect(!arena.InHeap(regionHolder), "region value stays out of the heap")
	scope.Leave()
}

//go:noinline
func regionTargetHolds() bool { return regionHolder.target.holds(0x77) }

func checkRegions() {
	region := arena.NewRegion(64 << 10)
	holdFromRegion(region)
	collect()
	churn(64, 4096)
	expect(regionTargetHolds(), "a region value keeps its heap object")
	held := live()
	regionHolder = nil
	region.Clear()
	collect()
	expect(held >= live()+4096, "a cleared region releases its heap object")
	scope := region.Enter()
	fresh := allocate(8000)
	scope.Leave()
	expect(zeroed(fresh), "reused region memory reads as zero")
	marker("MEMRECLAIM_REGION_OK")
}

// pagesUsed is the page count the kernel charges this process.
func pagesUsed() uint64 {
	return processPages(func(record []byte) bool { return record[8] == kcall.ProcessRunning })
}

// processRecord is a global so that measuring allocates nothing.
var processRecord [kcall.ProcessInfoSize]byte

func processPages(match func(record []byte) bool) uint64 {
	record := &processRecord
	for index := uint64(0); utility.ProcessInfo(index, record); index++ {
		if match(record[:]) {
			return uint64(binary.LittleEndian.Uint32(record[kcall.ProcessInfoUsedOffset+kcall.ProcessPages*4:]))
		}
	}
	return 0
}

var touched [][]byte

//go:noinline
func touchHeap() {
	touched = make([][]byte, 128)
	for index := range touched {
		data := allocate(64 << 10)
		for offset := 0; offset < len(data); offset += 4096 {
			data[offset] = 1
		}
		touched[index] = data
	}
}

func checkPageReturn() {
	collect()
	before := pagesUsed()
	touchHeap()
	peak := pagesUsed()
	touched = nil
	collect()
	after := pagesUsed()
	report("MEMRECLAIM_PAGES_BEFORE=", before, "", 0, "", 0, "", 0)
	report("MEMRECLAIM_PAGES_PEAK=", peak, "", 0, "", 0, "", 0)
	report("MEMRECLAIM_PAGES_AFTER=", after, "", 0, "", 0, "", 0)
	// 8 MiB is 2048 pages. Page tables and metadata may stay.
	expect(peak >= before+2000, "touched heap is charged")
	expect(after <= before+(peak-before)/8, "collected pages go back to the kernel")
	marker("MEMRECLAIM_PAGES_OK")
}

type record struct {
	name     string
	children []*record
	data     []byte
}

// graph builds a mixed object graph of about 64 KiB and returns its size in
// records.
//
//go:noinline
func graph(seed int) int {
	root := &record{name: "root"}
	index := map[int]*record{}
	for child := 0; child < 64; child++ {
		r := &record{name: string(rune('a' + child%26)), data: allocate(16 + (seed+child)%900)}
		root.children = append(root.children, r)
		index[child] = r
		if child%8 == 0 {
			r.data = allocate(4096 + child)
		}
		if child > 0 {
			r.children = []*record{index[child-1], root}
		}
	}
	return len(index)
}

// checkCycles runs a thousand allocate, drop, and collect rounds, then three
// thousand that leave collection to the allocation policy, and requires the
// heap and the pages the process holds to stay where the early rounds put
// them.
func checkCycles() {
	var settled arena.Stats
	var settledPages uint64
	for round := 0; round < 1000; round++ {
		expect(graph(round) == 64, "graph round")
		collect()
		if round == 99 {
			settled, settledPages = arena.HeapStats(), pagesUsed()
		}
	}
	stats, pages := arena.HeapStats(), pagesUsed()
	report("MEMRECLAIM_CYCLES live=", stats.Live, " objects=", stats.Objects, " committed=", stats.Committed, " pages=", pages)
	expect(stats.Objects <= settled.Objects+16 && stats.Live <= settled.Live+64<<10, "live heap settles")
	expect(stats.Metadata == settled.Metadata && pages <= settledPages+16, "committed memory settles")
	marker("MEMRECLAIM_1000_CYCLES_OK")
	automatic := stats.Collections
	for round := 0; round < 3000; round++ {
		graph(round)
	}
	stats, pages = arena.HeapStats(), pagesUsed()
	report("MEMRECLAIM_AUTO collections=", uint64(stats.Collections-automatic), " committed=", stats.Committed, " pages=", pages, "", 0)
	expect(stats.Collections-automatic >= 20, "allocation starts collections")
	expect(stats.Committed <= settled.Committed+4<<20 && pages <= settledPages+1024+16, "automatic collection bounds the heap")
	marker("MEMRECLAIM_AUTO_GC_OK")
}

// checkService drives count filesystem requests through the VFS service and
// samples the pages it holds ten times.
func checkService(count uint64) {
	expect(count >= 100, "service request count")
	path, directory := []byte("/tmp/memreclaim"), []byte("/system/bin")
	vfsPages := func() uint64 {
		return processPages(func(record []byte) bool {
			name := record[16 : 16+kcall.MaxProcessName]
			return string(name[:3]) == "vfs" && name[3] == 0
		})
	}
	var first, highest uint64
	for done := uint64(0); done < count; done++ {
		created, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationCreate, Path: path})
		utility.Check(created, ok, path, true)
		listed, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationReadDir, Path: directory, Offset: done % 8})
		utility.Check(listed, ok, directory, false)
		removed, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
		utility.Check(removed, ok, path, true)
		if (done+1)%(count/10) == 0 {
			pages := vfsPages()
			report("MEMRECLAIM_SERVICE requests=", (done+1)*3, " vfs_pages=", pages, "", 0, "", 0)
			switch sample := (done + 1) / (count / 10); {
			case sample <= 2:
				// The first samples set the band: caches fill and the
				// heap reaches its first collection threshold.
				first = max(first, pages)
			default:
				highest = max(highest, pages)
			}
		}
	}
	expect(first != 0 && highest <= first+32, "service memory stays in its band")
	marker("MEMRECLAIM_SERVICE_OK")
}

func parse(text []byte) uint64 {
	value := uint64(0)
	for _, digit := range text {
		if digit < '0' || digit > '9' || value > 1<<40 {
			utility.Usage()
		}
		value = value*10 + uint64(digit-'0')
	}
	return value
}

// doubleFree frees one allocation twice. The heap must stop the process.
func doubleFree() {
	neighbour, data := allocate(64), allocate(64)
	arena.FreeBytes(data)
	marker("MEMRECLAIM_DOUBLE_FREE_ATTEMPT")
	arena.FreeBytes(data)
	runtime.KeepAlive(neighbour)
	marker("MEMRECLAIM_DOUBLE_FREE_MISSED")
	utility.Exit(1)
}

var (
	held        [][]byte
	seen        uint32
	exhaustedOK bool
)

// exhaust drops 64 MiB of garbage and then holds 1 MiB allocations until the
// heap is full. They are touched on one page each, so the 256 MiB reservation
// runs out long before the page quota. The garbage must be collected before
// exhaustion: the held total then exceeds what fits beside it.
func exhaust() {
	held = make([][]byte, 0, 512)
	for index := 0; index < 64; index++ {
		allocate(1 << 20)
	}
	arena.OnExhausted(func() {
		stats := arena.HeapStats()
		intact := true
		for _, data := range held {
			intact = intact && data[0] == 0x77
		}
		report("MEMRECLAIM_OOM held_mib=", uint64(len(held)), " collections=", uint64(stats.Collections), " live=", stats.Live, "", 0)
		if intact && stats.Collections > seen && len(held) >= 250 {
			marker("MEMRECLAIM_OOM_OK")
		}
		utility.Exit(3)
	})
	for {
		seen = arena.HeapStats().Collections
		data := allocate(1 << 20)
		data[0] = 0x77
		held = append(held, data)
	}
}
