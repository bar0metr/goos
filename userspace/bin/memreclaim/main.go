//go:build goos_userspace

// Command memreclaim checks the reclaimable heap from inside the running
// system (ADR 0201). Without an argument it checks that roots on the stack,
// in globals, and in regions keep their objects, that interior pointers keep
// the allocation they point into, that unreachable cycles and deep graphs are
// collected, that reused memory reads as zero, that explicit frees are reused,
// that collected pages go back to the kernel, and that a thousand allocation
// cycles settle. "service COUNT" drives COUNT filesystem requests and samples
// the VFS service's pages; "burst" measures the pages a short burst leaves
// before and after the process goes idle; "workload ROUNDS [RETAINED]" times
// a mixed workload, with RETAINED free pages kept after each collection, and
// prints what the allocator did for it; "double-free" and "oom"
// commit a failure the process must not survive; "quota" checks retained
// pages and then grows the live heap past the page quota, which the kernel
// must stop. Every mode that finishes
// prints an ALLOC_PROFILE of its own heap. It is built only into validation
// images.
package main

import (
	"encoding/binary"
	"runtime"

	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/runtime/arena"
	"goos/userspace/startup"
	"goos/userspace/utility"
	"goos/userspace/wait"
)

var manifest = commands.Manifest{
	Usage:   "memreclaim [service N|burst|workload N [R]|quota|double-free|oom]",
	Summary: "heap reclamation checks",
	MaxArgs: 3,
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
		case utility.Equal(mode, "burst"):
			checkBurst()
		case utility.Equal(mode, "workload") && data.ArgCount() >= 2:
			rounds, _ := data.Arg(2)
			if data.ArgCount() == 3 {
				retained, _ := data.Arg(3)
				arena.SetRetained(uint32(min(parse(retained), 1<<20)))
			}
			runWorkload(parse(rounds))
		case utility.Equal(mode, "quota"):
			checkQuota()
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
	checkContainersKeepTargets()
	checkStackRoot()
	checkGlobalRoot()
	checkInteriorPointers()
	checkCycle()
	checkDeepAndWideGraphs()
	checkStackScrub()
	checkZeroOnReuse()
	checkBoundarySizes()
	checkClassReuse()
	checkExplicitFree()
	checkRegions()
	checkPageReturn()
	checkCycles()
	stats := arena.HeapStats()
	last, longest := arena.Pauses()
	report("MEMRECLAIM_STATS reserved=", stats.Reserved, " live=", stats.Live, " objects=", stats.Objects, " held=", stats.Held)
	report("MEMRECLAIM_STATS peak_live=", stats.PeakLive, " peak_held=", stats.PeakHeld, " mallocs=", stats.Mallocs, " frees=", stats.Frees)
	report("MEMRECLAIM_STATS collections=", uint64(stats.Collections), "", 0, "", 0, "", 0)
	report("MEMRECLAIM_PAUSE last_ns=", last, " longest_ns=", longest, " total_ns=", arena.PauseTotal(), "", 0)
	printProfile("memreclaim")
	marker("MEMRECLAIM_OK")
}

// profile and profileLine are globals so that printing the profile allocates
// nothing.
var (
	profile     arena.Diagnostics
	profileLine [160]byte
)

