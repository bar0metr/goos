package ext4

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crashDevice records every write and flush a mount makes, so a test can
// rebuild what the disk would hold after a power cut at any point. Writes
// before the last flush are durable; writes after it reach the disk in any
// subset, and one may be torn at a sector boundary. The superblock's block
// and the journal superblock's are written whole or not at all: Linux
// assumes the same of the superblock's two sectors.
type crashDevice struct {
	// whole lists the offsets of blocks that never tear.
	whole   map[uint64]bool
	base    []byte
	current []byte
	events  []crashEvent
	// failAfter, when nonzero, refuses every write and flush past that
	// many events, as a dead device would.
	failAfter int
}

type crashEvent struct {
	flush  bool
	offset uint64
	data   []byte
}

func newCrashDevice(image []byte) *crashDevice {
	device := &crashDevice{base: append([]byte(nil), image...), current: append([]byte(nil), image...)}
	// Every image here comes from the canonical fixture, whose journal
	// superblock is the first block of the journal inode.
	fs := ok(probe(&scratchDevice{data: device.base}))
	fs.beforeReplay = true
	if err := fs.open(); err != nil {
		panic(err)
	}
	log := ok(fs.loadLog())
	device.whole = map[uint64]bool{log.superBlock * uint64(fs.blockSize): true}
	return device
}

func (device *crashDevice) ReadAt(offset uint64, target []byte) bool {
	end := offset + uint64(len(target))
	if end < offset || end > uint64(len(device.current)) {
		return false
	}
	copy(target, device.current[offset:end])
	return true
}

func (device *crashDevice) WriteAt(offset uint64, data []byte) bool {
	end := offset + uint64(len(data))
	if end < offset || end > uint64(len(device.current)) {
		return false
	}
	if device.failAfter != 0 && len(device.events) >= device.failAfter {
		return false
	}
	device.events = append(device.events, crashEvent{offset: offset, data: append([]byte(nil), data...)})
	copy(device.current[offset:end], data)
	return true
}

func (device *crashDevice) Flush() bool {
	if device.failAfter != 0 && len(device.events) >= device.failAfter {
		return false
	}
	device.events = append(device.events, crashEvent{flush: true})
	return true
}

func (device *crashDevice) Capacity() uint64 { return uint64(len(device.current)) }

// imageAt returns the disk after a power cut once the first point events
// happened.
func (device *crashDevice) imageAt(point int, random *rand.Rand) []byte {
	image := append([]byte(nil), device.base...)
	lastFlush := -1
	for index := 0; index < point; index++ {
		if device.events[index].flush {
			lastFlush = index
		}
	}
	for index := 0; index < point; index++ {
		event := &device.events[index]
		if event.flush {
			continue
		}
		data := event.data
		if index > lastFlush {
			if random.Intn(2) == 0 {
				continue
			}
			if event.offset >= superblockOffset+superblockSize && !device.whole[event.offset] &&
				len(data) > 512 && random.Intn(4) == 0 {
				data = data[:512*(1+random.Intn(len(data)/512-1))]
			}
		}
		copy(image[event.offset:], data)
	}
	return image
}

// crashOp is one step of a crash workload. prefix, when set, names a file
// the step writes in several transactions with the data it writes, so a
// cut may leave any committed prefix of it.
type crashOp struct {
	name   string
	run    func(fs *FS) error
	prefix string
	data   []byte
}

