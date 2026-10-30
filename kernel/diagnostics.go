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
	"goos/kernel/lock"
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
func traceDeniedInterests(root, address, count, interestSize uint64) {
	local := thisCPU()
	out := diagWriter{console: kernelconsole.New()}
	out.text("DIAG denied wire=")
	for index := uint64(0); index < count*interestSize && index < uint64(len(local.waitAnyWire)); index += 8 {
		out.console.WriteHex(decodeUint64(local.waitAnyWire[index : index+8]))
		out.text(",")
	}
	out.end()
	traceAddress(out, root, address)
}

// traceAddress reports the physical page behind a user address, whether the
// allocator and kernel image own it, and every other mapping or kernel stack
// that reaches the same page. The caller holds the registry and root's
// address space for reading. Other processes' page tables are read without
// their locks, since a processor may hold one address-space lock at a time;
// a mapping another processor removes meanwhile may still be reported.
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

// pidOfTask names a task's process for a trace record. Traces are taken
// under any locks, so it reads the task-to-process map without the
// registry's lock; a task that exits meanwhile may be named 0.
func pidOfTask(task sched.TaskID) process.ID {
	if owner, found := processRegistry.Owner(task); found {
		return owner.ID()
	}
	return 0
}

// Diagnostics prints a kernel state dump on the serial console in goos_diag
// builds. The caller needs the same console authority it would need to print
// the dump itself.
func (handler nativeCallHandler) Diagnostics(device uint64, address uint64) kcall.Status {
	if !diag.Enabled {
		return kcall.StatusUnsupported
	}
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := lookupHandle(owner, handle.ID(device), handle.TypeDevice, capability.Write)
	if !allowed || authority.Object() != 1 {
		return kcall.StatusPermissionDenied
	}
	dumpDiagnostics(owner.ID())
	if address != 0 {
		registryLock.RLock()
		owner.Space().RLock()
		traceAddress(diagWriter{console: kernelconsole.New()}, handler.root, address)
		owner.Space().RUnlock()
		registryLock.RUnlock()
	}
	return kcall.StatusOK
}

