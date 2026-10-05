package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/vfs"
)

// Device is one of the character devices the compatibility layer serves
// itself. No filesystem holds them: the kernel recognizes three canonical
// paths before it asks the VFS, so a Linux program can discard output, read
// zeros, and find out that it has no terminal. Native programs do not see
// these names, and no directory lists them.
type Device uint8

const (
	DeviceNone Device = iota
	// DeviceNull is /dev/null: reads end at once, writes vanish.
	DeviceNull
	// DeviceZero is /dev/zero: reads return zero bytes, writes vanish.
	DeviceZero
	// DeviceTTY is /dev/tty, the controlling terminal. A GoOS Linux process
	// has none, so the name has a status and no open.
	DeviceTTY
	// DeviceCPUOnline and DeviceCPUPossible are the sysfs lists of online
	// and present processors, read-only text such as "0-3". The C library
	// counts processors from them.
	DeviceCPUOnline
	DeviceCPUPossible
	lastDevice = DeviceCPUPossible
)

// CPUListSize bounds the text of a processor list: 64 processors written
// one by one, with commas, and a newline.
const CPUListSize = 192

// CPUListText writes mask as Linux writes a processor list, ranges joined
// by commas and ended by a newline: "0-3", "0,2-3".
func CPUListText(target *[CPUListSize]byte, mask uint64) []byte {
	size := 0
	for cpu := 0; cpu < 64; {
		if mask&(1<<uint(cpu)) == 0 {
			cpu++
			continue
		}
		last := cpu
		for last+1 < 64 && mask&(1<<uint(last+1)) != 0 {
			last++
		}
		if size != 0 {
			target[size] = ','
			size++
		}
		size += putDecimal(target[size:], uint64(cpu))
		if last != cpu {
			target[size] = '-'
			size++
			size += putDecimal(target[size:], uint64(last))
		}
		cpu = last + 1
	}
	target[size] = '\n'
	return target[:size+1]
}

func putDecimal(target []byte, value uint64) int {
	if value >= 10 {
		target[0], target[1] = byte('0'+value/10), byte('0'+value%10)
		return 2
	}
	target[0] = byte('0' + value)
	return 1
}

// Text reports whether a device is a read-only text file rather than a
// character device.
func (device Device) Text() bool {
	return device == DeviceCPUOnline || device == DeviceCPUPossible
}

const (
	// DeviceFilesystem is st_dev of the three devices. No mount uses it:
	// mount identities have a generation in their upper half.
	DeviceFilesystem = uint64(2)
	// MaxDeviceTransfer bounds the zero bytes one read returns. A longer
	// request is a short read, which the program continues.
	MaxDeviceTransfer = 64 << 10
	// MaxTransfer is Linux MAX_RW_COUNT, the most one read or write reports.
	MaxTransfer = uint64(0x7ffff000)
)

// LookupDevice names the device a canonical absolute path refers to. A path
// beneath a device name is ENOTDIR, as beneath any file.
func LookupDevice(path []byte) (Device, abi.Errno) {
	for device := DeviceNull; device <= lastDevice; device++ {
		exact, beneath := matches(path, device.path())
		if exact {
			return device, 0
		}
		if beneath {
			return DeviceNone, abi.ENOTDIR
		}
	}
	return DeviceNone, 0
}

func (device Device) path() string {
	switch device {
	case DeviceNull:
		return "/dev/null"
	case DeviceZero:
		return "/dev/zero"
	case DeviceTTY:
		return "/dev/tty"
	case DeviceCPUOnline:
		return "/sys/devices/system/cpu/online"
	case DeviceCPUPossible:
		return "/sys/devices/system/cpu/possible"
	}
	return ""
}

// matches compares byte by byte, so the kernel converts no path to a string.
func matches(path []byte, name string) (exact bool, beneath bool) {
	if len(name) == 0 || len(path) < len(name) {
		return false, false
	}
	for index := 0; index < len(name); index++ {
		if path[index] != name[index] {
			return false, false
		}
	}
	if len(path) == len(name) {
		return true, false
	}
	return false, path[len(name)] == '/'
}

// Number is the Linux device number: major 1 minor 3 for null, minor 5 for
// zero, and major 5 minor 0 for tty, in the encoding st_rdev uses.
func (device Device) Number() uint64 {
	switch device {
	case DeviceNull:
		return 1<<8 | 3
	case DeviceZero:
		return 1<<8 | 5
	case DeviceTTY:
		return 5 << 8
	}
	return 0
}

// DeviceStatus describes a device as stat reports it: a character device
// anyone may read and write, or a read-only text file of the 4096 bytes
// sysfs reports for every attribute.
func DeviceStatus(device Device) FileStatus {
	if device.Text() {
		return FileStatus{Device: DeviceFilesystem, Inode: 0x100 + uint64(device), Mode: abi.ModeRegular | 0o444, Size: 4096}
	}
	return FileStatus{Device: DeviceFilesystem, Inode: device.Number(), Mode: abi.ModeCharacter | 0o666, RawDevice: device.Number()}
}

// DeviceOpenErrno applies the open flags to a device. trailing reports that
// the path demanded a directory.
func DeviceOpenErrno(device Device, intent OpenIntent, trailing bool) abi.Errno {
	switch {
	case trailing || intent.Directory:
		return abi.ENOTDIR
	case intent.Create && intent.Exclusive:
		return abi.EEXIST
	case device == DeviceTTY:
		// Linux answers ENXIO to a process with no controlling terminal.
		return abi.ENXIO
	case device.Text() && (intent.Rights&vfs.Write != 0 || intent.Create || intent.Truncate):
		return abi.EACCES
	}
	return 0
}

// DevicePathErrno answers a path call that has no business with a device:
// exact when the path names one, ENOTDIR when it lies beneath one, and zero
// when the path is an ordinary one for the VFS.
func DevicePathErrno(path []byte, exact abi.Errno) abi.Errno {
	device, errno := LookupDevice(path)
	if errno == 0 && device != DeviceNone {
		errno = exact
	}
	return errno
}

// DeviceOf returns the device a synthetic description holds.
func DeviceOf(description FileDescription) Device {
	if description.Kind != FDSynthetic || description.Object > uint64(lastDevice) {
		return DeviceNone
	}
	return Device(description.Object)
}
