// Package sftp implements the server side of SFTP protocol version 3 over a
// borrowed filesystem. It holds one bounded input buffer, one bounded reply and
// one bounded handle table, and it handles one request at a time. It is not the
// security boundary: the session it is given decides what any path may reach.
package sftp

import (
	"encoding/binary"
	"errors"
	"goos/internal/limits"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

const (
	// Version is the only protocol version this server speaks.
	Version = 3
	// MaxPacket bounds a packet's length field, in either direction.
	MaxPacket = 64 << 10
	// MaxData bounds the file data one READ returns or one WRITE carries.
	MaxData = 32 << 10
	// MaxHandles bounds the files and directories one session holds open.
	MaxHandles = 32
	handleSize = 16
	// maxNameRecord is the most one directory entry takes in a NAME reply: its
	// name, a long name of that name and a fixed-size prefix, and its attributes.
	maxNameRecord = 4 + limits.NameMax + 4 + limits.NameMax + maxLongPrefix + attrsSize
	maxLongPrefix = 80
	attrsSize     = 4 + 8 + 4 + 8
	// maxDirBatches bounds the listing work one READDIR does when the entries it
	// examines are hidden from this session.
	maxDirBatches = 16
)

// Packet types of version 3.
const (
	fxpInit     = 1
	fxpVersion  = 2
	fxpOpen     = 3
	fxpClose    = 4
	fxpRead     = 5
	fxpWrite    = 6
	fxpLstat    = 7
	fxpFstat    = 8
	fxpSetstat  = 9
	fxpFsetstat = 10
	fxpOpendir  = 11
	fxpReaddir  = 12
	fxpRemove   = 13
	fxpMkdir    = 14
	fxpRmdir    = 15
	fxpRealpath = 16
	fxpStat     = 17
	fxpRename   = 18
	fxpStatus   = 101
	fxpHandle   = 102
	fxpData     = 103
	fxpName     = 104
	fxpAttrs    = 105
)

// Status codes of version 3.
const (
	fxOK               = 0
	fxEOF              = 1
	fxNoSuchFile       = 2
	fxPermissionDenied = 3
	fxFailure          = 4
	fxBadMessage       = 5
	fxOpUnsupported    = 8
)

// Open flags of version 3.
const (
	fxfRead   = 0x01
	fxfWrite  = 0x02
	fxfAppend = 0x04
	fxfCreat  = 0x08
	fxfTrunc  = 0x10
	fxfExcl   = 0x20
	fxfKnown  = fxfRead | fxfWrite | fxfAppend | fxfCreat | fxfTrunc | fxfExcl
)

// Attribute flags of version 3.
const (
	attrSize        = 0x00000001
	attrUIDGID      = 0x00000002
	attrPermissions = 0x00000004
	attrACModTime   = 0x00000008
	attrExtended    = 0x80000000
	attrKnown       = attrSize | attrUIDGID | attrPermissions | attrACModTime | attrExtended
)

// The file-type bits reported with the synthetic permissions. GoOS has no Unix
// ownership or modes; these only tell a client what it is looking at.
const (
	modeFile      = 0o100644
	modeDirectory = 0o040755
)

var (
	// ErrFraming ends a session whose packet framing or encoded lengths cannot
	// be trusted: the next packet's position is unknown.
	ErrFraming = errors.New("sftp: malformed packet")
	// ErrFilesystem ends a session whose filesystem session is gone. Its open
	// handles died with it, and no later request could be served.
	ErrFilesystem = errors.New("sftp: filesystem session lost")
)

// Attrs is what the protocol reports of a file.
type Attrs struct {
	Kind     ramfs.Kind
	Size     uint64
	Modified ramfs.Timestamp
	Node     uint64
}

// DirBatch reports one batched listing request. Records start target and use
// the format of ramfs.DirRecord; hidden entries are already gone.
type DirBatch struct {
	Used int
	Next uint32
	End  bool
}

// Files is one filesystem session. An operation answers vfs.StatusPeerDied when
// the session no longer answers. Seek sets the position of one open file.
type Files interface {
	Stat(path []byte) (Attrs, vfs.Status)
	StatOpen(file vfs.Handle) (Attrs, vfs.Status)
	Open(path []byte, rights vfs.Rights) (vfs.Handle, vfs.Status)
	Close(file vfs.Handle) vfs.Status
	Seek(file vfs.Handle, offset uint64) vfs.Status
	Read(file vfs.Handle, target []byte) (int, vfs.Status)
	Write(file vfs.Handle, data []byte) (int, vfs.Status)
	Create(path []byte) vfs.Status
	Mkdir(path []byte) vfs.Status
	Unlink(path []byte) vfs.Status
	Rename(source, target []byte) vfs.Status
	Truncate(path []byte, size uint64) vfs.Status
	ReadDir(path []byte, cursor uint32, target []byte) (DirBatch, vfs.Status)
}

// Counters measure a session's work. They hold sizes and counts, never content.
type Counters struct {
	BytesUploaded   uint64
	BytesDownloaded uint64
	Requests        uint64
	VFSReads        uint64
	VFSWrites       uint64
	DirBatches      uint64
}

type handleKind uint8

const (
	kindFree handleKind = iota
	kindFile
	kindDirectory
)

// slot is one entry of the handle table. Only slot, kind, generation and the
// session's nonce reach the peer. The generation changes when the slot is freed,
// so a closed handle never names the slot's next occupant; the nonce keeps a
// handle from one session out of the next one's table.
type slot struct {
	kind       handleKind
	generation uint32
	retired    bool
	file       vfs.Handle
	// position is the open file's VFS position, when positioned holds. Every
	// transfer states its own offset and seeks unless this already matches.
	position   uint64
	positioned bool
	appending  bool
	writable   bool
	readable   bool
	path       []byte
	cursor     uint32
	end        bool
}

// Server is one SFTP session.
type Server struct {
	files       Files
	root        []byte
	nonce       uint64
	input       [4 + MaxPacket]byte
	start, used int
	reply       [4 + MaxPacket]byte
	replySize   int
	initialized bool
	lost        bool
	handles     [MaxHandles]slot
	local       [limits.PathMax]byte
	other       [limits.PathMax]byte
	visible     [limits.PathMax]byte
	dir         [vfs.MaxDirBatch]byte
	longPrefix  [maxLongPrefix]byte
	Counters    Counters
}

// NewServer returns a session presenting the canonical directory root as "/".
// nonce must differ between sessions and should be unpredictable.
func NewServer(files Files, root []byte, nonce uint64) (*Server, bool) {
	if files == nil || !vfs.ValidRootPath(root) {
		return nil, false
	}
	// new, not a literal: a composite of this size would pass through the stack.
	server := new(Server)
	server.files, server.nonce, server.root = files, nonce, append([]byte(nil), root...)
	return server, true
}

// Space is the free input the caller may fill. It always has room for the rest
// of a valid packet once Step has returned no more work.
func (server *Server) Space() []byte {
	if server.start != 0 {
		copy(server.input[:], server.input[server.start:server.start+server.used])
		server.start = 0
	}
	return server.input[server.used:]
}

// Commit records count bytes written to the slice Space returned. A packet too
// long to accept fails here, before anything is stored for it.
func (server *Server) Commit(count int) error {
	if count < 0 || count > len(server.input)-server.start-server.used {
		return ErrFraming
	}
	server.used += count
	return server.checkFrame()
}

func (server *Server) checkFrame() error {
	if server.used < 4 {
		return nil
	}
	length := binary.BigEndian.Uint32(server.input[server.start:])
	if length == 0 || length > MaxPacket {
		return ErrFraming
	}
	return nil
}

// Step serves the next complete packet and returns its reply, which stays valid
// until the next Step. No reply and no error means Step needs more input. A
// failure that could still be reported has already been; ErrFilesystem follows
// the reply to the request that found the session gone.
func (server *Server) Step() ([]byte, error) {
	if server.lost {
		return nil, ErrFilesystem
	}
	if err := server.checkFrame(); err != nil {
		return nil, err
	}
	if server.used < 4 {
		return nil, nil
	}
	length := int(binary.BigEndian.Uint32(server.input[server.start:]))
	if server.used < 4+length {
		return nil, nil
	}
	packet := server.input[server.start+4 : server.start+4+length]
	server.start += 4 + length
	server.used -= 4 + length
	if err := server.serve(packet); err != nil {
		return nil, err
	}
	return server.reply[:server.replySize], nil
}

// Close releases every open handle, as the end of a session does.
func (server *Server) Close() {
	for index := range server.handles {
		entry := &server.handles[index]
		if entry.kind == kindFile && !server.lost {
			server.files.Close(entry.file)
		}
		server.free(entry)
	}
}

func (server *Server) serve(packet []byte) error {
	kind := packet[0]
	if !server.initialized {
		// The handshake precedes everything, and it has no request ID.
		if kind != fxpInit || len(packet) < 5 || binary.BigEndian.Uint32(packet[1:5]) < Version {
			return ErrFraming
		}
		server.initialized = true
		server.begin(fxpVersion)
		server.putUint32(Version)
		server.finish()
		return nil
	}
	if len(packet) < 5 {
		return ErrFraming
	}
	id := binary.BigEndian.Uint32(packet[1:5])
	server.Counters.Requests++
	body := reader{data: packet[5:]}
	switch kind {
	case fxpOpen:
		server.open(id, &body)
	case fxpClose:
		server.closeHandle(id, &body)
	case fxpRead:
		server.read(id, &body)
	case fxpWrite:
		server.write(id, &body)
	case fxpStat, fxpLstat:
		server.stat(id, &body)
	case fxpFstat:
		server.fstat(id, &body)
	case fxpSetstat:
		server.setstat(id, &body)
	case fxpFsetstat:
		server.fsetstat(id, &body)
	case fxpOpendir:
		server.opendir(id, &body)
	case fxpReaddir:
		server.readdir(id, &body)
	case fxpRemove:
		server.remove(id, &body, ramfs.KindFile)
	case fxpRmdir:
		server.remove(id, &body, ramfs.KindDirectory)
	case fxpMkdir:
		server.mkdir(id, &body)
	case fxpRealpath:
		server.realpath(id, &body)
	case fxpRename:
		server.rename(id, &body)
	default:
		// SYMLINK, READLINK, EXTENDED, and anything newer than this version.
		server.status(id, fxOpUnsupported)
		return nil
	}
	if body.bad {
		return ErrFraming
	}
	return nil
}

// reader decodes one request. An encoded length running past the packet marks
// it bad, and the session ends: the request can't be answered as anything else.
type reader struct {
	data []byte
	bad  bool
}

func (body *reader) uint32() uint32 {
	if len(body.data) < 4 {
		body.bad = true
		return 0
	}
	value := binary.BigEndian.Uint32(body.data)
	body.data = body.data[4:]
	return value
}

func (body *reader) uint64() uint64 {
	if len(body.data) < 8 {
		body.bad = true
		return 0
	}
	value := binary.BigEndian.Uint64(body.data)
	body.data = body.data[8:]
	return value
}

func (body *reader) bytes() []byte {
	size := body.uint32()
	if body.bad || uint64(size) > uint64(len(body.data)) {
		body.bad = true
		return nil
	}
	value := body.data[:size]
	body.data = body.data[size:]
	return value
}

// requestAttrs is the part of an ATTRS structure a request can change.
type requestAttrs struct {
	flags uint32
	size  uint64
}

func (body *reader) attrs() requestAttrs {
	var attrs requestAttrs
	attrs.flags = body.uint32()
	if attrs.flags&attrSize != 0 {
		attrs.size = body.uint64()
	}
	if attrs.flags&attrUIDGID != 0 {
		body.uint64()
	}
	if attrs.flags&attrPermissions != 0 {
		body.uint32()
	}
	if attrs.flags&attrACModTime != 0 {
		body.uint64()
	}
	if attrs.flags&attrExtended != 0 {
		// Each pair takes at least eight bytes, so the loop ends with the packet.
		for count := body.uint32(); count != 0 && !body.bad; count-- {
			body.bytes()
			body.bytes()
		}
	}
	return attrs
}

// done reports a request whose fields have all decoded and left nothing over.
// A request with trailing bytes is refused and the session goes on; framing
// still holds, as the packet's own length found the next one.
func (server *Server) done(id uint32, body *reader) bool {
	if body.bad {
		return false
	}
	if len(body.data) != 0 {
		server.status(id, fxBadMessage)
		return false
	}
	return true
}

func (server *Server) begin(kind byte) {
	server.reply[4] = kind
	server.replySize = 5
}

func (server *Server) finish() {
	binary.BigEndian.PutUint32(server.reply[:], uint32(server.replySize-4))
}

func (server *Server) putUint32(value uint32) {
	binary.BigEndian.PutUint32(server.reply[server.replySize:], value)
	server.replySize += 4
}

func (server *Server) putUint64(value uint64) {
	binary.BigEndian.PutUint64(server.reply[server.replySize:], value)
	server.replySize += 8
}

func (server *Server) putBytes(value []byte) {
	server.putUint32(uint32(len(value)))
	server.replySize += copy(server.reply[server.replySize:], value)
}

func (server *Server) putString(value string) {
	server.putUint32(uint32(len(value)))
	server.replySize += copy(server.reply[server.replySize:], value)
}

func (server *Server) putAttrs(attrs *Attrs) {
	flags := uint32(attrSize | attrPermissions)
	modified := attrs.Modified.Available && attrs.Modified.Seconds >= 0 && attrs.Modified.Seconds <= 0xffffffff
	if modified {
		flags |= attrACModTime
	}
	server.putUint32(flags)
	server.putUint64(attrs.Size)
	if attrs.Kind == ramfs.KindDirectory {
		server.putUint32(modeDirectory)
	} else {
		server.putUint32(modeFile)
	}
	if modified {
		server.putUint32(uint32(attrs.Modified.Seconds))
		server.putUint32(uint32(attrs.Modified.Seconds))
	}
}

func statusMessage(code uint32) string {
	switch code {
	case fxOK:
		return "Success"
	case fxEOF:
		return "End of file"
	case fxNoSuchFile:
		return "No such file"
	case fxPermissionDenied:
		return "Permission denied"
	case fxBadMessage:
		return "Bad message"
	case fxOpUnsupported:
		return "Operation unsupported"
	}
	return "Failure"
}

func (server *Server) status(id uint32, code uint32) {
	server.begin(fxpStatus)
	server.putUint32(id)
	server.putUint32(code)
	server.putString(statusMessage(code))
	server.putUint32(0) // An empty language tag.
	server.finish()
}

// vfsStatus maps a native result. The peer gets one of the protocol's codes and
// a fixed message, never a native value.
func (server *Server) vfsStatus(status vfs.Status) uint32 {
	switch status {
	case vfs.StatusOK:
		return fxOK
	case vfs.StatusNotFound:
		return fxNoSuchFile
	case vfs.StatusPermissionDenied:
		return fxPermissionDenied
	case vfs.StatusInvalidArgument:
		return fxBadMessage
	case vfs.StatusUnsupported, vfs.StatusCrossDevice:
		return fxOpUnsupported
	case vfs.StatusPeerDied:
		server.lost = true
	}
	return fxFailure
}

func (server *Server) fail(id uint32, status vfs.Status) { server.status(id, server.vfsStatus(status)) }

// resolve turns a client path into the path below the root it names, in target,
// and leaves the path the client sees in visible. It resolves "." and ".." the
// way the session's own canonical check does, and it never leaves "/".
func (server *Server) resolve(path []byte, target *[limits.PathMax]byte) ([]byte, []byte, uint32) {
	visible, code := canonicalVisible(path, &server.visible)
	if code != fxOK {
		return nil, nil, code
	}
	if len(server.root)+len(visible) > len(target) {
		return nil, nil, fxFailure
	}
	size := copy(target[:], server.root)
	if len(visible) > 1 {
		size += copy(target[size:], visible)
	}
	return target[:size], visible, fxOK
}

// canonicalVisible resolves a path against the client's "/". An embedded NUL is
// a malformed request; an over-long path or name is one the native limits refuse.
func canonicalVisible(path []byte, output *[limits.PathMax]byte) ([]byte, uint32) {
	output[0] = '/'
	size := 1
	for start := 0; start <= len(path); {
		end := start
		for end < len(path) && path[end] != '/' {
			if path[end] == 0 {
				return nil, fxBadMessage
			}
			end++
		}
		component := path[start:end]
		start = end + 1
		switch {
		case len(component) == 0 || string(component) == ".":
		case string(component) == "..":
			for size > 1 && output[size-1] != '/' {
				size--
			}
			if size > 1 {
				size--
			}
		default:
			if len(component) > limits.NameMax || size+1+len(component) > len(output) {
				return nil, fxFailure
			}
			if size > 1 {
				output[size] = '/'
				size++
			}
			size += copy(output[size:], component)
		}
	}
	return output[:size], fxOK
}

func (server *Server) allocate(kind handleKind, path []byte) (*slot, int) {
	for index := range server.handles {
		entry := &server.handles[index]
		if entry.kind != kindFree || entry.retired {
			continue
		}
		entry.kind = kind
		entry.path = append(entry.path[:0], path...)
		return entry, index
	}
	return nil, 0
}

// free ends a handle. A generation that would repeat retires the slot instead.
func (server *Server) free(entry *slot) {
	generation := entry.generation + 1
	retired := entry.retired || generation == 0
	path := entry.path[:0]
	*entry = slot{generation: generation, retired: retired, path: path}
}

func (server *Server) putHandle(id uint32, index int) {
	entry := &server.handles[index]
	server.begin(fxpHandle)
	server.putUint32(id)
	server.putUint32(handleSize)
	wire := server.reply[server.replySize : server.replySize+handleSize]
	wire[0], wire[1] = byte(entry.kind), byte(index)
	wire[2], wire[3] = 0, 0
	binary.BigEndian.PutUint32(wire[4:], entry.generation)
	binary.BigEndian.PutUint64(wire[8:], server.nonce)
	server.replySize += handleSize
	server.finish()
}

// handle finds an open handle of the wanted kind, or answers the request. A
// wrong shape is malformed; a stale, foreign, or other-kind handle simply fails.
func (server *Server) handle(id uint32, wire []byte, kind handleKind) *slot {
	if len(wire) != handleSize {
		server.status(id, fxBadMessage)
		return nil
	}
	index := int(wire[1])
	if index >= len(server.handles) || handleKind(wire[0]) != kind && kind != kindFree || wire[2] != 0 || wire[3] != 0 ||
		binary.BigEndian.Uint64(wire[8:]) != server.nonce {
		server.status(id, fxFailure)
		return nil
	}
	entry := &server.handles[index]
	if entry.kind == kindFree || entry.kind != handleKind(wire[0]) || entry.generation != binary.BigEndian.Uint32(wire[4:]) {
		server.status(id, fxFailure)
		return nil
	}
	return entry
}

func (server *Server) open(id uint32, body *reader) {
	name := body.bytes()
	flags := body.uint32()
	attrs := body.attrs()
	if !server.done(id, body) {
		return
	}
	path, _, code := server.resolve(name, &server.local)
	if code != fxOK {
		server.status(id, code)
		return
	}
	writes := flags&fxfWrite != 0
	if flags&^fxfKnown != 0 || flags&(fxfRead|fxfWrite) == 0 || !writes && flags&(fxfAppend|fxfCreat|fxfTrunc|fxfExcl) != 0 || flags&fxfExcl != 0 && flags&fxfCreat == 0 {
		server.status(id, fxBadMessage)
		return
	}
	if attrs.flags&^attrKnown != 0 {
		server.status(id, fxBadMessage)
		return
	}
	// A created file's mode is informational and is not authority. Anything else
	// would be a metadata change this filesystem has no way to make.
	if attrs.flags&^attrPermissions != 0 {
		server.status(id, fxOpUnsupported)
		return
	}
	entry, index := server.allocate(kindFile, path)
	if entry == nil {
		server.status(id, fxFailure)
		return
	}
	failed := func(status vfs.Status) {
		server.free(entry)
		server.fail(id, status)
	}
	created := false
	stat, status := server.files.Stat(path)
	switch {
	case status == vfs.StatusNotFound && flags&fxfCreat != 0:
		if status = server.files.Create(path); status != vfs.StatusOK {
			failed(status)
			return
		}
		created = true
	case status != vfs.StatusOK:
		failed(status)
		return
	case flags&fxfExcl != 0 || stat.Kind != ramfs.KindFile:
		failed(vfs.StatusAlreadyExists)
		return
	}
	rights := vfs.Rights(0)
	if flags&fxfRead != 0 {
		rights |= vfs.Read
	}
	if writes {
		rights |= vfs.Write
	}
	file, status := server.files.Open(path, rights)
	if status != vfs.StatusOK {
		if created {
			server.files.Unlink(path)
		}
		failed(status)
		return
	}
	if flags&fxfTrunc != 0 && !created {
		if status = server.files.Truncate(path, 0); status != vfs.StatusOK {
			server.files.Close(file)
			failed(status)
			return
		}
	}
	entry.file, entry.readable, entry.writable = file, flags&fxfRead != 0, writes
	entry.appending = flags&fxfAppend != 0
	server.putHandle(id, index)
}

func (server *Server) closeHandle(id uint32, body *reader) {
	wire := body.bytes()
	if !server.done(id, body) {
		return
	}
	entry := server.handle(id, wire, kindFree)
	if entry == nil {
		return
	}
	status := vfs.StatusOK
	if entry.kind == kindFile {
		status = server.files.Close(entry.file)
	}
	server.free(entry)
	server.fail(id, status)
}

func (server *Server) position(entry *slot, offset uint64) vfs.Status {
	if entry.positioned && entry.position == offset {
		return vfs.StatusOK
	}
	entry.positioned = false
	status := server.files.Seek(entry.file, offset)
	if status == vfs.StatusOK {
		entry.position, entry.positioned = offset, true
	}
	return status
}

func (server *Server) read(id uint32, body *reader) {
	wire := body.bytes()
	offset := body.uint64()
	length := body.uint32()
	if !server.done(id, body) {
		return
	}
	entry := server.handle(id, wire, kindFile)
	if entry == nil {
		return
	}
	if !entry.readable {
		server.status(id, fxPermissionDenied)
		return
	}
	if length > MaxData {
		length = MaxData
	}
	if status := server.position(entry, offset); status != vfs.StatusOK {
		server.fail(id, status)
		return
	}
	const header = 4 + 1 + 4 + 4
	server.Counters.VFSReads++
	count, status := server.files.Read(entry.file, server.reply[header:header+int(length)])
	if status != vfs.StatusOK {
		entry.positioned = false
		server.fail(id, status)
		return
	}
	if count < 0 || count > int(length) {
		entry.positioned = false
		server.status(id, fxFailure)
		return
	}
	entry.position += uint64(count)
	if count == 0 && length != 0 {
		server.status(id, fxEOF)
		return
	}
	server.Counters.BytesDownloaded += uint64(count)
	server.begin(fxpData)
	server.putUint32(id)
	server.putUint32(uint32(count))
	server.replySize += count
	server.finish()
}

func (server *Server) write(id uint32, body *reader) {
	wire := body.bytes()
	offset := body.uint64()
	data := body.bytes()
	if !server.done(id, body) {
		return
	}
	entry := server.handle(id, wire, kindFile)
	if entry == nil {
		return
	}
	if !entry.writable {
		server.status(id, fxPermissionDenied)
		return
	}
	if len(data) > MaxData {
		server.status(id, fxBadMessage)
		return
	}
	if entry.appending {
		stat, status := server.files.StatOpen(entry.file)
		if status != vfs.StatusOK {
			server.fail(id, status)
			return
		}
		offset = stat.Size
	}
	if status := server.position(entry, offset); status != vfs.StatusOK {
		server.fail(id, status)
		return
	}
	for len(data) != 0 {
		server.Counters.VFSWrites++
		count, status := server.files.Write(entry.file, data)
		if status != vfs.StatusOK || count <= 0 || count > len(data) {
			entry.positioned = false
			if status == vfs.StatusOK {
				status = vfs.StatusBackendError
			}
			server.fail(id, status)
			return
		}
		entry.position += uint64(count)
		server.Counters.BytesUploaded += uint64(count)
		data = data[count:]
	}
	server.status(id, fxOK)
}

func (server *Server) stat(id uint32, body *reader) {
	name := body.bytes()
	if !server.done(id, body) {
		return
	}
	path, _, code := server.resolve(name, &server.local)
	if code != fxOK {
		server.status(id, code)
		return
	}
	attrs, status := server.files.Stat(path)
	if status != vfs.StatusOK {
		server.fail(id, status)
		return
	}
	server.putAttrsReply(id, &attrs)
}

func (server *Server) putAttrsReply(id uint32, attrs *Attrs) {
	server.begin(fxpAttrs)
	server.putUint32(id)
	server.putAttrs(attrs)
	server.finish()
}

func (server *Server) fstat(id uint32, body *reader) {
	wire := body.bytes()
	if !server.done(id, body) {
		return
	}
	entry := server.handle(id, wire, kindFree)
	if entry == nil {
		return
	}
	var attrs Attrs
	var status vfs.Status
	if entry.kind == kindFile {
		attrs, status = server.files.StatOpen(entry.file)
	} else {
		attrs, status = server.files.Stat(entry.path)
	}
	if status != vfs.StatusOK {
		server.fail(id, status)
		return
	}
	server.putAttrsReply(id, &attrs)
}

// changeAttrs applies a SETSTAT's attributes all or nothing. Only a size can
// change; permissions, ownership and times have no native form that an SFTP
// client may set, and chmod must not look as if it worked.
func (server *Server) changeAttrs(id uint32, path []byte, attrs requestAttrs) {
	if attrs.flags&^attrKnown != 0 {
		server.status(id, fxBadMessage)
		return
	}
	if attrs.flags&^attrSize != 0 {
		server.status(id, fxOpUnsupported)
		return
	}
	if attrs.flags&attrSize == 0 {
		server.status(id, fxOK)
		return
	}
	server.fail(id, server.files.Truncate(path, attrs.size))
}

func (server *Server) setstat(id uint32, body *reader) {
	name := body.bytes()
	attrs := body.attrs()
	if !server.done(id, body) {
		return
	}
	path, visible, code := server.resolve(name, &server.local)
	if code != fxOK {
		server.status(id, code)
		return
	}
	if len(visible) == 1 && attrs.flags&attrSize != 0 {
		server.status(id, fxPermissionDenied)
		return
	}
	server.changeAttrs(id, path, attrs)
}

func (server *Server) fsetstat(id uint32, body *reader) {
	wire := body.bytes()
	attrs := body.attrs()
	if !server.done(id, body) {
		return
	}
	entry := server.handle(id, wire, kindFree)
	if entry == nil {
		return
	}
	if attrs.flags&attrSize != 0 && attrs.flags&^attrKnown == 0 && attrs.flags&^attrSize == 0 {
		if entry.kind != kindFile {
			server.status(id, fxFailure)
			return
		}
		if !entry.writable {
			server.status(id, fxPermissionDenied)
			return
		}
		// The native truncate takes a path. The open file may have been renamed
		// since, and the name may now be someone else's.
		open, status := server.files.StatOpen(entry.file)
		if status != vfs.StatusOK {
			server.fail(id, status)
			return
		}
		named, status := server.files.Stat(entry.path)
		if status != vfs.StatusOK {
			server.fail(id, status)
			return
		}
		if open.Node != named.Node {
			server.status(id, fxFailure)
			return
		}
	}
	server.changeAttrs(id, entry.path, attrs)
}

func (server *Server) opendir(id uint32, body *reader) {
	name := body.bytes()
	if !server.done(id, body) {
		return
	}
	path, _, code := server.resolve(name, &server.local)
	if code != fxOK {
		server.status(id, code)
		return
	}
	attrs, status := server.files.Stat(path)
	if status != vfs.StatusOK {
		server.fail(id, status)
		return
	}
	if attrs.Kind != ramfs.KindDirectory {
		server.status(id, fxFailure)
		return
	}
	entry, index := server.allocate(kindDirectory, path)
	if entry == nil {
		server.status(id, fxFailure)
		return
	}
	server.putHandle(id, index)
}

// readdir answers with as many complete entries as fit one reply. A failure
// after some entries is left for the next request to find. The cursor advances
// past what was sent and nothing else, so no entry is lost between replies.
func (server *Server) readdir(id uint32, body *reader) {
	wire := body.bytes()
	if !server.done(id, body) {
		return
	}
	entry := server.handle(id, wire, kindDirectory)
	if entry == nil {
		return
	}
	if entry.end {
		server.status(id, fxEOF)
		return
	}
	server.begin(fxpName)
	server.putUint32(id)
	countAt := server.replySize
	server.putUint32(0)
	count := uint32(0)
	for batches := 0; batches < maxDirBatches && server.replySize+maxNameRecord <= len(server.reply); batches++ {
		server.Counters.DirBatches++
		batch, status := server.files.ReadDir(entry.path, entry.cursor, server.dir[:])
		if status == vfs.StatusNotFound && entry.cursor != 0 {
			batch, status = DirBatch{End: true, Next: entry.cursor}, vfs.StatusOK
		}
		if status != vfs.StatusOK {
			if count != 0 {
				break
			}
			server.fail(id, status)
			return
		}
		if batch.Used < 0 || batch.Used > len(server.dir) || !batch.End && batch.Next <= entry.cursor {
			if count != 0 {
				break
			}
			server.status(id, fxFailure)
			return
		}
		records := server.dir[:batch.Used]
		complete := true
		var record ramfs.DirRecord
		for len(records) != 0 {
			size, valid := record.Decode(records)
			if !valid {
				server.status(id, fxFailure)
				return
			}
			if server.replySize+maxNameRecord > len(server.reply) {
				complete = false
				break
			}
			server.putRecord(&record)
			count++
			entry.cursor = uint32(record.Next)
			records = records[size:]
		}
		if !complete {
			break
		}
		entry.cursor = batch.Next
		if batch.End {
			entry.end = true
			break
		}
	}
	if count == 0 {
		if entry.end {
			server.status(id, fxEOF)
		} else {
			server.status(id, fxFailure)
		}
		return
	}
	binary.BigEndian.PutUint32(server.reply[countAt:], count)
	server.finish()
}

func (server *Server) putRecord(record *ramfs.DirRecord) {
	attrs := Attrs{Kind: record.Stat.Kind, Size: record.Stat.Size, Modified: record.Stat.Modified}
	server.putBytes(record.Name)
	prefix := appendLongPrefix(server.longPrefix[:0], &attrs)
	server.putUint32(uint32(len(prefix) + len(record.Name)))
	server.replySize += copy(server.reply[server.replySize:], prefix)
	server.replySize += copy(server.reply[server.replySize:], record.Name)
	server.putAttrs(&attrs)
}

// appendLongPrefix writes what precedes the name in a long name. A client shows
// it as it is, and ownership here is fixed text, not an account.
func appendLongPrefix(out []byte, attrs *Attrs) []byte {
	if attrs.Kind == ramfs.KindDirectory {
		out = append(out, "drwxr-xr-x 1 user user "...)
	} else {
		out = append(out, "-rw-r--r-- 1 user user "...)
	}
	out = appendDecimal(out, attrs.Size)
	out = append(out, ' ')
	if attrs.Modified.Available && attrs.Modified.Seconds >= 0 {
		out = appendDate(out, attrs.Modified.Seconds)
	} else {
		out = append(out, "-"...)
	}
	return append(out, ' ')
}

func appendDecimal(out []byte, value uint64) []byte {
	var digits [20]byte
	at := len(digits)
	for {
		at--
		digits[at] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(out, digits[at:]...)
}

func appendTwo(out []byte, value int64) []byte {
	return append(out, byte('0'+value/10%10), byte('0'+value%10))
}

// appendDate writes UTC seconds as YYYY-MM-DD HH:MM, by the civil-from-days
// calculation; a native service has no time package to spare for one field.
func appendDate(out []byte, seconds int64) []byte {
	days := seconds / 86400
	remainder := seconds % 86400
	z := days + 719468
	era := z / 146097
	dayOfEra := z - era*146097
	yearOfEra := (dayOfEra - dayOfEra/1460 + dayOfEra/36524 - dayOfEra/146096) / 365
	dayOfYear := dayOfEra - (365*yearOfEra + yearOfEra/4 - yearOfEra/100)
	shifted := (5*dayOfYear + 2) / 153
	day := dayOfYear - (153*shifted+2)/5 + 1
	month := shifted + 3
	if month > 12 {
		month -= 12
	}
	year := yearOfEra + era*400
	if month <= 2 {
		year++
	}
	out = appendDecimal(out, uint64(year))
	out = append(out, '-')
	out = appendTwo(out, month)
	out = append(out, '-')
	out = appendTwo(out, day)
	out = append(out, ' ')
	out = appendTwo(out, remainder/3600)
	out = append(out, ':')
	return appendTwo(out, remainder%3600/60)
}

func (server *Server) remove(id uint32, body *reader, kind ramfs.Kind) {
	name := body.bytes()
	if !server.done(id, body) {
		return
	}
	path, visible, code := server.resolve(name, &server.local)
	if code != fxOK {
		server.status(id, code)
		return
	}
	if len(visible) == 1 {
		server.status(id, fxPermissionDenied)
		return
	}
	attrs, status := server.files.Stat(path)
	if status != vfs.StatusOK {
		server.fail(id, status)
		return
	}
	if attrs.Kind != kind {
		server.status(id, fxFailure)
		return
	}
	server.fail(id, server.files.Unlink(path))
}

func (server *Server) mkdir(id uint32, body *reader) {
	name := body.bytes()
	attrs := body.attrs()
	if !server.done(id, body) {
		return
	}
	path, _, code := server.resolve(name, &server.local)
	if code != fxOK {
		server.status(id, code)
		return
	}
	if attrs.flags&^attrKnown != 0 {
		server.status(id, fxBadMessage)
		return
	}
	if attrs.flags&^attrPermissions != 0 {
		server.status(id, fxOpUnsupported)
		return
	}
	server.fail(id, server.files.Mkdir(path))
}

func (server *Server) rename(id uint32, body *reader) {
	oldName := body.bytes()
	newName := body.bytes()
	if !server.done(id, body) {
		return
	}
	source, visible, code := server.resolve(oldName, &server.local)
	if code != fxOK {
		server.status(id, code)
		return
	}
	root := len(visible) == 1
	target, visible, code := server.resolve(newName, &server.other)
	if code != fxOK {
		server.status(id, code)
		return
	}
	if root || len(visible) == 1 {
		server.status(id, fxPermissionDenied)
		return
	}
	server.fail(id, server.files.Rename(source, target))
}

// realpath answers without asking the filesystem: a client resolves names that
// do not exist yet, and the answer tells it nothing the path did not.
func (server *Server) realpath(id uint32, body *reader) {
	name := body.bytes()
	if !server.done(id, body) {
		return
	}
	visible, code := canonicalVisible(name, &server.visible)
	if code != fxOK {
		server.status(id, code)
		return
	}
	server.begin(fxpName)
	server.putUint32(id)
	server.putUint32(1)
	server.putBytes(visible)
	server.putBytes(visible)
	server.putUint32(0)
	server.finish()
}