// The dump prints slowly, over the serial port, so it holds no lock across
// the whole of it: each process and each channel is printed under the
// locks of its own domains, and other calls run between them. A dump of a
// busy machine is therefore not one instant's state.
const (
	processDumpLocks = handleRead | streams
	channelDumpLocks = registryRead
)

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
	out.hex("running", state.Running)
	out.hex("online", state.Online)
	out.uint("switches", state.Stats.Switches)
	out.uint("steals", state.Stats.Steals)
	out.uint("migrations", state.Stats.Migrations)
	out.uint("kills", state.Stats.Kills)
	out.uint("parallel-peak", amd64.ParallelPeak())
	out.uint("kernel-parallel-peak", amd64.KernelParallelPeak())
	out.uint("kernel-thread-peak", amd64.KernelThreadPeak())
	out.uint("lock-contention", lock.Contended())
	shootdowns, targets, timeouts := amd64.ShootdownStats()
	out.uint("shootdowns", shootdowns)
	out.uint("shootdown-ipis", targets)
	out.uint("shootdown-timeouts", timeouts)
	out.end()
	// One line per lock level ever taken; spin times are TSC cycles.
	for level := lock.Level(1); int(level) < lock.Levels; level++ {
		stats := lock.Statistics(level)
		if stats.Acquired == 0 {
			continue
		}
		out.text("DIAG lock")
		out.console.WriteString(" level=")
		out.console.WriteString(level.String())
		out.uint("acquired", stats.Acquired)
		out.uint("contended", stats.Contended)
		out.uint("spin", stats.Spin)
		out.uint("longest", stats.Longest)
		out.uint("stuck", stats.Stuck)
		out.end()
	}
	for cpu := 0; cpu < amd64.MaxCPUs; cpu++ {
		if state.Online&(uint64(1)<<uint(cpu)) == 0 {
			continue
		}
		counters := amd64.ProcessorCounters(cpu)
		out.text("DIAG cpu")
		out.uint("index", uint64(cpu))
		out.uint("ticks", counters.Ticks)
		out.uint("idle", amd64.IdleTicks(cpu))
		out.uint("resched-ipis", counters.RescheduleIPIs)
		out.uint("shootdown-ipis", counters.ShootdownIPIs)
		out.end()
	}
	for index := 0; ; index++ {
		registryLock.RLock()
		owner, found := processRegistry.At(index)
		if found {
			// The registry stays held, so the process stays.
			held := takeLocks(processDumpLocks, owner)
			dumpProcess(out, owner, state)
			held.drop()
		}
		registryLock.RUnlock()
		if !found {
			break
		}
	}
	for index := 0; index < ipc.MaxChannels; index++ {
		held := takeLocks(channelDumpLocks, nil)
		dumpChannel(out, index)
		held.drop()
	}
	for slot := 0; slot < sched.MaxTasks; slot++ {
		view := &thisCPU().waitAnyView
		if !waitAnyTable.Inspect(slot, view) {
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
	lifecycleLock.Lock()
	pending, overflows := pendingLifecycleCount, lifecycleOverflows
	lifecycleLock.Unlock()
	out.uint("lifecycle-pending", uint64(pending))
	out.uint("lifecycle-overflows", overflows)
	out.end()
	unsupported := linuxcompat.Unsupported()
	sessions := linuxcompat.VFSStatistics()
	out.text("DIAG linux")
	out.uint("syscalls", linuxSyscallEntries.Load())
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
	for index := 0; index < linuxcompat.MaxTerminals; index++ {
		id, terminal, live := linuxcompat.TerminalAt(index)
		if !live {
			continue
		}
		out.text("DIAG linux-tty")
		out.hex("id", uint64(id))
		out.uint("family", terminal.FamilyID)
		out.uint("session", uint64(terminal.Session))
		out.uint("foreground", uint64(terminal.Foreground))
		out.uint("rows", uint64(terminal.Rows))
		out.uint("columns", uint64(terminal.Columns))
		out.hex("iflag", uint64(terminal.Settings.Input))
		out.hex("oflag", uint64(terminal.Settings.Output))
		out.hex("lflag", uint64(terminal.Settings.Local))
		out.uint("hangup", boolDiag(terminal.Hangup))
		out.uint("interrupts", terminal.Stats.Interrupts)
		out.uint("suspends", terminal.Stats.Suspends)
		out.uint("quits", terminal.Stats.Quits)
		out.uint("resizes", terminal.Stats.Resizes)
		out.hex("last-refused-ioctl", terminal.Stats.LastUnsupported)
		out.end()
	}
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
			if channel, exists := channelRegistry.Acquire(channelID); valid && exists {
				peerAlive, queued := channel.Alive(side^1), channel.Queued(side)
				channel.Unlock()
				out.uint("peer-alive", boolValue(peerAlive))
				out.uint("queued", uint64(queued))
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

// dumpChannel prints the channel in slot index, if one is there. The
// registry is held; the channel's lock is held only to read its state.
func dumpChannel(out diagWriter, index int) {
	id, channel, found := channelRegistry.AcquireAt(index)
	if !found {
		return
	}
	alive0, alive1 := channel.Alive(0), channel.Alive(1)
	queued0, queued1 := channel.Queued(0), channel.Queued(1)
	pending0, pending1 := channel.Pending(0), channel.Pending(1)
	channel.Unlock()
	out.text("DIAG channel")
	out.hex("id", id)
	out.uint("alive0", boolValue(alive0))
	out.uint("alive1", boolValue(alive1))
	out.uint("queued0", uint64(queued0))
	out.uint("queued1", uint64(queued1))
	out.uint("pending0", uint64(pending0))
	out.uint("pending1", uint64(pending1))
	for side := uint8(0); side <= 1; side++ {
		object, _ := ipc.EndpointObject(id, side)
		holder, held := processRegistry.EndpointHolder(object)
		if held {
			out.uint(sideName(side), uint64(holder.ID()))
		}
	}
	out.end()
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

func boolDiag(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}
