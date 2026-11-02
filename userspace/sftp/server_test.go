package sftp

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"testing"

	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

const testRoot = "/home/user"

// memFiles is a filesystem whose only paths are the ones the server names, so a
// test sees exactly what an operation asked for.
type memFiles struct {
	files        map[string][]byte
	dirs         map[string]bool
	denied       map[string]bool
	hidden       map[string]bool
	open         map[vfs.Handle]*memOpen
	next         vfs.Handle
	dead         bool
	shortRead    int
	seeks, reads int
	batches      int
	calls        int
	batchLimit   int
}

type memOpen struct {
	path     string
	position uint64
	rights   vfs.Rights
}

func newMem() *memFiles {
	return &memFiles{files: map[string][]byte{}, dirs: map[string]bool{testRoot: true}, denied: map[string]bool{}, hidden: map[string]bool{}, open: map[vfs.Handle]*memOpen{}, batchLimit: vfs.MaxDirBatch}
}

func (m *memFiles) Stat(path []byte) (Attrs, vfs.Status) {
	m.calls++
	if m.dead {
		return Attrs{}, vfs.StatusPeerDied
	}
	name := string(path)
	if m.denied[name] {
		return Attrs{}, vfs.StatusPermissionDenied
	}
	if m.dirs[name] {
		return Attrs{Kind: ramfs.KindDirectory, Node: nodeOf(name)}, vfs.StatusOK
	}
	data, ok := m.files[name]
	if !ok {
		return Attrs{}, vfs.StatusNotFound
	}
	return Attrs{Kind: ramfs.KindFile, Size: uint64(len(data)), Node: nodeOf(name), Modified: ramfs.Timestamp{Seconds: 1700000000, Available: true}}, vfs.StatusOK
}
func nodeOf(name string) uint64 {
	var value uint64
	for _, c := range []byte(name) {
		value = value*31 + uint64(c)
	}
	return value
}
func (m *memFiles) StatOpen(file vfs.Handle) (Attrs, vfs.Status) {
	entry, ok := m.open[file]
	if !ok {
		return Attrs{}, vfs.StatusBackendError
	}
	return m.Stat([]byte(entry.path))
}
func (m *memFiles) Open(path []byte, rights vfs.Rights) (vfs.Handle, vfs.Status) {
	if m.dead {
		return 0, vfs.StatusPeerDied
	}
	if m.denied[string(path)] {
		return 0, vfs.StatusPermissionDenied
	}
	if _, ok := m.files[string(path)]; !ok {
		return 0, vfs.StatusNotFound
	}
	m.next++
	m.open[m.next] = &memOpen{path: string(path), rights: rights}
	return m.next, vfs.StatusOK
}
func (m *memFiles) Close(file vfs.Handle) vfs.Status {
	if m.dead {
		return vfs.StatusPeerDied
	}
	delete(m.open, file)
	return vfs.StatusOK
}
func (m *memFiles) Seek(file vfs.Handle, offset uint64) vfs.Status {
	if m.dead {
		return vfs.StatusPeerDied
	}
	m.seeks++
	m.open[file].position = offset
	return vfs.StatusOK
}
func (m *memFiles) Read(file vfs.Handle, target []byte) (int, vfs.Status) {
	if m.dead {
		return 0, vfs.StatusPeerDied
	}
	m.reads++
	entry := m.open[file]
	data := m.files[entry.path]
	if entry.position >= uint64(len(data)) {
		return 0, vfs.StatusOK
	}
	if m.shortRead != 0 && len(target) > m.shortRead {
		target = target[:m.shortRead]
	}
	count := copy(target, data[entry.position:])
	entry.position += uint64(count)
	return count, vfs.StatusOK
}
func (m *memFiles) Write(file vfs.Handle, data []byte) (int, vfs.Status) {
	if m.dead {
		return 0, vfs.StatusPeerDied
	}
	entry := m.open[file]
	if entry.rights&vfs.Write == 0 {
		return 0, vfs.StatusPermissionDenied
	}
	content := m.files[entry.path]
	if end := entry.position + uint64(len(data)); end > uint64(len(content)) {
		content = append(content, make([]byte, end-uint64(len(content)))...)
	}
	copy(content[entry.position:], data)
	m.files[entry.path] = content
	entry.position += uint64(len(data))
	return len(data), vfs.StatusOK
}
func (m *memFiles) Create(path []byte) vfs.Status {
	if m.dead {
		return vfs.StatusPeerDied
	}
	if m.denied[string(path)] {
		return vfs.StatusPermissionDenied
	}
	m.files[string(path)] = nil
	return vfs.StatusOK
}
func (m *memFiles) Mkdir(path []byte) vfs.Status {
	if m.dirs[string(path)] {
		return vfs.StatusAlreadyExists
	}
	m.dirs[string(path)] = true
	return vfs.StatusOK
}
func (m *memFiles) Unlink(path []byte) vfs.Status {
	delete(m.files, string(path))
	delete(m.dirs, string(path))
	return vfs.StatusOK
}
func (m *memFiles) Rename(source, target []byte) vfs.Status {
	if m.denied[string(target)] {
		return vfs.StatusPermissionDenied
	}
	if _, ok := m.files[string(target)]; ok {
		return vfs.StatusAlreadyExists
	}
	data, ok := m.files[string(source)]
	if !ok {
		return vfs.StatusNotFound
	}
	delete(m.files, string(source))
	m.files[string(target)] = data
	return vfs.StatusOK
}
func (m *memFiles) Truncate(path []byte, size uint64) vfs.Status {
	if m.denied[string(path)] {
		return vfs.StatusPermissionDenied
	}
	data, ok := m.files[string(path)]
	if !ok {
		return vfs.StatusNotFound
	}
	if size <= uint64(len(data)) {
		m.files[string(path)] = data[:size]
	} else {
		m.files[string(path)] = append(data, make([]byte, size-uint64(len(data)))...)
	}
	return vfs.StatusOK
}
func (m *memFiles) ReadDir(path []byte, cursor uint32, target []byte) (DirBatch, vfs.Status) {
	m.batches++
	if m.dead {
		return DirBatch{}, vfs.StatusPeerDied
	}
	prefix := string(path) + "/"
	var names []string
	for name := range m.files {
		if strings.HasPrefix(name, prefix) && !strings.Contains(name[len(prefix):], "/") && !m.hidden[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	batch := DirBatch{Next: cursor}
	for index := int(cursor); index < len(names); index++ {
		stat := ramfs.Stat{Kind: ramfs.KindFile, Size: uint64(len(m.files[names[index]]))}
		written := ramfs.AppendDirRecord(target[batch.Used:m.batchLimit], &stat, index+1, []byte(names[index][len(prefix):]))
		if written == 0 {
			return batch, vfs.StatusOK
		}
		batch.Used += written
		batch.Next = uint32(index + 1)
	}
	batch.End = true
	return batch, vfs.StatusOK
}

// wire builds requests; client reads replies out of a server under test.
type packet struct{ data []byte }

func request(kind byte, id uint32) *packet {
	p := &packet{data: []byte{kind}}
	return p.u32(id)
}
func (p *packet) u32(value uint32) *packet {
	p.data = binary.BigEndian.AppendUint32(p.data, value)
	return p
}
func (p *packet) u64(value uint64) *packet {
	p.data = binary.BigEndian.AppendUint64(p.data, value)
	return p
}
func (p *packet) str(value string) *packet {
	p.u32(uint32(len(value)))
	p.data = append(p.data, value...)
	return p
}
func (p *packet) bytes(value []byte) *packet {
	p.u32(uint32(len(value)))
	p.data = append(p.data, value...)
	return p
}
func (p *packet) framed() []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(p.data))), p.data...)
}

