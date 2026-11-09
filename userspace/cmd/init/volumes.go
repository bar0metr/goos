//go:build goos_userspace

package main

// Additional filesystem volumes (ADR 0222). Each mounted volume is one
// supervised service instance, volume1 to volume4, running the driver the
// probe chose. PID 1 probes the partition through a read-only block endpoint,
// hands that same endpoint to the driver, creates a file cache for each
// launch, and attaches the instance to the VFS at its mount path. The policy
// decisions live in userspace/volume; this file does the I/O.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	configproto "goos/userspace/config"
	execproto "goos/userspace/exec"
	"goos/userspace/fsdetect"
	"goos/userspace/fsproto"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
	"goos/userspace/volume"
)

// volumeRuntime is what PID 1 holds for one slot between the policy
// decisions: the handles of the launch being prepared and the process that
// serves the volume.
type volumeRuntime struct {
	// blockClient, vfsClient and cache are the next launch's handles. The
	// block and VFS service ends move to the instance at launch; the VFS
	// client end and the cache move to the VFS with the attach.
	blockClient handle.ID
	vfsServer   handle.ID
	vfsClient   handle.ID
	cache       handle.ID
	prepared    bool
	registered  bool
	process     handle.ID
	pid         uint64
	// replyServer and replyCorrelation name the shell waiting for the
	// outcome of a mount, or zero. The driver is read from the installed
	// volume, which may finish after the request was handled, so the answer
	// goes out when the attach or the launch completes.
	replyServer      uint64
	replyCorrelation uint64
}

var (
	volumes        volume.Table
	volumeRuntimes [volume.Slots]volumeRuntime
	volumeWire     [ipc.WireMaxSize]byte
	volumeMessage  ipc.Message
	volumeWindow   [fsdetect.Window]byte
)

var volumeServiceNames = [volume.Slots]string{"volume1", "volume2", "volume3", "volume4"}

// cachePages is the size of a volume's file cache in pages.
const cachePages = fsproto.CacheSize / fsproto.PageSize

// volumeOfService returns the slot a service name serves.
func volumeOfService(name string) (uint8, bool) {
	for index, service := range volumeServiceNames {
		if service == name {
			return uint8(index + 1), true
		}
	}
	return 0, false
}

func runtimeOf(slot uint8) *volumeRuntime { return &volumeRuntimes[slot-1] }

func closeHandle(id *handle.ID) {
	if *id != 0 {
		sys.HandleClose(*id)
		*id = 0
	}
}

// dropPrepared closes the handles of a launch that will not happen.
func dropPrepared(runtime *volumeRuntime) {
	closeHandle(&runtime.blockClient)
	closeHandle(&runtime.vfsServer)
	closeHandle(&runtime.vfsClient)
	closeHandle(&runtime.cache)
	runtime.prepared = false
}

// blockCall makes one exchange with the block service over a volume
// endpoint.
func blockCall(endpoint handle.ID, request blockproto.Request) (blockproto.Reply, bool) {
	correlation++
	message, ok := blockproto.EncodeRequest(request, correlation)
	var size int
	if ok {
		size, ok = ipc.Encode(message, volumeWire[:])
	}
	if !ok || sys.IPCSend(endpoint, volumeWire[:size]) != kcall.StatusOK {
		return blockproto.Reply{}, false
	}
	if sys.IPCWaitReceive(endpoint, volumeWire[:], blockproto.RequestDeadline) != kcall.StatusOK {
		return blockproto.Reply{}, false
	}
	payloadSize := int(volumeWire[24]) | int(volumeWire[25])<<8
	if !ipc.DecodeInto(volumeWire[:ipc.WireHeaderSize+payloadSize+int(volumeWire[26])*8], &volumeMessage) {
		return blockproto.Reply{}, false
	}
	reply, valid := blockproto.DecodeReplyFrom(&volumeMessage)
	return reply, valid && volumeMessage.Correlation == correlation
}

