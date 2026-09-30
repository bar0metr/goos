//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	kernelconsole "goos/kernel/console"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/stream"
	kernelwait "goos/kernel/wait"
)

var diagRing diag.Ring

func trace(kind diag.Kind, pid process.ID, task uint64, object uint64, value uint64) {
	if !diag.Enabled {
		return
	}
	diagRing.Record(diag.Event{Time: nowNanos(), Kind: kind, Process: uint64(pid), Task: task, Object: object, Value: value})
}

func traceWaitKey(kind diag.Kind, pid process.ID, task sched.TaskID, key kernelwait.Key, value uint64) {
	trace(kind, pid, uint64(task), key.Object|uint64(key.Side)<<63, value)
}

// traceDeniedInterests reports where a rejected interest array lives and who
// else can reach that physical page, so memory aliasing can be told apart from
// a genuinely stale handle.
func traceDeniedInterests(root, address, count uint64) {
	out := diagWriter{console: kernelconsole.New()}
	out.text("DIAG denied wire=")
	for index := uint64(0); index < count*kcall.WaitAnyInterestSize && index < uint64(len(waitAnyWire)); index += 8 {
		out.console.WriteHex(decodeUint64(waitAnyWire[index : index+8]))
		out.text(",")
	}
	out.end()
	traceAddress(out, root, address)
}

// traceAddress reports the physical page behind a user address, whether the
// allocator and kernel image own it, and every other mapping or kernel stack
// that reaches the same page.
func traceAddress(out diagWriter, root, address uint64) {
	physical, mapped := vm.ResolveUser(root, address)
	page := physical &^ (vm.PageSize - 1)
	out.text("DIAG address")
	out.hex("va", address)
	out.hex("pa", physical)
	out.uint("mapped", boolValue(mapped))
	out.uint("pmm-allocated", boolValue(pageAllocator.IsAllocated(page)))
	image, valid := vm.LoadedImage()
	out.uint("in-kernel-image", boolValue(valid && page >= vm.KernelImageBase && page < vm.KernelImageBase+image.Size))
	out.end()
	if !mapped {
		return
	}
	for index := 0; ; index++ {
		other, found := processRegistry.At(index)
		if !found {
			break
		}
		if other.AddressSpace().Root == root {
			continue
		}
		if virtual, aliased := vm.FindUserMapping(other.AddressSpace().Root, page); aliased {
			out.text("DIAG alias")
			out.uint("pid", uint64(other.ID()))
			out.hex("va", virtual)
			out.end()
		}
	}
	state := amd64.ReadSchedulerState()
	for slot := uint64(0); slot < state.Count; slot++ {
		top := amd64.TaskStackTop(slot)
		if top != 0 && page < top && page+vm.PageSize > top-4*vm.PageSize-vm.PageSize {
			out.text("DIAG alias-kernel-stack")
			out.uint("slot", slot)
			out.hex("top", top)
			out.end()
		}
	}
	auditAddressSpace(out, root)
	var bytes [64]byte
	if vm.CopyIn(root, address, bytes[:]) {
		out.text("DIAG bytes=")
		for index := 0; index < len(bytes); index += 8 {
			out.console.WriteHex(decodeUint64(bytes[index : index+8]))
			out.text(",")
		}
		out.end()
	}
}

// auditAddressSpace reports every user page of a process that the allocator
// does not own, that lies in the kernel image, or that another process maps.
func auditAddressSpace(out diagWriter, root uint64) {
	image, _ := vm.LoadedImage()
	leaves, flagged := 0, 0
	for index := 0; ; index++ {
		virtual, physical, found := vm.UserLeaf(root, index)
		if !found {
			break
		}
		leaves++
		allocated := pageAllocator.IsAllocated(physical)
		inImage := physical >= vm.KernelImageBase && physical < vm.KernelImageBase+image.Size
		sharedWith := uint64(0)
		for other := 0; ; other++ {
			candidate, exists := processRegistry.At(other)
			if !exists {
				break
			}
			if candidate.AddressSpace().Root == root {
				continue
			}
			if _, aliased := vm.FindUserMapping(candidate.AddressSpace().Root, physical); aliased {
				sharedWith = uint64(candidate.ID())
			}
		}
		if allocated && !inImage && sharedWith == 0 {
			continue
		}
		flagged++
		out.text("DIAG audit")
		out.hex("va", virtual)
		out.hex("pa", physical)
		out.uint("pmm-allocated", boolValue(allocated))
		out.uint("in-kernel-image", boolValue(inImage))
		out.uint("shared-with", sharedWith)
		out.end()
	}
	out.text("DIAG audit")
	out.uint("leaves", uint64(leaves))
	out.uint("flagged", uint64(flagged))
	out.end()
}