type session struct {
	t      *testing.T
	server *Server
	mem    *memFiles
	id     uint32
}

func newSession(t *testing.T) *session {
	t.Helper()
	return newSessionOn(t, newMem(), 42)
}
func newSessionOn(t *testing.T, mem *memFiles, nonce uint64) *session {
	t.Helper()
	server, ok := NewServer(mem, []byte(testRoot), nonce)
	if !ok {
		t.Fatal("server refused its root")
	}
	s := &session{t: t, server: server, mem: mem}
	reply := s.feed((&packet{data: []byte{fxpInit, 0, 0, 0, 3}}).framed())
	if len(reply) != 1 || reply[0][0] != fxpVersion || binary.BigEndian.Uint32(reply[0][1:]) != Version || len(reply[0]) != 5 {
		t.Fatalf("handshake: %x", reply)
	}
	return s
}

// feed delivers bytes and returns the replies, each without its length field.
func (s *session) feed(data []byte) [][]byte {
	s.t.Helper()
	var replies [][]byte
	for {
		for {
			reply, err := s.server.Step()
			if err != nil {
				s.t.Fatalf("step: %v", err)
			}
			if reply == nil {
				break
			}
			if int(binary.BigEndian.Uint32(reply)) != len(reply)-4 {
				s.t.Fatalf("reply length field %x", reply[:4])
			}
			replies = append(replies, append([]byte(nil), reply[4:]...))
		}
		if len(data) == 0 {
			return replies
		}
		count := copy(s.server.Space(), data)
		if err := s.server.Commit(count); err != nil {
			s.t.Fatalf("commit: %v", err)
		}
		data = data[count:]
	}
}

func (s *session) call(kind byte, build func(*packet)) []byte {
	s.t.Helper()
	s.id++
	p := request(kind, s.id)
	if build != nil {
		build(p)
	}
	replies := s.feed(p.framed())
	if len(replies) != 1 {
		s.t.Fatalf("%d replies", len(replies))
	}
	if binary.BigEndian.Uint32(replies[0][1:]) != s.id {
		s.t.Fatalf("request ID not preserved: %x", replies[0][:5])
	}
	return replies[0]
}

func statusOf(t *testing.T, reply []byte) uint32 {
	t.Helper()
	if reply[0] != fxpStatus {
		t.Fatalf("expected status, got type %d", reply[0])
	}
	return binary.BigEndian.Uint32(reply[5:])
}

func (s *session) wantStatus(want uint32, kind byte, build func(*packet)) {
	s.t.Helper()
	if got := statusOf(s.t, s.call(kind, build)); got != want {
		s.t.Fatalf("status %d, want %d", got, want)
	}
}

func (s *session) openFile(name string, flags uint32) []byte {
	s.t.Helper()
	reply := s.call(fxpOpen, func(p *packet) { p.str(name).u32(flags).u32(0) })
	if reply[0] != fxpHandle {
		s.t.Fatalf("open %s flags %x: status %d", name, flags, statusOf(s.t, reply))
	}
	return reply[9:]
}

func (s *session) openDir(name string) []byte {
	s.t.Helper()
	reply := s.call(fxpOpendir, func(p *packet) { p.str(name) })
	if reply[0] != fxpHandle {
		s.t.Fatalf("opendir: status %d", statusOf(s.t, reply))
	}
	return reply[9:]
}