func crashWorkload(t *testing.T) []crashOp {
	in := func(fs *FS, path string) *Inode { return resolve(t, fs, path) }
	write := func(path string, data []byte) func(fs *FS) error {
		return func(fs *FS) error {
			_, err := fs.WriteFile(in(fs, path), 0, data)
			return err
		}
	}
	create := func(dir string, name string) func(fs *FS) error {
		return func(fs *FS) error {
			_, err := fs.Create(in(fs, dir), []byte(name), ModeRegular|0o644, 0, 0)
			return err
		}
	}
	big := bytes.Repeat([]byte("ordered data must reach the disk first. "), 40000)[:1536<<10]
	ops := []crashOp{
		{name: "create", run: create("/", "a")},
		{name: "write", run: write("/a", bytes.Repeat([]byte{'a'}, 10<<10))},
		{name: "mkdir", run: func(fs *FS) error { _, err := fs.Mkdir(in(fs, "/"), []byte("d1"), 0o755, 0, 0); return err }},
		{name: "create in dir", run: create("/d1", "b")},
		{name: "write small", run: write("/d1/b", []byte("small file"))},
		{name: "rename across", run: func(fs *FS) error { return fs.Rename(in(fs, "/"), []byte("a"), in(fs, "/d1"), []byte("a2")) }},
		{name: "link", run: func(fs *FS) error { return fs.Link(in(fs, "/"), []byte("hard"), in(fs, "/d1/a2")) }},
		{name: "symlink", run: func(fs *FS) error { _, err := fs.Symlink(in(fs, "/"), []byte("s"), []byte("d1/a2"), 0, 0); return err }},
		{name: "overwrite", run: write("/hello.txt", []byte("HELLO"))},
		{name: "append", run: func(fs *FS) error {
			file := in(fs, "/hello.txt")
			_, err := fs.WriteFile(file, file.Size, []byte(" appended"))
			return err
		}},
		{name: "truncate many extents", run: func(fs *FS) error { return fs.Truncate(in(fs, "/fragmented"), 10*8192) }},
		{name: "create big", run: create("/", "bigfile")},
		{name: "large write", run: write("/bigfile", big), prefix: "./bigfile", data: big},
		{name: "unlink large", run: func(fs *FS) error { return fs.Unlink(in(fs, "/"), []byte("fragmented")) }},
		{name: "replace by rename", run: func(fs *FS) error { return fs.Rename(in(fs, "/d1"), []byte("b"), in(fs, "/"), []byte("hello.txt")) }},
		{name: "mkdir for rmdir", run: func(fs *FS) error { _, err := fs.Mkdir(in(fs, "/"), []byte("d2"), 0o755, 0, 0); return err }},
		{name: "rmdir", run: func(fs *FS) error { return fs.Rmdir(in(fs, "/"), []byte("d2")) }},
		{name: "unlink while pinned", run: func(fs *FS) error {
			file := in(fs, "/bigfile")
			if err := fs.Pin(file); err != nil {
				return err
			}
			return fs.Unlink(in(fs, "/"), []byte("bigfile"))
		}},
		{name: "write to unlinked", run: func(fs *FS) error {
			_, err := fs.WriteFile(ok(fs.orphanInode(fs.orphans[0])), 0, []byte("still open"))
			return err
		}},
		{name: "last close", run: func(fs *FS) error { return fs.Unpin(ok(fs.orphanInode(fs.orphans[0]))) }},
	}
	for index := 0; index < 30; index++ {
		ops = append(ops, crashOp{name: "create indexed", run: create("/big", fmt.Sprintf("crash-%d", index))})
	}
	for index := 0; index < 2000; index += 101 {
		name := fmt.Sprintf("file-%d", index)
		ops = append(ops, crashOp{name: "unlink indexed", run: func(fs *FS) error { return fs.Unlink(in(fs, "/big"), []byte(name)) }})
	}
	return ops
}

// crashBase is the canonical image without its 5 GiB sparse file, which
// would make every manifest read 5 GiB of zeros.
func crashBase(t *testing.T) []byte {
	fs, device := scratch(t, "canonical")
	if err := fs.Unlink(root(t, fs), []byte("sparse")); err != nil {
		t.Fatal(err)
	}
	if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	}
	return device.data
}

type crashRun struct {
	device *crashDevice
	// manifests[k] is the tree after the first k operations; ends[k] is
	// the event count when operation k returned (ends[0]: the mount).
	manifests []string
	ends      []int
	ops       []crashOp
}

func recordCrashRun(t *testing.T) *crashRun {
	run := &crashRun{device: newCrashDevice(crashBase(t)), ops: crashWorkload(t)}
	fs := ok(mountScratch(run.device, testClock()))
	run.manifests = append(run.manifests, volumeManifest(t, fs))
	run.ends = append(run.ends, len(run.device.events))
	for _, op := range run.ops {
		if err := op.run(fs); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
		run.manifests = append(run.manifests, volumeManifest(t, fs))
		run.ends = append(run.ends, len(run.device.events))
	}
	if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	}
	return run
}