func pidOfTask(task sched.TaskID) process.ID {
	for index := 0; ; index++ {
		owner, found := processRegistry.At(index)
		if !found {
			return 0
		}
		if owner.OwnsTask(task) {
			return owner.ID()
		}
	}
}

// Diagnostics prints a kernel state dump on the serial console in goos_diag
// builds. The caller needs the same console authority it would need to print
// the dump itself.
func (handler nativeCallHandler) Diagnostics(device uint64, address uint64) kcall.Status {
	if !diag.Enabled {
		return kcall.StatusUnsupported
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, capability.Write)
	if !allowed || authority.Object() != 1 {
		return kcall.StatusPermissionDenied
	}
	dumpDiagnostics(owner.ID())
	if address != 0 {
		traceAddress(diagWriter{console: kernelconsole.New()}, handler.root, address)
	}
	return kcall.StatusOK
}

type diagWriter struct{ console kernelconsole.Writer }

func (writer diagWriter) text(value string) { writer.console.WriteString(value) }

func (writer diagWriter) hex(name string, value uint64) {
	writer.console.WriteString(" ")
	writer.console.WriteString(name)
	writer.console.WriteString("=")
	writer.console.WriteHex(value)
}

func (writer diagWriter) uint(name string, value uint64) {
	writer.console.WriteString(" ")
	writer.console.WriteString(name)
	writer.console.WriteString("=")
	writer.console.WriteUint(value)
}

func (writer diagWriter) word(name, value string) {
	writer.console.WriteString(" ")
	writer.console.WriteString(name)
	writer.console.WriteString("=")
	writer.console.WriteString(value)
}

func (writer diagWriter) end() { writer.console.WriteString("\n") }

var handleTypeNames = [...]string{"invalid", "process", "task", "memory", "channel", "device", "irq", "stream"}

func dumpDiagnostics(requester process.ID) {
	out := diagWriter{console: kernelconsole.New()}
	state := amd64.ReadSchedulerState()
	out.text("DIAG begin")
	out.uint("tick", amd64.TimerTicks())
	out.uint("hz", clock.TicksPerSecond)
	out.uint("requester", uint64(requester))
	out.end()
	out.text("DIAG sched")
	out.hex("runnable", state.Runnable)
	out.hex("dead", state.Dead)
	out.uint("slots", state.Count)
	out.uint("current", state.Current)
	out.end()
	for index := 0; ; index++ {
		owner, found := processRegistry.At(index)
		if !found {
			break
		}
		dumpProcess(out, owner, state)
	}
	for index := 0; index < ipc.MaxChannels; index++ {
		id, channel, found := channelRegistry.At(index)
		if !found {
			continue
		}
		out.text("DIAG channel")
		out.hex("id", id)
		out.uint("alive0", boolValue(channel.Alive(0)))
		out.uint("alive1", boolValue(channel.Alive(1)))
		out.uint("queued0", uint64(channel.Queued(0)))
		out.uint("queued1", uint64(channel.Queued(1)))
		out.uint("pending0", uint64(channel.Pending(0)))
		out.uint("pending1", uint64(channel.Pending(1)))
		for side := uint8(0); side <= 1; side++ {
			object, _ := ipc.EndpointObject(id, side)
			holder, held := processRegistry.ByCapabilityObject(handle.TypeChannel, object)
			if held {
				out.uint(sideName(side), uint64(holder.ID()))
			}
		}
		out.end()
	}
	for slot := 0; slot < sched.MaxTasks; slot++ {
		view, found := waitAnyTable.At(slot)
		if !found {
			continue
		}
		out.text("DIAG waitany")
		out.hex("task", uint64(view.Task))
		out.uint("pid", uint64(pidOfTask(view.Task)))
		switch {
		case view.TimedOut:
			out.word("state", "timed-out")
		case view.Complete:
			out.word("state", "complete")
		default:
			out.word("state", "active")
		}
		out.uint("deadline", view.Deadline)
		out.end()
		for entry := 0; entry < view.Count; entry++ {
			out.text("DIAG   interest")
			out.uint("kind", uint64(view.Entries[entry].Key.Kind))
			out.hex("object", view.Entries[entry].Key.Object)
			out.uint("side", uint64(view.Entries[entry].Key.Side))
			out.hex("events", uint64(view.Entries[entry].Events))
			out.hex("ready", uint64(view.Ready[entry]))
			out.end()
		}
	}
	out.text("DIAG waits")
	out.uint("ipc", uint64(ipcWaitSet.Count()))
	out.uint("stream", uint64(streamWaitSet.Count()))
	out.uint("process", uint64(processWaitSet.Count()))
	out.uint("waitany", uint64(waitAnyTable.Count()))
	out.uint("lifecycle-pending", uint64(pendingLifecycleCount))
	out.uint("lifecycle-overflows", lifecycleOverflows)
	out.end()
	unsupported := linuxcompat.Unsupported()
	sessions := linuxcompat.VFSStatistics()
	out.text("DIAG linux")
	out.uint("syscalls", linuxSyscallEntries)
	out.uint("unsupported", unsupported.Count)
	out.uint("last-unsupported", uint64(unsupported.Last()))
	// The numbers most recently refused, newest last, so a run shows which
	// calls a program missed and not only the final one.
	out.console.WriteString(" recent-unsupported=")
	for index := 0; index < int(unsupported.Stored); index++ {
		if index != 0 {
			out.console.WriteString(",")
		}
		out.console.WriteUint(uint64(unsupported.Recent(index)))
	}
	// Every call number ever refused, in ascending order.
	out.console.WriteString(" refused=")
	first := true
	for number := uint64(0); number < linuxcompat.RefusedNumbers; number++ {
		if unsupported.Refused[number/64]&(1<<(number%64)) == 0 {
			continue
		}
		if !first {
			out.console.WriteString(",")
		}
		out.console.WriteUint(number)
		first = false
	}
	faults, faulted := linuxcompat.BadPointers()
	out.uint("bad-pointers", faults)
	out.uint("last-bad-pointer", uint64(faulted))
	out.uint("vfs-requests", sessions.Requests)
	out.uint("vfs-failures", sessions.Failures)
	out.uint("vfs-stale", sessions.Stale)
	out.end()
	dumpTiming(out)
	out.text("DIAG events")
	out.uint("total", diagRing.Total())
	out.end()
	for index := 0; index < diagRing.Len(); index++ {
		event := diagRing.At(index)
		out.text("DIAG event")
		out.uint("us", event.Time/1000)
		out.word("kind", event.Kind.String())
		out.uint("pid", event.Process)
		out.hex("task", event.Task)
		out.hex("object", event.Object)
		if event.Kind == diag.WaitDenied {
			out.word("reason", diag.Deny(event.Value).String())
		} else {
			out.hex("value", event.Value)
		}
		out.end()
	}
	out.text("DIAG end\n")
}