func TestPacketSplitAtEveryBoundary(t *testing.T) {
	whole := request(fxpRealpath, 7).str("a/./b/../c").framed()
	for cut := 0; cut <= len(whole); cut++ {
		s := newSession(t)
		if replies := s.feed(whole[:cut]); cut != len(whole) && len(replies) != 0 {
			t.Fatalf("cut %d: replied early", cut)
		}
		replies := s.feed(whole[cut:])
		if cut == len(whole) {
			// The first feed returned its reply; serve nothing twice.
			if len(replies) != 0 {
				t.Fatalf("second reply")
			}
			continue
		}
		if len(replies) != 1 || !bytes.Contains(replies[0], []byte("/a/c")) {
			t.Fatalf("cut %d: %q", cut, replies)
		}
	}
}

func TestSeveralRequestsInOneBuffer(t *testing.T) {
	s := newSession(t)
	var input []byte
	for id := uint32(100); id < 140; id++ {
		input = append(input, request(fxpRealpath, id).str(".").framed()...)
	}
	replies := s.feed(input)
	if len(replies) != 40 {
		t.Fatalf("%d replies", len(replies))
	}
	for index, reply := range replies {
		if binary.BigEndian.Uint32(reply[1:]) != uint32(100+index) {
			t.Fatalf("reply %d out of order", index)
		}
	}
}

func TestInvalidFramingEndsSession(t *testing.T) {
	for _, length := range []uint32{0, MaxPacket + 1, 0xffffffff} {
		server, _ := NewServer(newMem(), []byte(testRoot), 1)
		wire := binary.BigEndian.AppendUint32(nil, length)
		count := copy(server.Space(), wire)
		if err := server.Commit(count); err != ErrFraming {
			t.Fatalf("length %d: %v", length, err)
		}
	}
	// A short packet has no room for its request ID.
	s := newSession(t)
	count := copy(s.server.Space(), []byte{0, 0, 0, 2, fxpStat, 0})
	if err := s.server.Commit(count); err != nil {
		t.Fatal(err)
	}
	if _, err := s.server.Step(); err != ErrFraming {
		t.Fatalf("short packet: %v", err)
	}
	// A string length past the packet is an impossible encoding.
	s = newSession(t)
	bad := request(fxpStat, 1).u32(0xfffffff0).framed()
	count = copy(s.server.Space(), bad)
	s.server.Commit(count)
	if _, err := s.server.Step(); err != ErrFraming {
		t.Fatalf("string length: %v", err)
	}
	// Anything before INIT, or a version below 3, ends the session too.
	server, _ := NewServer(newMem(), []byte(testRoot), 1)
	count = copy(server.Space(), request(fxpStat, 1).str(".").framed())
	server.Commit(count)
	if _, err := server.Step(); err != ErrFraming {
		t.Fatalf("no handshake: %v", err)
	}
	server, _ = NewServer(newMem(), []byte(testRoot), 1)
	count = copy(server.Space(), (&packet{data: []byte{fxpInit, 0, 0, 0, 2}}).framed())
	server.Commit(count)
	if _, err := server.Step(); err != ErrFraming {
		t.Fatalf("old version: %v", err)
	}
}

func TestLargestPacketFitsInput(t *testing.T) {
	s := newSession(t)
	handle := s.openFile("/big", fxfWrite|fxfCreat)
	// A whole MaxPacket request is framed and refused as too much data.
	data := make([]byte, MaxPacket-(1+4+4+len(handle)+8+4))
	reply := s.feed(request(fxpWrite, 9).bytes(handle).u64(0).bytes(data).framed())
	if len(reply) != 1 || statusOf(t, reply[0]) != fxBadMessage {
		t.Fatalf("oversized transfer: %x", reply)
	}
}

func TestUnknownAndUnsupportedRequests(t *testing.T) {
	s := newSession(t)
	for _, kind := range []byte{19, 20, 200, 201, 99} {
		s.wantStatus(fxOpUnsupported, kind, func(p *packet) { p.str("x") })
	}
}

func TestPathNormalization(t *testing.T) {
	cases := map[string]string{
		"":             "/",
		".":            "/",
		"/":            "/",
		"//":           "/",
		"..":           "/",
		"../../..":     "/",
		"/../../etc":   "/etc",
		"a":            "/a",
		"a/b":          "/a/b",
		"/a/./b/":      "/a/b",
		"a/../b":       "/b",
		"a/../../b":    "/b",
		"документы/ж":  "/документы/ж",
		"É":           "/É",
		"/state/x":     "/state/x",
		"/home/user/a": "/home/user/a",
	}
	for input, want := range cases {
		s := newSession(t)
		reply := s.call(fxpRealpath, func(p *packet) { p.str(input) })
		if reply[0] != fxpName || binary.BigEndian.Uint32(reply[5:]) != 1 {
			t.Fatalf("%q: reply %x", input, reply)
		}
		size := binary.BigEndian.Uint32(reply[9:])
		if got := string(reply[13 : 13+size]); got != want {
			t.Fatalf("%q: got %q want %q", input, got, want)
		}
	}
	s := newSession(t)
	s.wantStatus(fxBadMessage, fxpRealpath, func(p *packet) { p.str("a\x00b") })
	s.wantStatus(fxFailure, fxpRealpath, func(p *packet) { p.str(strings.Repeat("a", 1025)) })
}

func TestDotDotCannotLeaveRoot(t *testing.T) {
	s := newSession(t)
	s.mem.files["/state/secret"] = []byte("x")
	s.mem.files[testRoot+"/state/secret"] = []byte("inside")
	for _, name := range []string{"../../state/secret", "/../../../state/secret", "a/../../../state/secret"} {
		handle := s.openFile(name, fxfRead)
		if entry := s.mem.open[s.server.handles[handle[1]].file]; entry.path != testRoot+"/state/secret" {
			t.Fatalf("%s mapped to %s", name, entry.path)
		}
	}
}