// allowed lists the trees a cut at point may recover to: the tree after
// the last operation that returned, the one after the operation in
// flight, and for a multi-transaction write every committed prefix.
func (run *crashRun) allowed(point int) map[string]bool {
	done := 0
	for done+1 < len(run.ends) && run.ends[done+1] <= point {
		done++
	}
	result := map[string]bool{}
	if point < run.ends[0] {
		result[run.manifests[0]] = true
		return result
	}
	result[run.manifests[done]] = true
	if done+1 >= len(run.manifests) {
		return result
	}
	result[run.manifests[done+1]] = true
	op := run.ops[done]
	if op.prefix == "" {
		return result
	}
	lines := strings.Split(run.manifests[done], "\n")
	for at, line := range lines {
		if !strings.HasPrefix(line, "file\t"+op.prefix+"\t") {
			continue
		}
		for length := 0; length <= len(op.data); length += 4096 {
			sum := sha256.Sum256(op.data[:length])
			lines[at] = fmt.Sprintf("file\t%s\t%d\t%s", op.prefix, length, hex.EncodeToString(sum[:]))
			result[strings.Join(lines, "\n")] = true
		}
	}
	return result
}

// recoverAt rebuilds the disk after a cut and mounts it writable, which
// replays the journal and finishes the orphan list.
func recoverAt(t *testing.T, image []byte) (*scratchDevice, *FS) {
	device := &scratchDevice{data: image}
	fs, err := mountScratch(device, testClock())
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	return device, fs
}

// crashPoints picks the cuts to try: around every flush, the first and
// last events of each operation, and a sample in between.
func (run *crashRun) crashPoints(random *rand.Rand, extra int) []int {
	seen := map[int]bool{}
	var points []int
	add := func(point int) {
		if point >= 0 && point <= len(run.device.events) && !seen[point] {
			seen[point] = true
			points = append(points, point)
		}
	}
	for index, event := range run.device.events {
		if event.flush {
			add(index)
			add(index + 1)
		}
	}
	for _, end := range run.ends {
		add(end - 1)
		add(end + 1)
	}
	for count := 0; count < extra; count++ {
		add(random.Intn(len(run.device.events) + 1))
	}
	return points
}

func TestPowerCutRecovery(t *testing.T) {
	run := recordCrashRun(t)
	random := rand.New(rand.NewSource(1))
	points := run.crashPoints(random, 200)
	// The full sweep takes over a minute; test-ext4-stress runs it.
	if os.Getenv("GOOS_STORAGE_STRESS") == "" {
		random.Shuffle(len(points), func(a, b int) { points[a], points[b] = points[b], points[a] })
		points = points[:120]
	}
	t.Logf("%d events, %d operations, %d cuts", len(run.device.events), len(run.ops), len(points))
	for _, point := range points {
		image := run.device.imageAt(point, random)
		device, fs := recoverAt(t, image)
		allowed := run.allowed(point)
		manifest := volumeManifest(t, fs)
		if !allowed[manifest] {
			t.Fatalf("cut at event %d recovered a tree no operation sequence produces:\n%s", point, manifest)
		}
		if err := verifyVolume(device); err != nil {
			t.Fatalf("cut at event %d: %v", point, err)
		}
		if len(fs.orphans) != 0 || fs.sb.LastOrphan != 0 {
			t.Fatalf("cut at event %d left orphans %v", point, fs.orphans)
		}
	}
}

// TestPowerCutDuringReplay cuts power while recovery itself runs, then
// recovers again: replay must give the same volume however often it is
// interrupted.
func TestPowerCutDuringReplay(t *testing.T) {
	run := recordCrashRun(t)
	random := rand.New(rand.NewSource(2))
	for attempt := 0; attempt < 12; attempt++ {
		point := run.ends[2+random.Intn(len(run.ends)-3)] - 1 - random.Intn(3)
		image := run.device.imageAt(point, random)
		_, fs := recoverAt(t, append([]byte(nil), image...))
		want := volumeManifest(t, fs)
		if fs.Recovery().Transactions == 0 {
			continue
		}
		current := image
		for cut := 0; cut < 3; cut++ {
			replaying := newCrashDevice(current)
			if _, err := mountScratch(replaying, testClock()); err != nil {
				t.Fatalf("recovery: %v", err)
			}
			current = replaying.imageAt(random.Intn(len(replaying.events)+1), random)
		}
		device, fs := recoverAt(t, current)
		if got := volumeManifest(t, fs); got != want {
			t.Fatalf("interrupted replay of cut %d recovered a different tree", point)
		}
		checkVolume(t, device)
	}
}

