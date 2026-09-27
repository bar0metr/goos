//go:build goos_userspace

// Command fsck-fat checks or explicitly repairs an unmounted FAT32 volume
// through an exclusive block-slice capability.
package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/block"
	"goos/userspace/commands"
	"goos/userspace/fat32"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "fsck-fat (--check|--repair) auto",
	Summary: "check or explicitly repair an unmounted FAT32 volume",
	MinArgs: 2,
	MaxArgs: 2,
	Grants:  commands.GrantBlockCheck,
}

var (
	blockHandle handle.ID
	wire        [ipc.WireMaxSize]byte
	correlation uint64
	line        [160]byte
)

type blockDevice struct{ capacity uint64 }

func (device *blockDevice) Capacity() uint64 { return device.capacity }
func (*blockDevice) Flush() bool {
	reply, ok := exchange(block.Request{Operation: block.OperationFlush})
	return ok && reply.Status == block.StatusOK
}

func (device *blockDevice) ReadAt(offset uint64, target []byte) bool {
	for done := 0; done < len(target); {
		chunk := len(target) - done
		if chunk > block.MaxData {
			chunk = block.MaxData
		}
		reply, ok := exchange(block.Request{Operation: block.OperationRead, Offset: offset + uint64(done), Length: uint32(chunk)})
		if !ok || reply.Status != block.StatusOK || len(reply.Data) != chunk {
			return false
		}
		copy(target[done:], reply.Data)
		done += chunk
	}
	return true
}

func (device *blockDevice) WriteAt(offset uint64, data []byte) bool {
	for done := 0; done < len(data); {
		chunk := len(data) - done
		if chunk > block.MaxData {
			chunk = block.MaxData
		}
		reply, ok := exchange(block.Request{Operation: block.OperationWrite, Offset: offset + uint64(done), Data: data[done : done+chunk]})
		if !ok || reply.Status != block.StatusOK {
			return false
		}
		done += chunk
	}
	return true
}

type printer struct{}

func (printer) Issue(issue fat32.CheckIssue) {
	text := append(line[:0], "error "...)
	text = append(text, issue.Code.String()...)
	text = append(text, " cluster="...)
	text = utility.AppendUint(text, uint64(issue.Cluster))
	text = append(text, " entry="...)
	text = utility.AppendUint(text, uint64(issue.Entry))
	text = append(text, " value="...)
	text = utility.AppendUint(text, uint64(issue.Value))
	utility.Print(text)
	utility.EndLine()
}

func (printer) Repair(action fat32.RepairAction) {
	text := append(line[:0], "repair "...)
	text = append(text, action.Code.String()...)
	text = append(text, " cluster="...)
	text = utility.AppendUint(text, uint64(action.Cluster))
	text = append(text, " entry="...)
	text = utility.AppendUint(text, uint64(action.Entry))
	text = append(text, " value="...)
	text = utility.AppendUint(text, uint64(action.Value))
	utility.Print(text)
	utility.EndLine()
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	mode, _ := data.Arg(1)
	volume, _ := data.Arg(2)
	checking, repairing := utility.Equal(mode, "--check"), utility.Equal(mode, "--repair")
	if (!checking && !repairing) || !utility.Equal(volume, "auto") {
		utility.Usage()
	}
	var ok bool
	blockHandle, ok = data.Handle("block")
	if !ok {
		utility.Fail(nil, "exclusive block slice unavailable")
	}
	info, ok := exchange(block.Request{Operation: block.OperationInfo})
	if !ok || info.Status != block.StatusOK || info.Capacity == 0 {
		utility.Fail(nil, "block slice unavailable")
	}
	device := blockDevice{capacity: info.Capacity}
	var report fat32.CheckReport
	repaired := uint32(0)
	if repairing {
		result := fat32.Repair(&device, printer{}, printer{})
		report, repaired = result.After, result.Repaired
		if result.Status != 0 {
			report.Status = result.Status
		}
	} else {
		report = fat32.Check(&device, printer{})
	}
	text := append(line[:0], "files="...)
	text = utility.AppendUint(text, uint64(report.Files))
	text = append(text, " directories="...)
	text = utility.AppendUint(text, uint64(report.Directories))
	text = append(text, " used-clusters="...)
	text = utility.AppendUint(text, uint64(report.UsedClusters))
	text = append(text, " free-clusters="...)
	text = utility.AppendUint(text, uint64(report.FreeClusters))
	text = append(text, " errors="...)
	text = utility.AppendUint(text, uint64(report.Issues))
	if repairing {
		text = append(text, " repaired="...)
		text = utility.AppendUint(text, uint64(repaired))
	}
	utility.Print(text)
	utility.EndLine()
	if report.Status != 0 || report.Issues != 0 {
		utility.Exit(1)
	}
	if repairing {
		utility.Mark("FSCK_FAT_REPAIR_OK\n")
	} else {
		utility.Mark("FSCK_FAT_CHECK_OK\n")
	}
}

func exchange(request block.Request) (block.Reply, bool) {
	correlation++
	message, ok := block.EncodeRequest(request, correlation)
	if !ok {
		return block.Reply{}, false
	}
	size, ok := ipc.Encode(message, wire[:])
	if !ok || sys.IPCSend(blockHandle, wire[:size]) != kcall.StatusOK || sys.IPCWaitReceive(blockHandle, wire[:], block.RequestDeadline) != kcall.StatusOK {
		return block.Reply{}, false
	}
	received, ok := ipc.Decode(wire[:wireSize(wire[:])])
	reply, valid := block.DecodeReply(received)
	return reply, ok && valid && received.Correlation == correlation
}

func wireSize(data []byte) int {
	size := ipc.WireHeaderSize + (int(data[24]) | int(data[25])<<8) + int(data[26])*8
	if size > len(data) {
		return len(data)
	}
	return size
}