func TestRootCannotBeRemovedOrRenamed(t *testing.T) {
	s := newSession(t)
	s.wantStatus(fxPermissionDenied, fxpRmdir, func(p *packet) { p.str("/") })
	s.wantStatus(fxPermissionDenied, fxpRmdir, func(p *packet) { p.str("a/..") })
	s.wantStatus(fxPermissionDenied, fxpRemove, func(p *packet) { p.str("/") })
	s.wantStatus(fxPermissionDenied, fxpRename, func(p *packet) { p.str("/").str("/x") })
	s.mem.files[testRoot+"/f"] = nil
	s.wantStatus(fxPermissionDenied, fxpRename, func(p *packet) { p.str("/f").str("/") })
	if !s.mem.dirs[testRoot] {
		t.Fatal("root removed")
	}
}

func TestStaleAndForeignHandles(t *testing.T) {
	s := newSession(t)
	s.mem.files[testRoot+"/f"] = []byte("data")
	old := s.openFile("/f", fxfRead)
	s.wantStatus(fxOK, fxpClose, func(p *packet) { p.bytes(old) })
	// The slot is reused, and the old handle must not reach its new occupant.
	fresh := s.openFile("/f", fxfRead)
	if fresh[1] != old[1] {
		t.Fatalf("slot %d not reused from %d", fresh[1], old[1])
	}
	s.wantStatus(fxFailure, fxpRead, func(p *packet) { p.bytes(old).u64(0).u32(4) })
	s.wantStatus(fxFailure, fxpClose, func(p *packet) { p.bytes(old) })
	if reply := s.call(fxpRead, func(p *packet) { p.bytes(fresh).u64(0).u32(4) }); reply[0] != fxpData {
		t.Fatalf("fresh handle: %x", reply)
	}
	// A handle from another session fails even with the same slot and generation.
	other := newSessionOn(t, s.mem, 43)
	other.openFile("/f", fxfRead)
	other.wantStatus(fxFailure, fxpRead, func(p *packet) { p.bytes(fresh).u64(0).u32(4) })
	// A file handle is not a directory handle, and the reverse.
	dir := s.openDir("/")
	s.wantStatus(fxFailure, fxpRead, func(p *packet) { p.bytes(dir).u64(0).u32(4) })
	s.wantStatus(fxFailure, fxpReaddir, func(p *packet) { p.bytes(fresh) })
}

func TestMalformedHandles(t *testing.T) {
	s := newSession(t)
	s.mem.files[testRoot+"/f"] = []byte("data")
	good := s.openFile("/f", fxfRead)
	for _, wire := range [][]byte{nil, good[:15], append(append([]byte(nil), good...), 0)} {
		s.wantStatus(fxBadMessage, fxpRead, func(p *packet) { p.bytes(wire).u64(0).u32(4) })
	}
	bad := append([]byte(nil), good...)
	bad[1] = 200
	s.wantStatus(fxFailure, fxpRead, func(p *packet) { p.bytes(bad).u64(0).u32(4) })
	bad = append([]byte(nil), good...)
	bad[2] = 1
	s.wantStatus(fxFailure, fxpRead, func(p *packet) { p.bytes(bad).u64(0).u32(4) })
}

func TestHandleTableExhaustion(t *testing.T) {
	s := newSession(t)
	s.mem.files[testRoot+"/f"] = nil
	var handles [][]byte
	for len(handles) < MaxHandles {
		handles = append(handles, s.openFile("/f", fxfRead))
	}
	s.wantStatus(fxFailure, fxpOpen, func(p *packet) { p.str("/f").u32(fxfRead).u32(0) })
	s.wantStatus(fxFailure, fxpOpendir, func(p *packet) { p.str("/") })
	// An exhausted table creates nothing it could not then open.
	s.wantStatus(fxFailure, fxpOpen, func(p *packet) { p.str("/new").u32(fxfWrite | fxfCreat).u32(0) })
	if _, found := s.mem.files[testRoot+"/new"]; found {
		t.Fatal("refused open created a file")
	}
	s.wantStatus(fxOK, fxpClose, func(p *packet) { p.bytes(handles[3]) })
	s.openFile("/f", fxfRead)
	// Ending the session closes everything it held.
	s.server.Close()
	if len(s.mem.open) != 0 {
		t.Fatalf("%d VFS files left open", len(s.mem.open))
	}
}