func printProfile(name string) {
	arena.ReadDiagnostics(&profile)
	profile.AppendProfile(name, profileLine[:], utility.Write)
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

// checkLayoutContract checks the layouts the compiler passes. Byte buffers
// and string bytes are allocated pointer-free, so the collector never reads
// them; anything that can hold a pointer, wherever the pointer sits, is
// scanned. A false "pointer-free" would free live objects, so the check covers
// the shapes that place a pointer away from the start of an object.
func checkLayoutContract() {
	small, large := allocate(64), allocate(5000)
	expect(arena.PointerFree(&small[0]) && arena.PointerFree(&large[0]), "byte buffers are pointer-free")
	expect(arena.PointerFreeString(runtimeString()), "string bytes are pointer-free")
	pointers := pointerSlice()
	value := newHolder()
	expect(arena.InHeap(&pointers[0]) && arena.InHeap(value), "pointer values come from the heap")
	expect(!arena.PointerFree(&pointers[0]) && !arena.PointerFree(value), "pointer values are scanned")
	shapes := newShapes()
	expect(!arena.PointerFree(shapes.offset) && !arena.PointerFree(shapes.nested) && !arena.PointerFree(shapes.array) &&
		!arena.PointerFree(shapes.tail) && !arena.PointerFree(shapes.iface) && !arena.PointerFree(&shapes.strings[0]),
		"pointers at any offset are scanned")
	marker("MEMRECLAIM_LAYOUT_OK")
}

//go:noinline
func runtimeString() string { return string(allocate(40)) + "-built" }

type offsetHolder struct {
	a, b, c, d, e uint64
	p             *page
}

type innerHolder struct {
	x uint64
	p *page
}

type nestedHolder struct {
	n  [3]uint64
	in innerHolder
}

// tailHolder is larger than a slab slot, so it is a page run with its only
// pointer at the end.
type tailHolder struct {
	pad [600]uint64
	p   *page
}

type ifaceHolder struct {
	pad uint64
	v   any
}

// shapes holds one target object through each kind of container. Each target
// is reachable only through its container.
type shapes struct {
	offset   *offsetHolder
	nested   *nestedHolder
	array    *[8]*page
	tail     *tailHolder
	iface    *ifaceHolder
	strings  []string
	mapValue map[int]*page
	mapKey   map[*page]int
	channel  chan *page
	closure  func() *page
}

var heldShapes *shapes

//go:noinline
func newShapes() *shapes {
	s := &shapes{
		offset: &offsetHolder{p: newPage(101)}, nested: &nestedHolder{in: innerHolder{p: newPage(102)}},
		array: new([8]*page), tail: &tailHolder{p: newPage(104)}, iface: &ifaceHolder{v: newPage(105)},
		strings: []string{runtimeString(), runtimeString()}, mapValue: map[int]*page{}, mapKey: map[*page]int{},
		channel: make(chan *page, 4),
	}
	s.array[5] = newPage(103)
	s.mapValue[7] = newPage(106)
	s.mapKey[newPage(107)] = 1
	s.channel <- newPage(108)
	captured := newPage(109)
	s.closure = func() *page { return captured }
	return s
}

//go:noinline
func storeShapes() { heldShapes = newShapes() }

//go:noinline
func shapesHold() bool {
	s := heldShapes
	iface, _ := s.iface.v.(*page)
	keyed := false
	for key := range s.mapKey {
		keyed = key.holds(107)
	}
	sent := <-s.channel
	s.channel <- sent
	return s.offset.p.holds(101) && s.nested.in.p.holds(102) && s.array[5].holds(103) && s.tail.p.holds(104) &&
		iface != nil && iface.holds(105) && s.mapValue[7].holds(106) && keyed && sent.holds(108) &&
		s.closure().holds(109) && len(s.strings[0]) == 46
}

// checkContainersKeepTargets keeps each target only inside one container
// across collections that reuse every freed page.
func checkContainersKeepTargets() {
	storeShapes()
	for round := 0; round < 3; round++ {
		collect()
		churn(64, 4096, 4800)
	}
	expect(shapesHold(), "objects reachable through offsets, nesting, arrays, interfaces, maps, channels, and closures survive")
	heldShapes = nil
	marker("MEMRECLAIM_CONTAINERS_OK")
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
		explainSurvivor()
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

//go:noinline
func cycleToken() uint64 { return arena.Token(cycle) }

// explainSurvivor prints where the collector found the word that kept the
// watched allocation, when a check finds an object that should be gone.
func explainSurvivor() {
	phase, at, below, above := arena.Watched()
	report("MEMRECLAIM_DEBUG phase=", uint64(phase), " at=", uint64(at), " below=", uint64(below), " above=", uint64(above))
}

func checkCycle() {
	makeCycle()
	collect()
	held := live()
	expect(cycleSurvives(), "rooted cycle survives")
	arena.Watch(cycleToken())
	cycle = nil
	collect()
	if held < live()+3*64<<10 {
		explainSurvivor()
	}
	expect(held >= live()+3*64<<10, "unreachable cycle collected")
	marker("MEMRECLAIM_CYCLE_OK")
}

// scrubRooms are the stack room below a frame at which checkStackScrub
// collects. The last is less than the 4 KiB the collector zeroes below
// itself, so the zeroing must stop at the bottom of the stack: past it lies
// the guard, and a write there would stop the process.
var scrubRooms = [...]uintptr{16 << 10, 8 << 10, 3 << 10}

var (
	scrubNext    int
	scrubDeepest int
	scrubRoom    uintptr
)

// deepCollect recurses to the bottom of the stack with one live object per
// frame, held only in the frame or a register, and collects on the way down
// at each of scrubRooms. It reports whether every frame's object kept its
// value.
//
//go:noinline
func deepCollect(depth int) bool {
	held := newBlock(uint64(depth))
	room := arena.StackRoom()
	if scrubNext < len(scrubRooms) && room <= scrubRooms[scrubNext] {
		scrubNext++
		scrubDeepest, scrubRoom = depth, room
		for pass := 0; pass < 2; pass++ {
			collect()
			churn(8, 64, 4096, 70000)
		}
	}
	ok := true
	if scrubNext < len(scrubRooms) {
		ok = deepCollect(depth + 1)
	}
	return ok && held.holds(uint64(depth))
}

// deepGarbage allocates 256 KiB in a frame depth calls down and drops it, so
// the only copy of its address lies in stack below the caller. The addition
// after the call keeps the recursion from becoming a loop in one frame.
//
//go:noinline
func deepGarbage(depth int) int {
	if depth > 0 {
		return deepGarbage(depth-1) + 1
	}
	scratch := allocate(256 << 10)
	scratch[0] = 1
	return 0
}

// checkStackScrub collects with frames down to 3 KiB above the bottom of the
// stack and requires every frame's object to survive, then drops an object
// whose address only dead frames held and requires a collection from above
// them to free it: the collector's own frames, built over those, must not
// keep it, at 48 depths from 6 to 100 frames down. The frames that call the
// collector are not zeroed, and a stale word in them is the false retention
// a conservative scan accepts; where such a word lands depends on how the
// compiler laid the frames out. One depth in ten may be kept that way.
func checkStackScrub() {
	scrubNext = 0
	expect(arena.StackRoom() > 64<<10, "the stack has room to recurse")
	expect(deepCollect(0), "objects held by frames down to the bottom of the stack survive")
	expect(scrubNext == len(scrubRooms), "the recursion reached the bottom of the stack")
	report("MEMRECLAIM_STACK_SCRUB depth=", uint64(scrubDeepest), " room=", uint64(scrubRoom), "", 0, "", 0)
	kept, tried := uint64(0), uint64(0)
	for depth := 6; depth <= 100; depth += 2 {
		collect()
		expect(deepGarbage(depth) == depth, "dead-frame recursion")
		held := live()
		collect()
		tried++
		if held < live()+256<<10 {
			kept++
			report("MEMRECLAIM_STACK_KEPT depth=", uint64(depth), "", 0, "", 0, "", 0)
		}
	}
	report("MEMRECLAIM_STACK_STALE tried=", tried, " kept=", kept, "", 0, "", 0)
	expect(kept*10 <= tried, "objects only dead frames held are collected")
	marker("MEMRECLAIM_STACK_SCRUB_OK")
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

// classProbes are the slot sizes of the heap's classes. checkClassReuse
// allocates pointer slices of each size and of a word less and more.
var classProbes = [...]int{16, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1360, 2048}

// classHeld keeps the slices: the first element of even ones, so the
// allocation is held from its start, and the last element of odd ones, so
// only an interior pointer holds it.
var classHeld [3 * len(classProbes)]**block

//go:noinline
func buildClassObjects(round int) {
	for index, size := range classProbes {
		for offset, words := range [...]int{size/8 - 1, size / 8, size/8 + 1} {
			slot := 3*index + offset
			pointers := make([]*block, max(words, 1))
			for element := range pointers {
				pointers[element] = newBlock(uint64(round<<24 | slot<<12 | element))
			}
			if slot%2 == 0 {
				classHeld[slot] = &pointers[0]
			} else {
				classHeld[slot] = &pointers[len(pointers)-1]
			}
		}
	}
}

//go:noinline
func classObjectsHold(round int) bool {
	for index, size := range classProbes {
		for offset, words := range [...]int{size/8 - 1, size / 8, size/8 + 1} {
			slot := 3*index + offset
			element := 0
			if slot%2 != 0 {
				element = max(words, 1) - 1
			}
			if !(*classHeld[slot]).holds(uint64(round<<24 | slot<<12 | element)) {
				return false
			}
		}
	}
	return true
}

// checkClassReuse keeps pointer slices of every class size, held from their
// start or only from inside, across three collections with churn of every
// class between them, three rounds over.
func checkClassReuse() {
	for round := 0; round < 3; round++ {
		buildClassObjects(round)
		for pass := 0; pass < 3; pass++ {
			collect()
			for _, size := range classProbes {
				churn(16, size-8, size, size+8)
			}
		}
		expect(classObjectsHold(round), "objects of every class keep their pointers and bytes")
	}
	classHeld = [len(classHeld)]**block{}
	marker("MEMRECLAIM_CLASSES_OK")
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

// releasedRegionReturnsPages touches 256 KiB of a fresh region and requires
// Release to give all of it back.
//
//go:noinline
func releasedRegionReturnsPages() bool {
	before := pagesUsed()
	region := arena.NewRegion(256 << 10)
	scope := region.Enter()
	for index := 0; index < 60; index++ {
		data := allocate(4000)
		data[0] = 1
	}
	scope.Leave()
	touched := pagesUsed()
	region.Release()
	after := pagesUsed()
	return touched >= before+58 && after <= before+2
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
	region.Release()
	expect(releasedRegionReturnsPages(), "a released region returns its pages")
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
	report("MEMRECLAIM_CYCLES live=", stats.Live, " objects=", stats.Objects, " held=", stats.Held, " pages=", pages)
	expect(stats.Objects <= settled.Objects+16 && stats.Live <= settled.Live+64<<10, "live heap settles")
	expect(stats.Metadata == settled.Metadata && pages <= settledPages+16, "held memory settles")
	marker("MEMRECLAIM_1000_CYCLES_OK")
	automatic := stats.Collections
	for round := 0; round < 3000; round++ {
		graph(round)
	}
	stats, pages = arena.HeapStats(), pagesUsed()
	report("MEMRECLAIM_AUTO collections=", uint64(stats.Collections-automatic), " held=", stats.Held, " pages=", pages, "", 0)
	expect(stats.Collections-automatic >= 20, "allocation starts collections")
	expect(stats.Held <= settled.Held+4<<20 && pages <= settledPages+1024+16, "automatic collection bounds the heap")
	marker("MEMRECLAIM_AUTO_GC_OK")
}

// checkService drives count filesystem requests through the VFS service and
// samples the pages it holds ten times, each time after the service has been
// idle long enough to collect and give its free pages back. A sample taken
// while requests run lands somewhere in a collection cycle, in which the
// service's pages swing by up to 200 as garbage grows to the next threshold,
// so only idle samples can show whether its memory stays in a band.
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
			busy := vfsPages()
			wait.Sleep(idleTicks)
			pages := vfsPages()
			report("MEMRECLAIM_SERVICE requests=", (done+1)*3, " vfs_pages=", pages, " busy_pages=", busy, "", 0)
			switch sample := (done + 1) / (count / 10); {
			case sample <= 2:
				// The first samples set the band: caches fill and the
				// service settles.
				first = max(first, pages)
			default:
				highest = max(highest, pages)
			}
		}
	}
	expect(first != 0 && highest <= first+32, "service memory stays in its band")
	printProfile("service")
	marker("MEMRECLAIM_SERVICE_OK")
}

// idleTicks is long enough for wait.Sleep to treat the process as idle.
const idleTicks = clock.TicksPerSecond / 2

// burstGarbage allocates about 768 KiB of mixed objects and drops them all.
// That stays below the growth that starts a collection, so the pages it
// takes are still held when it returns.
//
//go:noinline
func burstGarbage() {
	for round := 0; round < 4; round++ {
		graph(round)
		for index := 0; index < 8; index++ {
			data := allocate(16 << 10)
			for offset := 0; offset < len(data); offset += 4096 {
				data[offset] = 1
			}
		}
	}
}

// checkBurst measures the pages a short burst of garbage leaves before and
// after the process goes idle. An idle wait must leave no free page undiscarded
// and, as the page test requires of a collection, keep at most an eighth of
// what the burst took: metadata up to the heap's highest page and page tables
// stay by design (ADR 0201), and a stale word in a frame may keep a few
// objects. A second idle wait with nothing allocated in between may add at
// most 2 pages.
func checkBurst() {
	collect()
	before := pagesUsed()
	arena.ReadDiagnostics(&profile)
	metaBefore, inUseBefore := profile.MetadataPages, arena.HeapStats().InUse
	burstGarbage()
	peak := pagesUsed()
	wait.Sleep(idleTicks)
	idle := pagesUsed()
	wait.Sleep(idleTicks)
	quiet := pagesUsed()
	arena.ReadDiagnostics(&profile)
	report("MEMRECLAIM_BURST before=", before, " peak=", peak, " idle=", idle, " quiet=", quiet)
	inUse := arena.HeapStats().InUse
	report("MEMRECLAIM_BURST_HEAP meta_before=", metaBefore, " meta_after=", profile.MetadataPages, " inuse_before=", inUseBefore, " inuse_idle=", inUse)
	expect(peak > before+64, "the burst touched pages")
	expect(profile.DirtyFreePages == 0, "an idle process keeps no free page")
	expect(idle <= before+(peak-before)/8 && quiet <= idle+2, "an idle process gives its free pages back")
	printProfile("burst")
	marker("MEMRECLAIM_BURST_OK")
}

// runWorkload times rounds of the mixed graph workload with collection left
// to the allocation policy, then reports what the allocator did for it.
// Times are guest clock nanoseconds and under QEMU TCG measure the emulator
// as much as the allocator.
func runWorkload(rounds uint64) {
	expect(rounds >= 1, "workload round count")
	collect()
	start, startPages := utility.ClockNanos(), pagesUsed()
	highest := startPages
	for round := uint64(0); round < rounds; round++ {
		graph(int(round))
		if round%64 == 0 {
			highest = max(highest, pagesUsed())
		}
	}
	elapsed := utility.ClockNanos() - start
	stats := arena.HeapStats()
	arena.ReadDiagnostics(&profile)
	retained := uint64(profile.Retained)
	report("MEMRECLAIM_WORKLOAD retained=", retained, " rounds=", rounds, " ns=", elapsed, " collections=", uint64(stats.Collections))
	report("MEMRECLAIM_WORKLOAD_PAGES retained=", retained, " start=", startPages, " highest=", highest, " end=", pagesUsed())
	report("MEMRECLAIM_WORKLOAD_POLICY retained=", retained, " discards=", profile.DiscardCalls, " recommitted=", profile.PagesRecommitted, " cleared=", profile.ClearedBytes)
	report("MEMRECLAIM_WORKLOAD_GC retained=", retained, " pause_ns=", arena.PauseTotal(), " peak_held=", stats.PeakHeld, "", 0)
	printProfile("workload")
	marker("MEMRECLAIM_WORKLOAD_OK")
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

// pageLimit is the page quota of this process, found the way pagesUsed finds
// its count: as the process ProcessInfo reports running, which holds only
// while one processor runs processes, as in the memory gate's guest.
func pageLimit() uint64 {
	record := &processRecord
	for index := uint64(0); utility.ProcessInfo(index, record); index++ {
		if record[8] == kcall.ProcessRunning {
			return uint64(binary.LittleEndian.Uint32(record[kcall.ProcessInfoLimitsOffset+kcall.ProcessPages*4:]))
		}
	}
	return 0
}

// quotaBuffers holds the 64 KiB buffers checkQuota keeps alive.
var quotaBuffers [][]byte

//go:noinline
func touchBuffers(count int) {
	for index := 0; index < count; index++ {
		data := allocate(64 << 10)
		for offset := range data {
			data[offset] = 0x5c
		}
		quotaBuffers = append(quotaBuffers, data)
	}
}

// dropBuffers lets go of the buffers from index first on.
//
//go:noinline
func dropBuffers(first int) {
	for index := first; index < len(quotaBuffers); index++ {
		quotaBuffers[index] = nil
	}
	quotaBuffers = quotaBuffers[:first]
}

// checkQuota retains 256 free pages and checks what retention promises:
// reuse of the retained pages, cleared, without the kernel supplying them
// again, and an idle wait that gives them all back. Then it keeps 64 KiB
// buffers alive until the process is past its page quota. Retention does
// not change that ending: the kernel stops the process when it touches a
// page past the quota, after the allocator has returned, and no trim can run
// then. The memory gate requires that fault and nothing after it.
func checkQuota() {
	arena.SetRetained(256)
	limit := pageLimit()
	quotaBuffers = make([][]byte, 0, 1024)
	touchBuffers(64)
	dropBuffers(16)
	collect()
	arena.ReadDiagnostics(&profile)
	retained, recommitted, cleared := uint64(profile.DirtyFreePages), profile.PagesRecommitted, profile.ClearedBytes
	report("MEMRECLAIM_QUOTA limit=", limit, " pages=", pagesUsed(), " retained=", retained, "", 0)
	expect(limit != 0 && retained == 256, "a collection retains the pages it may")

	// The retained pages serve the next 256 pages of buffers, cleared.
	first := len(quotaBuffers)
	for index := 0; index < 16; index++ {
		data := allocate(64 << 10)
		expect(zeroed(data), "a retained page is cleared before reuse")
		quotaBuffers = append(quotaBuffers, data)
	}
	arena.ReadDiagnostics(&profile)
	expect(profile.PagesRecommitted == recommitted && profile.ClearedBytes >= cleared+16*(64<<10), "allocations reuse the retained pages")
	dropBuffers(first)

	// An idle wait gives back the retained pages without a new allocation.
	collect()
	busy := pagesUsed()
	wait.Sleep(idleTicks)
	idle := pagesUsed()
	arena.ReadDiagnostics(&profile)
	report("MEMRECLAIM_QUOTA_IDLE busy=", busy, " idle=", idle, " retained=", uint64(profile.DirtyFreePages), " trims=", profile.Trims)
	expect(profile.DirtyFreePages == 0 && profile.Trims != 0 && idle+240 <= busy, "an idle wait gives the retained pages back")
	marker("MEMRECLAIM_QUOTA_RETAIN_OK")

	// Past the quota the kernel stops the process.
	marker("MEMRECLAIM_QUOTA_ATTEMPT")
	for len(quotaBuffers) < cap(quotaBuffers) {
		touchBuffers(1)
	}
	marker("MEMRECLAIM_QUOTA_MISSED")
	utility.Exit(1)
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