// probeVolume asks the block service for a read-only endpoint on the slot's
// partition and reads the probe window through it. The endpoint is kept for
// the instance: the driver gets no more than the probe had.
func probeVolume(slot uint8) (fsdetect.Result, volume.Status) {
	runtime := runtimeOf(slot)
	closeHandle(&runtime.blockClient)
	var pair [2]uint64
	if !createChannel(&pair) {
		return fsdetect.Result{}, volume.Failed
	}
	correlation++
	grant := blockproto.VolumeGrant{Server: handle.ID(pair[1]), Slot: slot, Selector: volumes.Slot(slot).Request.Source}
	request, ok := blockproto.SetVolumeRequest(grant, correlation)
	if !ok || !sendMessage(uint64(blockControl[0]), request) {
		closePair(pair)
		return fsdetect.Result{}, volume.Failed
	}
	reply, received := receive(uint64(blockControl[0]), vfsSessionDeadline)
	status, valid := blockproto.DecodeSetDriverReply(reply)
	if received != ipc.StatusOK || !valid {
		sys.HandleClose(handle.ID(pair[0]))
		return fsdetect.Result{}, volume.Failed
	}
	client := handle.ID(pair[0])
	switch status {
	case blockproto.StatusOK:
	case blockproto.StatusInvalidArgument:
		sys.HandleClose(client)
		return fsdetect.Result{}, volume.PartitionInUse
	default:
		sys.HandleClose(client)
		return fsdetect.Result{}, volume.NoPartition
	}
	info, ok := blockCall(client, blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || info.Status != blockproto.StatusOK {
		sys.HandleClose(client)
		return fsdetect.Result{}, volume.NoPartition
	}
	window := volumeWindow[:min(uint64(len(volumeWindow)), info.Capacity)]
	for done := 0; done < len(window); {
		chunk := min(len(window)-done, blockproto.MaxData)
		read, ok := blockCall(client, blockproto.Request{Operation: blockproto.OperationRead, Offset: uint64(done), Length: uint32(chunk)})
		if !ok || read.Status != blockproto.StatusOK || len(read.Data) != chunk {
			sys.HandleClose(client)
			return fsdetect.Result{}, volume.Failed
		}
		copy(window[done:], read.Data)
		done += chunk
	}
	runtime.blockClient = client
	return fsdetect.Probe(window, info.Capacity), volume.OK
}

// prepareVolumeLaunch gets a slot ready to launch: a probed block endpoint,
// a fresh channel to the VFS and a fresh file cache. A replacement launch
// probes again and must find the same volume.
func prepareVolumeLaunch(slot uint8) volume.Status {
	runtime := runtimeOf(slot)
	if runtime.prepared {
		return volume.OK
	}
	if runtime.blockClient == 0 {
		result, status := probeVolume(slot)
		if status == volume.OK {
			status = volumes.Probed(slot, result)
		}
		if status != volume.OK {
			closeHandle(&runtime.blockClient)
			return status
		}
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		dropPrepared(runtime)
		return volume.Failed
	}
	runtime.vfsClient, runtime.vfsServer = handle.ID(pair[0]), handle.ID(pair[1])
	cache, status := sys.MemoryCreate(cachePages)
	if status != kcall.StatusOK {
		dropPrepared(runtime)
		return volume.Failed
	}
	runtime.cache, runtime.prepared = cache, true
	return volume.OK
}

func volumeCapabilities(runtime *volumeRuntime) []supervisor.Capability {
	return []supervisor.Capability{
		{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
		{Name: "vfs", Handle: runtime.vfsServer, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "block", Handle: runtime.blockClient, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		// The instance gets a copy; PID 1's handle moves to the VFS with
		// the attach.
		{Name: "cache", Handle: runtime.cache, Type: handle.TypeMemory, Rights: capability.Read | capability.Write},
	}
}

// launchVolumeSpec is the launch hook for volume services: a restart after a
// failure prepares new handles before the launch goes ahead.
func launchVolumeSpec(slot uint8, spec *supervisor.LaunchSpec) bool {
	runtime := runtimeOf(slot)
	if !runtime.prepared {
		if status := prepareVolumeLaunch(slot); status != volume.OK {
			syslog.Error("volume not relaunched", syslog.Uint("slot", uint64(slot)), syslog.Str("reason", status.String()))
			manager.Hold(volumeServiceNames[slot-1])
			// The mount stays and answers errors until it is unmounted.
			if state := volumes.Slot(slot); state.State != volume.Starting {
				state.State = volume.Offline
			}
			return false
		}
	}
	name := volumeServiceNames[slot-1]
	for _, grant := range volumeCapabilities(runtime) {
		if !renewGrant(spec, name, grant.Name, grant.Handle) {
			dropPrepared(runtime)
			return false
		}
	}
	runtime.prepared = false
	// The service ends are consumed by the launch; PID 1 forgets them.
	runtime.blockClient, runtime.vfsServer = 0, 0
	return true
}

// volumeLaunched attaches a started instance to the VFS.
func volumeLaunched(slot uint8, process handle.ID, pid uint64) {
	runtime := runtimeOf(slot)
	runtime.process, runtime.pid = keepProcess(runtime.process, process), pid
	generation := volumes.Launching(slot)
	var attach vfs.VolumeAttach
	attach.Slot, attach.Client, attach.Cache, attach.Generation = slot, runtime.vfsClient, runtime.cache, generation
	attach.SetPath(volumes.Slot(slot).Request.PathBytes())
	runtime.vfsClient, runtime.cache = 0, 0
	attachStatus := vfs.StatusBackendError
	correlation++
	request, ok := vfs.AttachVolumeRequest(attach, correlation)
	if ok {
		result, status, sent := filesystemRequests.Control(handle.ID(vfsControlHandle), &request, attachDeadline)
		if !sent {
			sys.HandleClose(attach.Client)
			sys.HandleClose(attach.Cache)
		}
		if status == kcall.StatusOK {
			attachStatus = result
		}
	} else {
		sys.HandleClose(attach.Client)
		sys.HandleClose(attach.Cache)
	}
	if volumes.Attached(slot, generation, attachStatus) == volume.OK {
		mark("VOLUME_MOUNT_OK\n")
		syslog.Info("volume mounted", syslog.Uint("slot", uint64(slot)), syslog.Str("path", string(volumes.Slot(slot).Request.PathBytes())),
			syslog.Str("type", volumes.Slot(slot).Driver.Name))
		finishMount(slot, volume.OK, vfs.StatusOK)
		return
	}
	syslog.Warn("volume refused", syslog.Uint("slot", uint64(slot)), syslog.Uint("status", uint64(attachStatus)))
	if volumes.Slot(slot).Restarts == 0 {
		// A first attach that fails is a refused mount, not a crash to
		// recover from.
		stopVolume(slot)
		finishMount(slot, volume.Refused, attachStatus)
		return
	}
	// A replacement the VFS refused leaves the mount offline: it keeps its
	// place until unmounted, and the instance is ended.
	volumes.Slot(slot).State = volume.Offline
	name := volumeServiceNames[slot-1]
	manager.Hold(name)
	if manager.PlanStop(name) == supervisor.StatusOK && runtime.process != 0 {
		sys.ProcessKill(runtime.process)
	}
	mark("VOLUME_OFFLINE\n")
}

// volumeLaunchFailed handles a launch of a volume instance that did not
// start. A mount waiting on it fails; a replacement is retried by the
// supervisor within its budget.
func volumeLaunchFailed(slot uint8) {
	runtime := runtimeOf(slot)
	if runtime.replyServer == 0 && volumes.Slot(slot).State != volume.Starting {
		return
	}
	syslog.Warn("volume service did not start", syslog.Uint("slot", uint64(slot)))
	stopVolume(slot)
	finishMount(slot, volume.LaunchFailed, vfs.StatusOK)
}

// finishMount answers the shell waiting for a slot's mount, if one is.
func finishMount(slot uint8, status volume.Status, detail vfs.Status) {
	runtime := runtimeOf(slot)
	if runtime.replyServer == 0 {
		return
	}
	if reply, ok := execproto.EncodeVolumeReply(uint8(status), uint8(detail), runtime.replyCorrelation); ok {
		sendMessage(runtime.replyServer, reply)
	}
	runtime.replyServer, runtime.replyCorrelation = 0, 0
}

// stopVolume ends a slot's instance after its mount went away, or after it
// was refused. Its exit releases the slot.
func stopVolume(slot uint8) {
	runtime := runtimeOf(slot)
	volumes.Stopping(slot)
	name := volumeServiceNames[slot-1]
	manager.Hold(name)
	if manager.PlanStop(name) == supervisor.StatusOK && runtime.process != 0 && sys.ProcessKill(runtime.process) == kcall.StatusOK {
		return
	}
	releaseVolumeSlot(slot)
}

func releaseVolumeSlot(slot uint8) {
	runtime := runtimeOf(slot)
	dropPrepared(runtime)
	closeHandle(&runtime.process)
	*runtime = volumeRuntime{registered: runtime.registered, replyServer: runtime.replyServer, replyCorrelation: runtime.replyCorrelation}
	volumes.Release(slot)
}

// mountVolume mounts request in a free slot. It reports a refusal found
// before the launch at once, and done false when the outcome will reach
// replyServer later, through finishMount.
func mountVolume(request volume.Request, replyServer, replyCorrelation uint64) (volume.Status, vfs.Status, bool) {
	slot, status := volumes.Reserve(request)
	if status != volume.OK {
		return status, vfs.StatusOK, true
	}
	runtime := runtimeOf(slot)
	if status = prepareVolumeLaunch(slot); status != volume.OK {
		releaseVolumeSlot(slot)
		return status, vfs.StatusOK, true
	}
	name := volumeServiceNames[slot-1]
	driver := volumes.Slot(slot).Driver
	if !runtime.registered {
		definition := supervisor.Definition{Name: name, Executable: driver.Executable, Dependencies: []string{"block", "virtio-blk", "vfs"},
			Capabilities: volumeCapabilities(runtime), Restart: supervisor.RestartOnFailure, FollowsDependencies: true}
		if manager.Register(definition) != supervisor.StatusOK {
			releaseVolumeSlot(slot)
			return volume.Failed, vfs.StatusOK, true
		}
		runtime.registered = true
	} else if manager.Rebase(name, driver.Executable, supervisionTicks) != supervisor.StatusOK {
		releaseVolumeSlot(slot)
		return volume.Failed, vfs.StatusOK, true
	}
	manager.Release(name)
	runtime.replyServer, runtime.replyCorrelation = replyServer, replyCorrelation
	manager.StartReady(supervisionTicks, launchService)
	return volume.OK, vfs.StatusOK, false
}

// unmountVolume detaches the volume at path and ends its instance.
func unmountVolume(path []byte) volume.Status {
	slot, found := volumes.Find(path)
	if !found {
		return volume.NotMounted
	}
	state := volumes.Slot(slot)
	if state.State == volume.Mounted || state.State == volume.Offline || state.State == volume.Lost {
		correlation++
		request, ok := vfs.DetachVolumeRequest(slot, state.Generation, correlation)
		if !ok {
			return volume.Failed
		}
		result, status, _ := filesystemRequests.Control(handle.ID(vfsControlHandle), &request, vfsSessionDeadline)
		switch {
		case status != kcall.StatusOK:
			return volume.Failed
		case result == vfs.StatusBusy:
			return volume.Busy
		case result != vfs.StatusOK && result != vfs.StatusNotFound:
			return volume.Failed
		}
	}
	stopVolume(slot)
	mark("VOLUME_UNMOUNT_OK\n")
	syslog.Info("volume unmounted", syslog.Uint("slot", uint64(slot)), syslog.Str("path", string(path)))
	return volume.OK
}

// volumeExited is called for every process exit before the supervisor sees
// it. A planned stop releases the slot; any other exit of a mounted volume's
// instance is a loss, which the supervisor's restart replaces within the
// budget and otherwise leaves offline.
func volumeExited(pid uint64) {
	for slot := uint8(1); slot <= volume.Slots; slot++ {
		runtime := runtimeOf(slot)
		if runtime.pid == 0 || runtime.pid != pid {
			continue
		}
		runtime.pid = 0
		closeHandle(&runtime.process)
		state := volumes.Slot(slot)
		if state.State == volume.Stopping {
			releaseVolumeSlot(slot)
			return
		}
		if !volumes.Lost(slot, state.Generation) && state.State != volume.Lost {
			manager.Hold(volumeServiceNames[slot-1])
			syslog.Error("volume offline", syslog.Uint("slot", uint64(slot)), syslog.Str("path", string(state.Request.PathBytes())))
			mark("VOLUME_OFFLINE\n")
		}
		return
	}
}

// volumeLost acts on the VFS's notice that a volume's instance stopped
// answering: the process is ended, and its exit starts the replacement.
func volumeLost(slot uint8, generation uint64) {
	state := volumes.Slot(slot)
	if state == nil || state.Generation != generation {
		return
	}
	syslog.Warn("volume service lost", syslog.Uint("slot", uint64(slot)))
	if runtime := runtimeOf(slot); runtime.process != 0 {
		sys.ProcessKill(runtime.process)
	}
}

// pollVolumeNotices takes the volume notices the VFS recovery endpoint
// delivered; pollFilesystemNotices has already drained the endpoint.
func pollVolumeNotices() {
	for {
		slot, generation, found := vfsRecoveryRequests.VolumeLost()
		if !found {
			return
		}
		volumeLost(slot, generation)
	}
}

// handleVolumeCommand answers a shell's mount or unmount.
func handleVolumeCommand(command execproto.VolumeCommand, correlationID uint64) {
	var status volume.Status
	detail := vfs.StatusOK
	if command.Mount {
		request, made := volume.MakeRequest(command.Type, command.Source, command.Path)
		status = made
		if made == volume.OK {
			done := false
			if status, detail, done = mountVolume(request, replying.execServer, correlationID); !done {
				return
			}
		}
	} else {
		status = unmountVolume(command.Path)
	}
	reply, ok := execproto.EncodeVolumeReply(uint8(status), uint8(detail), correlationID)
	if ok {
		sendMessage(replying.execServer, reply)
	}
}

// mountConfiguredVolumes mounts the volumes configuration names, once the
// configuration service runs: system.mount.1 to system.mount.4, each
// "TYPE SOURCE PATH" or empty.
func mountConfiguredVolumes() {
	session, ok := openConfigSession("system.mount.", configproto.Read)
	if !ok {
		return
	}
	defer sys.HandleClose(session)
	key := []byte("system.mount.0")
	for slot := byte(1); slot <= volume.Slots; slot++ {
		key[len(key)-1] = '0' + slot
		value, found := configText(session, key)
		if !found || len(value) == 0 {
			continue
		}
		fields, count := splitFields(value)
		if count != 3 {
			syslog.Warn("mount setting malformed", syslog.Str("key", string(key)))
			continue
		}
		request, status := volume.MakeRequest(fields[0], fields[1], fields[2])
		if status == volume.OK {
			status, _, _ = mountVolume(request, 0, 0)
		}
		if status != volume.OK {
			syslog.Error("configured volume not mounted", syslog.Str("key", string(key)), syslog.Str("reason", status.String()))
		}
	}
}

var configTextValue [configproto.MaxText]byte

// configText reads one text setting over session.
func configText(session handle.ID, key []byte) ([]byte, bool) {
	correlation++
	message, ok := configproto.EncodeRequest(configproto.Request{Operation: configproto.OperationGet, Key: key}, correlation)
	if !ok || !sendMessage(uint64(session), message) {
		return nil, false
	}
	received, status := receive(uint64(session), vfsSessionDeadline)
	if status != ipc.StatusOK || received.Correlation != correlation {
		return nil, false
	}
	reply, valid := configproto.DecodeReply(received)
	if !valid || reply.Status != configproto.StatusOK || reply.Value.Kind != configproto.KindText {
		return nil, false
	}
	return configTextValue[:copy(configTextValue[:], reply.Value.Bytes())], true
}

// splitFields splits value at spaces into at most three fields.
func splitFields(value []byte) ([3][]byte, int) {
	var fields [3][]byte
	count := 0
	for start := 0; start < len(value); {
		for start < len(value) && value[start] == ' ' {
			start++
		}
		end := start
		for end < len(value) && value[end] != ' ' {
			end++
		}
		if end > start {
			if count == len(fields) {
				return fields, count + 1
			}
			fields[count] = value[start:end]
			count++
		}
		start = end
	}
	return fields, count
}

// injectVolumeFault kills the instance of the lowest mounted slot, in the
// validation build, to show it replaced while the others keep serving.
func injectVolumeFault() bool {
	for slot := uint8(1); slot <= volume.Slots; slot++ {
		runtime := runtimeOf(slot)
		if volumes.Slot(slot).State == volume.Mounted && runtime.process != 0 {
			mark("VOLUME_FAULT_INJECTED\n")
			return sys.ProcessKill(runtime.process) == kcall.StatusOK
		}
	}
	return false
}