func TestOpenFlagCombinations(t *testing.T) {
	path := testRoot + "/f"
	cases := []struct {
		name   string
		exists bool
		flags  uint32
		status uint32 // fxOK means a handle.
		after  string
		absent bool
	}{
		{"read missing", false, fxfRead, fxNoSuchFile, "", true},
		{"read existing", true, fxfRead, fxOK, "old", false},
		{"write without create", false, fxfWrite, fxNoSuchFile, "", true},
		{"write existing keeps", true, fxfWrite, fxOK, "old", false},
		{"create new", false, fxfWrite | fxfCreat, fxOK, "", false},
		{"create existing keeps", true, fxfWrite | fxfCreat, fxOK, "old", false},
		{"create trunc", true, fxfWrite | fxfCreat | fxfTrunc, fxOK, "", false},
		{"trunc existing", true, fxfWrite | fxfTrunc, fxOK, "", false},
		{"exclusive new", false, fxfWrite | fxfCreat | fxfExcl, fxOK, "", false},
		{"exclusive existing", true, fxfWrite | fxfCreat | fxfExcl, fxFailure, "old", false},
		{"read write", true, fxfRead | fxfWrite, fxOK, "old", false},
		{"append", true, fxfWrite | fxfAppend, fxOK, "old", false},
		{"nothing", true, 0, fxBadMessage, "old", false},
		{"unknown bit", true, fxfRead | 0x100, fxBadMessage, "old", false},
		{"create read only", false, fxfRead | fxfCreat, fxBadMessage, "", true},
		{"trunc read only", true, fxfRead | fxfTrunc, fxBadMessage, "old", false},
		{"exclusive alone", false, fxfWrite | fxfExcl, fxBadMessage, "", true},
	}
	for _, test := range cases {
		s := newSession(t)
		if test.exists {
			s.mem.files[path] = []byte("old")
		}
		reply := s.call(fxpOpen, func(p *packet) { p.str("/f").u32(test.flags).u32(0) })
		if test.status == fxOK {
			if reply[0] != fxpHandle {
				t.Fatalf("%s: status %d", test.name, statusOf(t, reply))
			}
		} else if got := statusOf(t, reply); got != test.status {
			t.Fatalf("%s: status %d want %d", test.name, got, test.status)
		}
		data, found := s.mem.files[path]
		if found == test.absent || string(data) != test.after {
			t.Fatalf("%s: file %q found=%v", test.name, data, found)
		}
	}
	// A directory is not a file to open, and a mode on creation is no authority.
	s := newSession(t)
	s.wantStatus(fxFailure, fxpOpen, func(p *packet) { p.str("/").u32(fxfRead).u32(0) })
	reply := s.call(fxpOpen, func(p *packet) { p.str("/mode").u32(fxfWrite | fxfCreat).u32(attrPermissions).u32(0o600) })
	if reply[0] != fxpHandle {
		t.Fatalf("mode on create: %x", reply)
	}
	s.wantStatus(fxOpUnsupported, fxpOpen, func(p *packet) { p.str("/own").u32(fxfWrite | fxfCreat).u32(attrUIDGID).u32(0).u32(0) })
	s.mem.denied[testRoot+"/no"] = true
	s.wantStatus(fxPermissionDenied, fxpOpen, func(p *packet) { p.str("/no").u32(fxfRead).u32(0) })
}

func TestReadShortAndEOF(t *testing.T) {
	s := newSession(t)
	s.mem.files[testRoot+"/f"] = []byte("0123456789")
	handle := s.openFile("/f", fxfRead)
	data := func(reply []byte) []byte {
		if reply[0] != fxpData {
			t.Fatalf("expected data, got %x", reply)
		}
		return reply[9:]
	}
	if got := data(s.call(fxpRead, func(p *packet) { p.bytes(handle).u64(2).u32(3) })); string(got) != "234" {
		t.Fatalf("read at 2: %q", got)
	}
	s.mem.shortRead = 4
	if got := data(s.call(fxpRead, func(p *packet) { p.bytes(handle).u64(0).u32(100) })); string(got) != "0123" {
		t.Fatalf("short read: %q", got)
	}
	s.mem.shortRead = 0
	if got := data(s.call(fxpRead, func(p *packet) { p.bytes(handle).u64(8).u32(100) })); string(got) != "89" {
		t.Fatalf("read to end: %q", got)
	}
	s.wantStatus(fxEOF, fxpRead, func(p *packet) { p.bytes(handle).u64(10).u32(100) })
	s.wantStatus(fxEOF, fxpRead, func(p *packet) { p.bytes(handle).u64(1 << 60).u32(100) })
	// Reads may come in any order, and only a change of offset costs a seek.
	if got := data(s.call(fxpRead, func(p *packet) { p.bytes(handle).u64(0).u32(1) })); string(got) != "0" {
		t.Fatalf("rewind: %q", got)
	}
	before := s.mem.seeks
	if got := data(s.call(fxpRead, func(p *packet) { p.bytes(handle).u64(1).u32(1) })); string(got) != "1" {
		t.Fatalf("sequential: %q", got)
	}
	if s.mem.seeks != before {
		t.Fatal("sequential read issued a seek")
	}
	// A transfer is bounded, not refused.
	s.mem.files[testRoot+"/big"] = make([]byte, 3*MaxData)
	big := s.openFile("/big", fxfRead)
	if got := data(s.call(fxpRead, func(p *packet) { p.bytes(big).u64(0).u32(1 << 30) })); len(got) != MaxData {
		t.Fatalf("bounded read: %d", len(got))
	}
	s.wantStatus(fxPermissionDenied, fxpWrite, func(p *packet) { p.bytes(handle).u64(0).str("x") })
}

func TestRandomOffsetWritesAndAppend(t *testing.T) {
	s := newSession(t)
	handle := s.openFile("/f", fxfRead|fxfWrite|fxfCreat)
	for _, write := range []struct {
		offset uint64
		text   string
	}{{5, "fghij"}, {0, "abcde"}, {2, "XY"}, {10, "k"}} {
		s.wantStatus(fxOK, fxpWrite, func(p *packet) { p.bytes(handle).u64(write.offset).str(write.text) })
	}
	if got := string(s.mem.files[testRoot+"/f"]); got != "abXYefghijk" {
		t.Fatalf("content %q", got)
	}
	// A write and a read on one handle each state their own offset.
	if reply := s.call(fxpRead, func(p *packet) { p.bytes(handle).u64(2).u32(2) }); string(reply[9:]) != "XY" {
		t.Fatalf("read after write: %q", reply[9:])
	}
	s.wantStatus(fxOK, fxpWrite, func(p *packet) { p.bytes(handle).u64(0).str("A") })
	if got := string(s.mem.files[testRoot+"/f"]); got != "AbXYefghijk" {
		t.Fatalf("content %q", got)
	}
	appender := s.openFile("/f", fxfWrite|fxfAppend)
	s.wantStatus(fxOK, fxpWrite, func(p *packet) { p.bytes(appender).u64(0).str("Z") })
	if got := string(s.mem.files[testRoot+"/f"]); got != "AbXYefghijkZ" {
		t.Fatalf("append %q", got)
	}
	readOnly := s.openFile("/f", fxfRead)
	s.wantStatus(fxPermissionDenied, fxpWrite, func(p *packet) { p.bytes(readOnly).u64(0).str("x") })
	writeOnly := s.openFile("/f", fxfWrite)
	s.wantStatus(fxPermissionDenied, fxpRead, func(p *packet) { p.bytes(writeOnly).u64(0).u32(1) })
	s.wantStatus(fxBadMessage, fxpWrite, func(p *packet) { p.bytes(handle).u64(0).bytes(make([]byte, MaxData+1)) })
}