// TestCommitOrder checks the order of one transaction's writes: file data
// and the log body, a flush, the commit block, a flush, then home
// metadata.
func TestCommitOrder(t *testing.T) {
	device := newCrashDevice(crashBase(t))
	fs := ok(mountScratch(device, testClock()))
	file := ok(fs.Create(root(t, fs), []byte("ordered"), ModeRegular|0o644, 0, 0))
	start := len(device.events)
	if _, err := fs.WriteFile(file, 0, bytes.Repeat([]byte{'x'}, 8192)); err != nil {
		t.Fatal(err)
	}
	events := device.events[start:]
	inLog := func(offset uint64) bool { return fs.log.inLog(offset / uint64(fs.blockSize)) }
	var stages []string
	for _, event := range events {
		stage := "home"
		switch {
		case event.flush:
			stage = "flush"
		case inLog(event.offset) && be32(event.data, 4) == jbd2Commit:
			stage = "commit"
		case inLog(event.offset):
			stage = "log"
		case !bytes.Equal(event.data[:8], []byte("xxxxxxxx")):
			stage = "meta"
		default:
			stage = "data"
		}
		if n := len(stages); n == 0 || stages[n-1] != stage {
			stages = append(stages, stage)
		}
	}
	got := strings.Join(stages, " ")
	if got != "data log flush commit flush meta" {
		t.Fatalf("commit wrote %s", got)
	}
}

// TestFailedFlushStopsMutation fails the device in the middle of a commit:
// the operation reports an I/O error, the volume refuses further work and
// the next mount recovers a consistent tree.
func TestFailedFlushStopsMutation(t *testing.T) {
	device := newCrashDevice(crashBase(t))
	fs := ok(mountScratch(device, testClock()))
	before := volumeManifest(t, fs)
	device.failAfter = len(device.events) + 3
	_, err := fs.Create(root(t, fs), []byte("lost"), ModeRegular|0o644, 0, 0)
	wantKind(t, err, KindIO)
	if fs.State() != StateFailed {
		t.Fatalf("state after a failed commit: %v", fs.State())
	}
	_, err = fs.Create(root(t, fs), []byte("after"), ModeRegular|0o644, 0, 0)
	wantKind(t, err, KindReadOnly)
	wantKind(t, fs.Unmount(), KindIO)
	recovered, again := recoverAt(t, append([]byte(nil), device.current...))
	if got := volumeManifest(t, again); got != before {
		t.Fatalf("recovered tree after a failed commit:\n%s", got)
	}
	checkVolume(t, recovered)
}

// TestCleanUnmountState checks the superblock and journal superblock a
// writable mount leaves: mounted, they say a replay is due; after
// Unmount they say the volume is clean and a read-only mount accepts it.
func TestCleanUnmountState(t *testing.T) {
	fs, device := scratch(t, "canonical")
	if _, err := Mount(&scratchDevice{data: device.data}, Options{}); KindOf(err) != KindNeedsRecovery {
		t.Fatalf("read-only mount of a mounted volume: %v", err)
	}
	ok(fs.Create(root(t, fs), []byte("kept"), ModeRegular|0o644, 0, 0))
	if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	}
	reread, err := Mount(device, Options{})
	if err != nil {
		t.Fatalf("read-only mount after unmount: %v", err)
	}
	if !reread.journal.Clean() || reread.sb.NeedsRecovery() || reread.sb.State&stateValid == 0 {
		t.Fatalf("unmount left journal %+v state %#x", reread.journal, reread.sb.State)
	}
	if reread.journal.Incompat&jbd2IncompatCsumV3 == 0 || reread.journal.Incompat&jbd2IncompatRevoke == 0 {
		t.Fatalf("journal features %#x", reread.journal.Incompat)
	}
	lookupInode(t, reread, root(t, reread), "kept")
}

