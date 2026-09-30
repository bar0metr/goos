//go:build goos_userspace

package main

// Validation-build probes of kernel mechanisms PID 1 relies on.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	supervisor "goos/userspace/init"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
)

var linuxProbeStreams [2][3][2]uint64
var linuxProbeGrants [2][5]supervisor.Capability

// probeLinuxLaunch exercises policy, mapping, stdin/stdout and process exit
// with a static Linux ELF using raw syscalls.
//
//go:noinline
func probeLinuxLaunch() bool {
	executable, name, located := locateExecutable([]byte("/system/linux/raw-probe"))
	if located != supervisor.LaunchOK || executable.Format != supervisor.ExecutableLinuxX8664 {
		return false
	}
	streams, grants, vfsHandle, prepared := prepareLinuxProbe(0)
	if !prepared {
		return false
	}
	// A false classifier hint must not select the native loader.
	claimedNative := executable
	claimedNative.Format = supervisor.ExecutableNative
	slot, data, ok := linuxProbeStartup(false)
	if !ok {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	if event, status := requestLaunch(claimedNative, name, grants, supervisor.ProfileLarge, slot, uint32(len(data))); status != supervisor.LaunchInvalidImage || event.Process != 0 {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	// The kernel fixes the initial Linux profile at large and refuses any
	// additional authority, even when PID 1 submits a valid handle for it.
	slot, data, ok = linuxProbeStartup(false)
	if !ok {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	if event, status := requestLaunch(executable, name, grants, supervisor.ProfileNormal, slot, uint32(len(data))); status != supervisor.LaunchCapabilityDenied || event.Process != 0 {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	slot, data, ok = linuxProbeStartup(true)
	extra := linuxProbeGrants[0][:5]
	if !ok {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	if event, status := requestLaunch(executable, name, extra, supervisor.ProfileLarge, slot, uint32(len(data))); status != supervisor.LaunchCapabilityDenied || event.Process != 0 {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	slot, data, ok = linuxProbeStartup(false)
	if !ok {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	first, status := requestLaunch(executable, name, grants, supervisor.ProfileLarge, slot, uint32(len(data)))
	if status != supervisor.LaunchOK || first.Process == 0 || first.ProcessHandle == 0 {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	secondStreams, secondGrants, secondVFS, prepared := prepareLinuxProbe(1)
	if !prepared {
		return false
	}
	slot, data, ok = linuxProbeStartup(false)
	if !ok {
		closeProbeStreams(secondStreams, false)
		sys.HandleClose(secondVFS)
		return false
	}
	second, status := requestLaunch(executable, name, secondGrants, supervisor.ProfileLarge, slot, uint32(len(data)))
	if status != supervisor.LaunchOK || second.Process == 0 || second.ProcessHandle == 0 {
		closeProbeStreams(secondStreams, false)
		sys.HandleClose(secondVFS)
		return false
	}
	inputs := [2]string{"in\n", "on\n"}
	for index, endpoints := range []*[3][2]uint64{streams, secondStreams} {
		if !linuxProbeWrite(endpoints[0][1], inputs[index][:1]) {
			return false
		}
	}
	for _, endpoints := range []*[3][2]uint64{streams, secondStreams} {
		if !linuxProbeRead(endpoints[1][0], "L") {
			return false
		}
	}
	for index, endpoints := range []*[3][2]uint64{streams, secondStreams} {
		if !linuxProbeWrite(endpoints[0][1], inputs[index][1:]) {
			return false
		}
		sys.HandleClose(handle.ID(endpoints[0][1]))
		sys.HandleClose(handle.ID(endpoints[2][0]))
	}
	for index, endpoints := range []*[3][2]uint64{streams, secondStreams} {
		if !linuxProbeRead(endpoints[1][0], inputs[index]+"LINUX_MEMORY_TLS_OK\n") {
			return false
		}
		sys.HandleClose(handle.ID(endpoints[1][0]))
	}
	waitApplicationExit(first.ProcessHandle)
	waitApplicationExit(second.ProcessHandle)
	mark("LINUX_MEMORY_TLS_OK\n")
	mark("LINUX_RAW_SYSCALL_OK\n")
	return true
}

// linuxLongPath names a file PID 1 creates for the musl probe. At 235 bytes
// the path is too long for one VFS request message, so opening it proves the
// Linux bridge can attach a path the way native clients do.
var linuxLongPath [235]byte

// probeLinuxMusl runs the static musl fixture through libc startup. The
// fixture checks its arguments, environment, TLS, heap, VFS session, path
// resolution from its working directory, and in a scratch directory under
// /tmp file I/O, file status, stdio, directory listing, vector I/O, and
// naming, then its identity, the clocks, and a sleep. It prints one line per
// result. PID 1 accepts only the complete
// expected output and a zero exit status.
//
//go:noinline
func probeLinuxMusl() bool {
	path := []byte("/system/linux/musl-hello")
	executable, name, located := locateExecutable(path)
	if located != supervisor.LaunchOK || executable.Format != supervisor.ExecutableLinuxX8664 {
		return false
	}
	for index := range linuxLongPath {
		linuxLongPath[index] = 'p'
	}
	copy(linuxLongPath[:], "/tmp/")
	session, reason := openVFSSession()
	if reason != "" {
		return false
	}
	if reply, ok := callSession(session, vfs.Request{Operation: vfs.OperationCreate, Path: linuxLongPath[:]}); !ok || reply.Status != vfs.StatusOK {
		sys.HandleClose(session)
		return false
	}
	passed := runLinuxMusl(executable, name, path)
	reply, ok := callSession(session, vfs.Request{Operation: vfs.OperationUnlink, Path: linuxLongPath[:]})
	sys.HandleClose(session)
	return passed && ok && reply.Status == vfs.StatusOK
}

func runLinuxMusl(executable supervisor.Executable, name string, path []byte) bool {
	streams, grants, vfsHandle, prepared := prepareLinuxProbe(0)
	if !prepared {
		return false
	}
	slot, staging := stagingSlot()
	writer := startup.NewWriter(staging, 3, 2, 4)
	writer.Command(path)
	writer.CWD([]byte("/system/linux"))
	writer.Argument(path)
	writer.Argument([]byte("one"))
	writer.Argument([]byte("two"))
	writer.Variable([]byte("GOOS_LINUX_PROBE"), []byte("musl"))
	writer.Variable([]byte("GOOS_LINUX_LONG"), linuxLongPath[:])
	writer.Handle([]byte("in"), 0)
	writer.Handle([]byte("stdout"), 0)
	writer.Handle([]byte("stderr"), 0)
	writer.Handle([]byte("vfs"), 0)
	data, ok := writer.Finish()
	if !ok {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	launched, status := requestLaunch(executable, name, grants, supervisor.ProfileLarge, slot, uint32(len(data)))
	if status != supervisor.LaunchOK || launched.Process == 0 || launched.ProcessHandle == 0 {
		closeProbeStreams(streams, false)
		sys.HandleClose(vfsHandle)
		return false
	}
	sys.HandleClose(handle.ID(streams[0][1]))
	sys.HandleClose(handle.ID(streams[2][0]))
	printed := linuxProbeRead(streams[1][0], "argc=3\nargv[1]=one\nargv[2]=two\nvfs session ok\npaths ok\nfiles ok\nstatus ok\nstdio ok\nlisting ok\nvectors ok\nnames ok\nsystem ok\nhello from musl\n")
	sys.HandleClose(handle.ID(streams[1][0]))
	return waitApplicationExit(launched.ProcessHandle) == 0 && printed
}

func prepareLinuxProbe(index int) (*[3][2]uint64, []supervisor.Capability, handle.ID, bool) {
	if index < 0 || index >= len(linuxProbeStreams) {
		return nil, nil, 0, false
	}
	streams := &linuxProbeStreams[index]
	*streams = [3][2]uint64{}
	for streamIndex := range streams {
		if !createStream(&streams[streamIndex]) {
			closeProbeStreams(streams, false)
			return nil, nil, 0, false
		}
	}
	vfsHandle, reason := openVFSSession()
	if reason != "" {
		closeProbeStreams(streams, false)
		return nil, nil, 0, false
	}
	linuxProbeGrants[index] = [5]supervisor.Capability{
		{Name: "in", Handle: handle.ID(streams[0][0]), Type: handle.TypeStream, Rights: capability.Read | capability.Wait, Consume: true},
		{Name: "stdout", Handle: handle.ID(streams[1][1]), Type: handle.TypeStream, Rights: capability.Write | capability.Wait, Consume: true},
		{Name: "stderr", Handle: handle.ID(streams[2][1]), Type: handle.TypeStream, Rights: capability.Write | capability.Wait, Consume: true},
		{Name: "vfs", Handle: vfsHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
	}
	return streams, linuxProbeGrants[index][:4], vfsHandle, true
}

func linuxProbeWrite(endpoint uint64, data string) bool {
	written, result := sys.StreamWrite(handle.ID(endpoint), sys.Bytes(data))
	if result != kcall.StatusOK || written != len(data) {
		mark("LINUX_RAW_INPUT_FAILED\n")
		return false
	}
	return true
}

func linuxProbeRead(endpoint uint64, expected string) bool {
	received := 0
	for received < len(expected) {
		if sys.StreamWait(handle.ID(endpoint), 200) != kcall.StatusOK {
			mark("LINUX_RAW_WAIT_FAILED\n")
			return false
		}
		read, result := sys.StreamRead(handle.ID(endpoint), streamReadback[received:len(expected)])
		if result != kcall.StatusOK || read == 0 {
			mark("LINUX_RAW_OUTPUT_FAILED\n")
			return false
		}
		received += read
	}
	if !equalBytes(streamReadback[:received], expected) {
		mark("LINUX_RAW_OUTPUT_FAILED\n")
		return false
	}
	return true
}

func linuxProbeStartup(extra bool) (uint8, []byte, bool) {
	slot, staging := stagingSlot()
	handles := 4
	if extra {
		handles++
	}
	writer := startup.NewWriter(staging, 1, 0, handles)
	path := []byte("/system/linux/raw-probe")
	writer.Command(path)
	writer.CWD([]byte("/"))
	writer.Argument(path)
	writer.Handle([]byte("in"), 0)
	writer.Handle([]byte("stdout"), 0)
	writer.Handle([]byte("stderr"), 0)
	writer.Handle([]byte("vfs"), 0)
	if extra {
		writer.Handle([]byte("console"), 0)
	}
	data, ok := writer.Finish()
	return slot, data, ok
}

func closeProbeStreams(streams *[3][2]uint64, launched bool) {
	for index := range streams {
		if launched {
			peer := 0
			if index == 0 {
				peer = 1
			}
			sys.HandleClose(handle.ID(streams[index][peer]))
			continue
		}
		closePair(streams[index])
	}
}

func probeStreamEndpoint() bool {
	if !createStream(&streamPair) {
		return false
	}
	payload := "bounded-stream"
	if written, status := sys.StreamWrite(handle.ID(streamPair[1]), sys.Bytes(payload)); status != kcall.StatusOK || written != len(payload) {
		return false
	}
	if read, status := sys.StreamRead(handle.ID(streamPair[0]), streamReadback[:]); status != kcall.StatusOK || read != len(payload) || !equalBytes(streamReadback[:len(payload)], payload) {
		return false
	}
	if sys.HandleClose(handle.ID(streamPair[1])) != 0 {
		return false
	}
	if _, status := sys.StreamRead(handle.ID(streamPair[0]), streamReadback[:]); status != ipc.StatusPeerDied {
		return false
	}
	return sys.HandleClose(handle.ID(streamPair[0])) == 0
}

// probeSharedEndpoint checks, in the booted kernel, that stream ends are
// reference counted (ADR 0089). PID 1 sends a writer to itself attached twice,
// which leaves two handles naming one end: closing the first must not end the
// reader's input, closing the second must, and a closed or wrong-direction
// handle must be refused.
func probeSharedEndpoint() bool {
	var pair [2]uint64
	if !createStream(&streamPair) {
		return false
	}
	reader, writer := streamPair[0], streamPair[1]
	if !createChannel(&pair) {
		sys.HandleClose(handle.ID(writer))
		sys.HandleClose(handle.ID(reader))
		return false
	}
	ok := sharedEndpointHolds(pair, reader, writer)
	closePair(pair)
	sys.HandleClose(handle.ID(reader))
	return ok
}

func sharedEndpointHolds(pair [2]uint64, reader, writer uint64) bool {
	message := ipc.Message{ProtocolID: 1, Version: 1, Kind: ipc.KindNotification, HandleCount: 2}
	message.Handles[0], message.Handles[1] = handle.ID(writer), handle.ID(writer)
	if !sendMessage(pair[0], message) {
		sys.HandleClose(handle.ID(writer))
		return false
	}
	received, status := receive(pair[1], 0)
	if status != ipc.StatusOK || received.HandleCount != 2 || received.Handles[0] == received.Handles[1] {
		return false
	}
	first, second := uint64(received.Handles[0]), uint64(received.Handles[1])
	// The moved handle is gone: closing it again is refused.
	if sys.HandleClose(handle.ID(writer)) != ipc.StatusNotFound {
		return false
	}
	payload := "shared"
	if _, status := sys.StreamWrite(handle.ID(first), sys.Bytes(payload)); status != kcall.StatusOK || sys.HandleClose(handle.ID(first)) != 0 {
		sys.HandleClose(handle.ID(second))
		return false
	}
	// A writing end cannot be read through.
	_, wrongEnd := sys.StreamRead(handle.ID(second), streamReadback[:])
	read, readStatus := sys.StreamRead(handle.ID(reader), streamReadback[:])
	if wrongEnd != kcall.StatusPermissionDenied || readStatus != kcall.StatusOK || !equalBytes(streamReadback[:read], payload) {
		sys.HandleClose(handle.ID(second))
		return false
	}
	// One writer is still held, so the drained stream is empty, not ended.
	if _, status := sys.StreamRead(handle.ID(reader), streamReadback[:]); status != kcall.StatusResourceExhausted {
		sys.HandleClose(handle.ID(second))
		return false
	}
	if sys.HandleClose(handle.ID(second)) != 0 || sys.HandleClose(handle.ID(second)) != ipc.StatusNotFound {
		return false
	}
	if _, status := sys.StreamRead(handle.ID(reader), streamReadback[:]); status != ipc.StatusPeerDied {
		return false
	}
	// A closed handle is not an endpoint any more.
	_, closedStatus := sys.StreamWrite(handle.ID(second), streamReadback[:1])
	return closedStatus == kcall.StatusPermissionDenied
}

// probeMinimumCapacity is the smallest disk the boot test attaches; the probe
// writes below it and checks only that the device reports a sane size.
const probeMinimumCapacity = 64 << 20

func probeBlockService() bool {
	correlation++
	info, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationInfo}, correlation)
	if !ok || !sendMessage(blockClientHandle, info) {
		return false
	}
	message, status := receive(blockClientHandle, storageProbeDeadline)
	infoReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || infoReply.Status != blockproto.StatusOK || infoReply.Capacity < probeMinimumCapacity || infoReply.Capacity%512 != 0 || infoReply.BlockSize != 512 {
		return false
	}
	// The probe reads what is there, writes its pattern across a sector
	// boundary, reads it back, and puts the original bytes back: on a
	// partitioned disk these bytes lie in the GPT entry array, and on a
	// superfloppy in the reserved sectors.
	original, ok := probeRead()
	if !ok || !probeWrite([]byte("GBLKIO")) {
		return false
	}
	pattern, ok := probeRead()
	if !ok || !equalBytes(pattern[:], "GBLKIO") || !probeWrite(original[:]) {
		return false
	}
	restored, ok := probeRead()
	if !ok || restored != original {
		return false
	}
	correlation++
	flushRequest, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationFlush}, correlation)
	if !ok || !sendMessage(blockClientHandle, flushRequest) {
		return false
	}
	message, status = receive(blockClientHandle, storageProbeDeadline)
	flushReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || flushReply.Status != blockproto.StatusOK {
		return false
	}
	mark("BLOCK_SERVICE_IO_OK\n")
	return true
}

// probeOffset and probeLength place the probe's pattern across the boundary
// of sectors 7 and 8.
const (
	probeOffset = 4093
	probeLength = 6
)

func probeRead() ([probeLength]byte, bool) {
	var data [probeLength]byte
	correlation++
	request, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationRead, Offset: probeOffset, Length: probeLength}, correlation)
	if !ok || !sendMessage(blockClientHandle, request) {
		return data, false
	}
	message, status := receive(blockClientHandle, storageProbeDeadline)
	reply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || reply.Status != blockproto.StatusOK || len(reply.Data) != probeLength {
		return data, false
	}
	copy(data[:], reply.Data)
	return data, true
}

func probeWrite(data []byte) bool {
	correlation++
	request, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationWrite, Offset: probeOffset, Length: uint32(len(data)), Data: data}, correlation)
	if !ok || !sendMessage(blockClientHandle, request) {
		return false
	}
	message, status := receive(blockClientHandle, storageProbeDeadline)
	reply, valid := blockproto.DecodeReply(message)
	return status == ipc.StatusOK && valid && reply.Status == blockproto.StatusOK
}