func TestTransferDoesNotScaleWithBytes(t *testing.T) {
	s := newSession(t)
	const size = 4 << 20
	handle := s.openFile("/f", fxfRead|fxfWrite|fxfCreat)
	block := bytes.Repeat([]byte{7}, MaxData)
	for offset := uint64(0); offset < size; offset += MaxData {
		s.wantStatus(fxOK, fxpWrite, func(p *packet) { p.bytes(handle).u64(offset).bytes(block) })
	}
	if s.server.Counters.BytesUploaded != size || s.server.Counters.VFSWrites != size/MaxData {
		t.Fatalf("writes %+v", s.server.Counters)
	}
	if s.mem.seeks > 1 {
		t.Fatalf("%d seeks in a sequential upload", s.mem.seeks)
	}
	for offset := uint64(0); offset < size; offset += MaxData {
		if reply := s.call(fxpRead, func(p *packet) { p.bytes(handle).u64(offset).u32(MaxData) }); len(reply) != 9+MaxData {
			t.Fatalf("read %d: %d", offset, len(reply))
		}
	}
	if s.server.Counters.BytesDownloaded != size || s.server.Counters.VFSReads != size/MaxData {
		t.Fatalf("reads %+v", s.server.Counters)
	}
}

func TestSetstat(t *testing.T) {
	s := newSession(t)
	path := testRoot + "/f"
	s.mem.files[path] = []byte("0123456789")
	s.wantStatus(fxOK, fxpSetstat, func(p *packet) { p.str("/f").u32(attrSize).u64(4) })
	if string(s.mem.files[path]) != "0123" {
		t.Fatalf("truncate: %q", s.mem.files[path])
	}
	handle := s.openFile("/f", fxfWrite)
	s.wantStatus(fxOK, fxpFsetstat, func(p *packet) { p.bytes(handle).u32(attrSize).u64(2) })
	if string(s.mem.files[path]) != "01" {
		t.Fatalf("ftruncate: %q", s.mem.files[path])
	}
	reader := s.openFile("/f", fxfRead)
	s.wantStatus(fxPermissionDenied, fxpFsetstat, func(p *packet) { p.bytes(reader).u32(attrSize).u64(0) })
	s.wantStatus(fxOK, fxpSetstat, func(p *packet) { p.str("/f").u32(0) })
	for _, flags := range []uint32{attrUIDGID, attrPermissions, attrACModTime} {
		s.wantStatus(fxOpUnsupported, fxpSetstat, func(p *packet) {
			p.str("/f").u32(flags).u32(0)
			if flags != attrPermissions {
				p.u32(0)
			}
		})
	}
	// Nothing is applied when part of a request cannot be.
	s.wantStatus(fxOpUnsupported, fxpSetstat, func(p *packet) { p.str("/f").u32(attrSize | attrPermissions).u64(0).u32(0o777) })
	if string(s.mem.files[path]) != "01" {
		t.Fatalf("partial change: %q", s.mem.files[path])
	}
	s.mem.denied[testRoot+"/no"] = true
	s.wantStatus(fxPermissionDenied, fxpSetstat, func(p *packet) { p.str("/no").u32(attrSize).u64(0) })
	s.wantStatus(fxPermissionDenied, fxpSetstat, func(p *packet) { p.str("/").u32(attrSize).u64(0) })
	// A handle's file may be renamed, and its old name reused by another file.
	s.mem.files[testRoot+"/g"] = []byte("other")
	moved := s.openFile("/g", fxfWrite)
	s.mem.open[s.server.handles[moved[1]].file].path = testRoot + "/moved"
	s.mem.files[testRoot+"/moved"] = []byte("mine")
	s.wantStatus(fxFailure, fxpFsetstat, func(p *packet) { p.bytes(moved).u32(attrSize).u64(0) })
	if string(s.mem.files[testRoot+"/g"]) != "other" {
		t.Fatal("truncated the file now at the old name")
	}
}

func attrsReply(t *testing.T, reply []byte) (flags uint32, size uint64, mode uint32, rest []byte) {
	t.Helper()
	if reply[0] != fxpAttrs {
		t.Fatalf("expected attrs: %x", reply)
	}
	flags = binary.BigEndian.Uint32(reply[5:])
	size = binary.BigEndian.Uint64(reply[9:])
	mode = binary.BigEndian.Uint32(reply[17:])
	return flags, size, mode, reply[21:]
}

func TestAttributes(t *testing.T) {
	s := newSession(t)
	s.mem.files[testRoot+"/f"] = []byte("data")
	for _, kind := range []byte{fxpStat, fxpLstat} {
		flags, size, mode, rest := attrsReply(t, s.call(kind, func(p *packet) { p.str("/f") }))
		if flags != attrSize|attrPermissions|attrACModTime || size != 4 || mode != 0o100644 || binary.BigEndian.Uint32(rest[4:]) != 1700000000 {
			t.Fatalf("file attrs %x %d %o %x", flags, size, mode, rest)
		}
	}
	_, _, mode, _ := attrsReply(t, s.call(fxpStat, func(p *packet) { p.str("/") }))
	if mode != 0o040755 {
		t.Fatalf("directory mode %o", mode)
	}
	handle := s.openFile("/f", fxfRead)
	if _, size, _, _ := attrsReply(t, s.call(fxpFstat, func(p *packet) { p.bytes(handle) })); size != 4 {
		t.Fatalf("fstat size %d", size)
	}
	dir := s.openDir("/")
	if _, _, mode, _ := attrsReply(t, s.call(fxpFstat, func(p *packet) { p.bytes(dir) })); mode != 0o040755 {
		t.Fatalf("fstat dir mode %o", mode)
	}
	s.wantStatus(fxNoSuchFile, fxpStat, func(p *packet) { p.str("/missing") })
	s.mem.denied[testRoot+"/no"] = true
	s.wantStatus(fxPermissionDenied, fxpStat, func(p *packet) { p.str("/no") })
}