// TestSyncEmptiesLog checks that Sync checkpoints: a cut right after it
// leaves nothing to replay.
func TestSyncEmptiesLog(t *testing.T) {
	device := newCrashDevice(crashBase(t))
	fs := ok(mountScratch(device, testClock()))
	ok(fs.Create(root(t, fs), []byte("synced"), ModeRegular|0o644, 0, 0))
	if err := fs.Sync(); err != nil {
		t.Fatal(err)
	}
	_, again := recoverAt(t, device.imageAt(len(device.events), rand.New(rand.NewSource(3))))
	if replay := again.Recovery(); replay.Transactions != 0 {
		t.Fatalf("replayed %+v after Sync", replay)
	}
	lookupInode(t, again, root(t, again), "synced")
}

// TestLogWrapsAndRevokes runs enough transactions to wrap the log several
// times while freeing and reusing metadata blocks, then cuts power at the
// end and checks the replay.
func TestLogWrapsAndRevokes(t *testing.T) {
	device := newCrashDevice(crashBase(t))
	fs := ok(mountScratch(device, testClock()))
	startSeq := fs.log.seq
	dir := ok(fs.Mkdir(root(t, fs), []byte("churn"), 0o755, 0, 0))
	for round := 0; round < 40; round++ {
		file := ok(fs.Create(dir, []byte(fmt.Sprintf("f%d", round)), ModeRegular|0o644, 0, 0))
		// Scattered writes give the file an extent leaf block. Unlinking
		// the previous file frees its leaf while its copy is still in the
		// log, and later files reuse the block.
		for index := 0; index < 200; index += 20 {
			if _, err := fs.WriteFile(file, uint64(index)*8192, []byte{byte(round)}); err != nil {
				t.Fatal(err)
			}
		}
		if round > 0 {
			if err := fs.Unlink(dir, []byte(fmt.Sprintf("f%d", round-1))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if fs.log.tailSeq == startSeq {
		t.Fatal("log never reclaimed")
	}
	want := volumeManifest(t, fs)
	revokes := 0
	for _, event := range device.events {
		if !event.flush && fs.log.inLog(event.offset/uint64(fs.blockSize)) && be32(event.data, 0) == jbd2Magic && be32(event.data, 4) == jbd2Revoke {
			revokes++
		}
	}
	if revokes == 0 {
		t.Fatal("no revoke blocks written")
	}
	random := rand.New(rand.NewSource(4))
	revoked := uint64(0)
	for attempt := 0; attempt < 6; attempt++ {
		recovered, again := recoverAt(t, device.imageAt(len(device.events), random))
		if got := volumeManifest(t, again); got != want {
			t.Fatal("replay after wrapping recovered a different tree")
		}
		checkVolume(t, recovered)
		revoked += again.Recovery().Revoked
	}
	if revoked == 0 {
		t.Fatal("replay never skipped a revoked block")
	}
}

// TestReplayRefusesDamage corrupts a committed log block: replay refuses
// the volume rather than apply or skip it.
func TestReplayRefusesDamage(t *testing.T) {
	device := newCrashDevice(crashBase(t))
	fs := ok(mountScratch(device, testClock()))
	ok(fs.Create(root(t, fs), []byte("damaged"), ModeRegular|0o644, 0, 0))
	image := append([]byte(nil), device.current...)
	// The first transaction's first copied block follows its descriptor.
	copied := fs.log.physical(fs.log.next(fs.log.first))
	image[copied*uint64(fs.blockSize)+100] ^= 0xFF
	_, err := mountScratch(&scratchDevice{data: image}, testClock())
	wantKind(t, err, KindCorrupt)
}

// TestOrphanCleanupAfterCut leaves a pinned, unlinked file and a file
// shrunk part way on the orphan list and checks that recovery frees what
// they held.
func TestOrphanCleanupAfterCut(t *testing.T) {
	device := newCrashDevice(crashBase(t))
	fs := ok(mountScratch(device, testClock()))
	free := fs.Info().FreeBlocks
	file := ok(fs.Create(root(t, fs), []byte("held"), ModeRegular|0o644, 0, 0))
	if _, err := fs.WriteFile(file, 0, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	if err := fs.Pin(file); err != nil {
		t.Fatal(err)
	}
	if err := fs.Unlink(root(t, fs), []byte("held")); err != nil {
		t.Fatal(err)
	}
	if fs.sb.LastOrphan != file.Number {
		t.Fatalf("orphan head %d, want %d", fs.sb.LastOrphan, file.Number)
	}
	if fs.Info().FreeBlocks == free {
		t.Fatal("pinned file lost its blocks before the last close")
	}
	_, again := recoverAt(t, device.imageAt(len(device.events), rand.New(rand.NewSource(5))))
	if replay := again.Recovery(); replay.Orphans != 1 {
		t.Fatalf("recovery %+v", replay)
	}
	if got := again.Info().FreeBlocks; got != free {
		t.Fatalf("free blocks after cleanup %d, want %d", got, free)
	}
}

// TestExportCrashImages writes volumes cut in the middle of the crash
// workload, before any recovery, with the tree GoOS recovers from each.
// tools/ext4fixture/check-mutations.sh replays them with the pinned
// e2fsck and compares.
func TestExportCrashImages(t *testing.T) {
	out := os.Getenv("GOOS_EXT4_EXPORT")
	if out == "" {
		t.Skip("GOOS_EXT4_EXPORT not set")
	}
	dir := filepath.Join(out, "crash")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := recordCrashRun(t)
	random := rand.New(rand.NewSource(6))
	exported := 0
	for _, op := range []int{2, 11, 13, 14, 18, 30} {
		point := run.ends[op] - 2
		image := run.device.imageAt(point, random)
		_, fs := recoverAt(t, append([]byte(nil), image...))
		if fs.Recovery().Transactions == 0 {
			t.Fatalf("cut in operation %d left nothing to replay", op)
		}
		name := fmt.Sprintf("cut-%02d", op)
		if err := os.WriteFile(filepath.Join(dir, name+".img"), image, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".manifest.tsv"), []byte(volumeManifest(t, fs)), 0o644); err != nil {
			t.Fatal(err)
		}
		exported++
	}
	t.Logf("exported %d cut images", exported)
}

// TestReplayForeignJournal replays journals debugfs wrote, which
// tools/ext4fixture/check-mutations.sh passes in GOOS_EXT4_IMPORT with the
// blocks the pinned e2fsck wrote when it replayed a copy of each.
func TestReplayForeignJournal(t *testing.T) {
	in := os.Getenv("GOOS_EXT4_IMPORT")
	if in == "" {
		t.Skip("GOOS_EXT4_IMPORT not set")
	}
	images, err := filepath.Glob(filepath.Join(in, "*.img"))
	if err != nil || len(images) == 0 {
		t.Fatalf("no images in %s", in)
	}
	for _, path := range images {
		name := strings.TrimSuffix(filepath.Base(path), ".img")
		image := ok(os.ReadFile(path))
		expect := ok(os.ReadFile(strings.TrimSuffix(path, ".img") + ".expect"))
		device := &scratchDevice{data: image}
		fs, err := mountScratch(device, testClock())
		if err != nil {
			t.Fatalf("%s: recovery: %v", name, err)
		}
		replay := fs.Recovery()
		if err := fs.Unmount(); err != nil {
			t.Fatalf("%s: unmount: %v", name, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(expect)), "\n") {
			var block uint64
			var want string
			if _, err := fmt.Sscanf(line, "%d\t%s", &block, &want); err != nil {
				t.Fatalf("%s: expect line %q", name, line)
			}
			sum := sha256.Sum256(device.data[block*4096 : (block+1)*4096])
			if got := hex.EncodeToString(sum[:]); got != want {
				t.Fatalf("%s: block %d after GoOS replay differs from e2fsck's", name, block)
			}
		}
		checkVolume(t, device)
		t.Logf("%s: %d transactions, %d blocks, %d revoked, same blocks as e2fsck", name, replay.Transactions, replay.Blocks, replay.Revoked)
	}
}