func dumpProcess(out diagWriter, owner *process.Process, state amd64.SchedulerState) {
	out.text("DIAG process")
	out.uint("pid", uint64(owner.ID()))
	out.hex("root", owner.AddressSpace().Root)
	for _, task := range owner.Tasks() {
		slot, _, valid := task.Parts()
		taskState := "invalid"
		if valid {
			switch {
			case state.Dead&(1<<slot) != 0:
				taskState = "dead"
			case state.Runnable&(1<<slot) != 0:
				taskState = "runnable"
			default:
				taskState = "blocked"
			}
		}
		out.hex("task", uint64(task))
		out.word("state", taskState)
	}
	out.uint("free-handles", uint64(owner.Handles().Available()))
	out.end()
	for index := 0; index < handle.MaxHandles; index++ {
		id, typeID, authority, found := owner.Handles().At(index)
		if !found {
			continue
		}
		out.text("DIAG   handle")
		out.hex("id", uint64(id))
		if int(typeID) < len(handleTypeNames) {
			out.word("type", handleTypeNames[typeID])
		}
		out.hex("rights", uint64(authority.Rights()))
		switch typeID {
		case handle.TypeChannel:
			channelID, side, valid := ipc.DecodeEndpointObject(authority.Object())
			out.hex("channel", channelID)
			out.uint("side", uint64(side))
			if channel, exists := channelRegistry.Lookup(channelID); valid && exists {
				out.uint("peer-alive", boolValue(channel.Alive(side^1)))
				out.uint("queued", uint64(channel.Queued(side)))
			} else {
				out.word("channel-state", "released")
			}
		case handle.TypeStream:
			streamID, writer, _ := stream.DecodeEndpointObject(authority.Object())
			out.hex("stream", streamID)
			out.uint("writer", boolValue(writer))
			if endpoint, exists := streamRegistry.Lookup(streamID); exists {
				readers, writers := endpoint.References()
				out.uint("readers", uint64(readers))
				out.uint("writers", uint64(writers))
			}
		default:
			out.hex("object", authority.Object())
		}
		out.end()
	}
}

func sideName(side uint8) string {
	if side == 0 {
		return "holder0"
	}
	return "holder1"
}

func boolValue(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}