func decodeNames(t *testing.T, reply []byte) (names, longs []string) {
	t.Helper()
	if reply[0] != fxpName {
		t.Fatalf("expected name: %x", reply[:min(len(reply), 16)])
	}
	count := binary.BigEndian.Uint32(reply[5:])
	rest := reply[9:]
	for index := uint32(0); index < count; index++ {
		size := binary.BigEndian.Uint32(rest)
		names = append(names, string(rest[4:4+size]))
		rest = rest[4+size:]
		size = binary.BigEndian.Uint32(rest)
		longs = append(longs, string(rest[4:4+size]))
		rest = rest[4+size:]
		flags := binary.BigEndian.Uint32(rest)
		rest = rest[4:]
		if flags&attrSize != 0 {
			rest = rest[8:]
		}
		if flags&attrPermissions != 0 {
			rest = rest[4:]
		}
		if flags&attrACModTime != 0 {
			rest = rest[8:]
		}
	}
	if len(rest) != 0 {
		t.Fatalf("%d bytes after the last name", len(rest))
	}
	return names, longs
}

func TestDirectoryPagination(t *testing.T) {
	s := newSession(t)
	want := map[string]bool{}
	for index := 0; index < 700; index++ {
		name := "entry-" + strings.Repeat("x", index%200) + "-" + strconv.Itoa(index)
		s.mem.files[testRoot+"/"+name] = []byte("x")
		want[name] = true
	}
	handle := s.openDir("/")
	replies := 0
	for {
		reply := s.call(fxpReaddir, func(p *packet) { p.bytes(handle) })
		if reply[0] == fxpStatus {
			if got := statusOf(t, reply); got != fxEOF {
				t.Fatalf("end status %d", got)
			}
			break
		}
		if len(reply)+4 > 4+MaxPacket {
			t.Fatalf("reply of %d bytes", len(reply))
		}
		names, longs := decodeNames(t, reply)
		for index, name := range names {
			if !want[name] {
				t.Fatalf("unexpected or repeated %q", name)
			}
			delete(want, name)
			if !strings.HasSuffix(longs[index], " "+name) || !strings.HasPrefix(longs[index], "-rw-r--r-- 1 user user 1 ") {
				t.Fatalf("long name %q", longs[index])
			}
		}
		replies++
	}
	if len(want) != 0 {
		t.Fatalf("%d names never listed", len(want))
	}
	if replies < 2 || replies > 8 {
		t.Fatalf("%d replies for 700 names", replies)
	}
	if s.mem.batches > 700/4 {
		t.Fatalf("%d VFS batches for 700 names", s.mem.batches)
	}
	// The end stays the end, without asking the filesystem again.
	before := s.mem.batches
	for index := 0; index < 3; index++ {
		s.wantStatus(fxEOF, fxpReaddir, func(p *packet) { p.bytes(handle) })
	}
	if s.mem.batches != before {
		t.Fatal("repeated READDIR at the end reached the filesystem")
	}
}

func TestDirectoryEdgeCases(t *testing.T) {
	s := newSession(t)
	s.mem.dirs[testRoot+"/empty"] = true
	empty := s.openDir("/empty")
	s.wantStatus(fxEOF, fxpReaddir, func(p *packet) { p.bytes(empty) })
	s.wantStatus(fxEOF, fxpReaddir, func(p *packet) { p.bytes(empty) })

	s.mem.files[testRoot+"/f"] = nil
	s.wantStatus(fxFailure, fxpOpendir, func(p *packet) { p.str("/f") })
	s.wantStatus(fxNoSuchFile, fxpOpendir, func(p *packet) { p.str("/missing") })

	long := strings.Repeat("л", 512)
	s.mem.dirs[testRoot+"/long"] = true
	s.mem.files[testRoot+"/long/"+long] = []byte("z")
	s.mem.files[testRoot+"/long/secret"] = []byte("z")
	s.mem.hidden[testRoot+"/long/secret"] = true
	longDir := s.openDir("/long")
	names, _ := decodeNames(t, s.call(fxpReaddir, func(p *packet) { p.bytes(longDir) }))
	if len(names) != 1 || names[0] != long {
		t.Fatalf("names %q", names)
	}
}

