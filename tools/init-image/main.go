package main

import (
	"encoding/binary"
	"fmt"
	"goos/kernel/ipc"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	supervisor "goos/userspace/init"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: init-image OUTPUT")
		os.Exit(2)
	}
	const lifecycleHandle = uint64(0x0000000100000001)
	processMessage := []byte("INIT_PROCESS_OK\n")
	lifecycleMessage := []byte("INIT_LIFECYCLE_IPC_OK\n")
	launchMessage := []byte("INIT_SERVICE_LAUNCH_OK\n")
	startedMessage := []byte("INIT_SERVICE_STARTED_OK\n")
	request, ok := supervisor.LaunchRequest(3, "/system/fault-probe", 1)
	if !ok {
		fmt.Fprintln(os.Stderr, "encode launch request failed")
		os.Exit(1)
	}
	var requestWire [ipc.WireMaxSize]byte
	requestSize, ok := ipc.Encode(request, requestWire[:])
	if !ok {
		fmt.Fprintln(os.Stderr, "encode launch wire failed")
		os.Exit(1)
	}
	data := make([]byte, 0, len(processMessage)+len(lifecycleMessage)+len(launchMessage)+len(startedMessage)+requestSize)
	processOffset := uint64(len(data))
	data = append(data, processMessage...)
	lifecycleOffset := uint64(len(data))
	data = append(data, lifecycleMessage...)
	launchOffset := uint64(len(data))
	data = append(data, launchMessage...)
	startedOffset := uint64(len(data))
	data = append(data, startedMessage...)
	requestOffset := uint64(len(data))
	data = append(data, requestWire[:requestSize]...)
	program := make([]byte, 0, 160)
	program = debugWrite(program, vm.UserDataAddress+processOffset, uint32(len(processMessage)))
	program = kernelCall3(program, 5, lifecycleHandle, vm.UserStackAddress, 24)
	program = requireOK(program)
	program = debugWrite(program, vm.UserDataAddress+lifecycleOffset, uint32(len(lifecycleMessage)))
	program = kernelCall3(program, 3, lifecycleHandle, vm.UserDataAddress+requestOffset, uint32(requestSize))
	program = requireOK(program)
	program = debugWrite(program, vm.UserDataAddress+launchOffset, uint32(len(launchMessage)))
	program = kernelCall3(program, 5, lifecycleHandle, vm.UserStackAddress, 24)
	program = requireOK(program)
	program = debugWrite(program, vm.UserDataAddress+startedOffset, uint32(len(startedMessage)))
	program = append(program, 0xf3, 0x90, 0xeb, 0xfc)
	stack := make([]byte, 24)
	binary.LittleEndian.PutUint64(stack[0:8], vm.UserStackAddress+64)
	binary.LittleEndian.PutUint64(stack[8:16], ipc.WireMaxSize)
	binary.LittleEndian.PutUint64(stack[16:24], 100000)
	storage := make([]byte, 2048)
	image, ok := loader.EncodeNative(storage, loader.ImageSource{
		Entry:    vm.UserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: vm.UserCodeAddress, MemorySize: uint32(len(program)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: program},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable, Data: stack},
			{VirtualAddress: vm.UserDataAddress, MemorySize: uint32(len(data)), Flags: loader.SegmentReadable, Data: data},
		},
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "encode init image failed")
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[1], image, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func debugWrite(code []byte, address uint64, size uint32) []byte {
	code = append(code, 0xb8, 2, 0, 0, 0, 0x48, 0xbf)
	code = binary.LittleEndian.AppendUint64(code, address)
	code = append(code, 0xbe)
	code = binary.LittleEndian.AppendUint32(code, size)
	code = append(code, 0xcd, 0x80)
	return requireOK(code)
}

func kernelCall3(code []byte, operation uint32, arg0 uint64, arg1 uint64, arg2 uint32) []byte {
	code = append(code, 0xb8)
	code = binary.LittleEndian.AppendUint32(code, operation)
	code = append(code, 0x48, 0xbf)
	code = binary.LittleEndian.AppendUint64(code, arg0)
	code = append(code, 0x48, 0xbe)
	code = binary.LittleEndian.AppendUint64(code, arg1)
	code = append(code, 0xba)
	code = binary.LittleEndian.AppendUint32(code, arg2)
	return append(code, 0xcd, 0x80)
}

func requireOK(code []byte) []byte {
	return append(code, 0x85, 0xc0, 0x74, 0x02, 0x0f, 0x0b)
}