func TestDirectoryManagement(t *testing.T) {
	s := newSession(t)
	s.wantStatus(fxOK, fxpMkdir, func(p *packet) { p.str("/d").u32(attrPermissions).u32(0o777) })
	if !s.mem.dirs[testRoot+"/d"] {
		t.Fatal("mkdir")
	}
	s.wantStatus(fxFailure, fxpMkdir, func(p *packet) { p.str("/d").u32(0) })
	s.wantStatus(fxOpUnsupported, fxpMkdir, func(p *packet) { p.str("/e").u32(attrUIDGID).u64(0) })
	// RMDIR removes directories and REMOVE files, and neither crosses over.
	s.wantStatus(fxFailure, fxpRemove, func(p *packet) { p.str("/d") })
	s.mem.files[testRoot+"/f"] = []byte("x")
	s.wantStatus(fxFailure, fxpRmdir, func(p *packet) { p.str("/f") })
	s.wantStatus(fxOK, fxpRename, func(p *packet) { p.str("/f").str("d/../g") })
	if _, found := s.mem.files[testRoot+"/g"]; !found {
		t.Fatalf("rename: %v", s.mem.files)
	}
	s.mem.files[testRoot+"/h"] = nil
	s.wantStatus(fxFailure, fxpRename, func(p *packet) { p.str("/g").str("/h") })
	s.wantStatus(fxNoSuchFile, fxpRename, func(p *packet) { p.str("/gone").str("/i") })
	s.mem.denied[testRoot+"/keys"] = true
	s.wantStatus(fxPermissionDenied, fxpRename, func(p *packet) { p.str("/g").str("/keys") })
	s.wantStatus(fxOK, fxpRemove, func(p *packet) { p.str("/g") })
	s.wantStatus(fxOK, fxpRmdir, func(p *packet) { p.str("/d") })
	s.wantStatus(fxBadMessage, fxpStat, func(p *packet) { p.str("/x").u32(0) })
}

func TestFilesystemDeathEndsSession(t *testing.T) {
	s := newSession(t)
	s.mem.files[testRoot+"/f"] = []byte("x")
	handle := s.openFile("/f", fxfRead)
	s.mem.dead = true
	count := copy(s.server.Space(), request(fxpRead, 5).bytes(handle).u64(0).u32(1).framed())
	if err := s.server.Commit(count); err != nil {
		t.Fatal(err)
	}
	reply, err := s.server.Step()
	if err != nil || statusOf(t, reply[4:]) != fxFailure {
		t.Fatalf("reply %x, %v", reply, err)
	}
	if _, err := s.server.Step(); err != ErrFilesystem {
		t.Fatalf("after death: %v", err)
	}
	// Nothing is reissued against a session that is gone.
	before := s.mem.calls
	s.server.Close()
	if s.mem.calls != before {
		t.Fatal("close reached a dead filesystem")
	}
}

func TestDeathDuringListingAfterEntriesReachesNextRequest(t *testing.T) {
	s := newSession(t)
	for index := 0; index < 100; index++ {
		s.mem.files[testRoot+"/name-"+strconv.Itoa(index)] = nil
	}
	dir := s.openDir("/")
	names, _ := decodeNames(t, s.call(fxpReaddir, func(p *packet) { p.bytes(dir) }))
	if len(names) != 100 {
		t.Fatalf("%d names", len(names))
	}
	s.wantStatus(fxEOF, fxpReaddir, func(p *packet) { p.bytes(dir) })
}

func TestEmptyDataAndZeroLengthTransfers(t *testing.T) {
	s := newSession(t)
	handle := s.openFile("/zero", fxfRead|fxfWrite|fxfCreat)
	s.wantStatus(fxOK, fxpWrite, func(p *packet) { p.bytes(handle).u64(0).str("") })
	if reply := s.call(fxpRead, func(p *packet) { p.bytes(handle).u64(0).u32(0) }); reply[0] != fxpData || len(reply) != 9 {
		t.Fatalf("zero read: %x", reply)
	}
	s.wantStatus(fxEOF, fxpRead, func(p *packet) { p.bytes(handle).u64(0).u32(16) })
}

func TestAppendDate(t *testing.T) {
	for seconds, want := range map[int64]string{0: "1970-01-01 00:00", 1700000000: "2023-11-14 22:13", 951782400: "2000-02-29 00:00", 4102444800: "2100-01-01 00:00"} {
		if got := string(appendDate(nil, seconds)); got != want {
			t.Fatalf("%d: %q want %q", seconds, got, want)
		}
	}
}

func TestRequestsSurviveReservedRootPaths(t *testing.T) {
	if _, ok := NewServer(newMem(), []byte("/"), 1); ok {
		t.Fatal("accepted / as a root")
	}
	if _, ok := NewServer(newMem(), []byte("/home/../x"), 1); ok {
		t.Fatal("accepted a non-canonical root")
	}
}

// FuzzServer feeds arbitrary bytes after the handshake. Nothing may panic, grow
// past its buffers, or answer with a malformed frame.
func FuzzServer(f *testing.F) {
	f.Add(request(fxpOpen, 1).str("/f").u32(fxfWrite | fxfCreat).u32(0).framed())
	f.Add(request(fxpRead, 1).str("xxxxxxxxxxxxxxxx").u64(0).u32(1 << 31).framed())
	f.Add(request(fxpWrite, 1).u32(0xffffffff).framed())
	f.Add(request(fxpSetstat, 1).str("/f").u32(attrExtended).u32(0xffffffff).framed())
	f.Add(request(fxpMkdir, 1).str("/d").u32(0xffffffff).framed())
	f.Fuzz(func(t *testing.T, data []byte) {
		mem := newMem()
		mem.files[testRoot+"/f"] = []byte("content")
		server, _ := NewServer(mem, []byte(testRoot), 7)
		count := copy(server.Space(), (&packet{data: []byte{fxpInit, 0, 0, 0, 3}}).framed())
		server.Commit(count)
		if _, err := server.Step(); err != nil {
			t.Fatal(err)
		}
		for {
			reply, err := server.Step()
			if err != nil {
				return
			}
			if reply != nil {
				if len(reply) > 4+MaxPacket || int(binary.BigEndian.Uint32(reply)) != len(reply)-4 {
					t.Fatalf("bad reply frame of %d bytes", len(reply))
				}
				continue
			}
			if len(data) == 0 {
				return
			}
			count := copy(server.Space(), data)
			if err := server.Commit(count); err != nil {
				return
			}
			data = data[count:]
		}
	})
}
