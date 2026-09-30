package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestLookupDevice(t *testing.T) {
	for _, test := range []struct {
		path   string
		device Device
		errno  abi.Errno
	}{
		{"/dev/null", DeviceNull, 0},
		{"/dev/zero", DeviceZero, 0},
		{"/dev/tty", DeviceTTY, 0},
		{"/dev/null/x", DeviceNone, abi.ENOTDIR},
		{"/dev/tty/", DeviceNone, abi.ENOTDIR},
		{"/dev/nullx", DeviceNone, 0},
		{"/dev/tty0", DeviceNone, 0},
		{"/dev", DeviceNone, 0},
		{"/dev/nul", DeviceNone, 0},
		{"/tmp/dev/null", DeviceNone, 0},
		{"/", DeviceNone, 0},
		{"", DeviceNone, 0},
	} {
		device, errno := LookupDevice([]byte(test.path))
		if device != test.device || errno != test.errno {
			t.Errorf("%q: device=%d errno=%d, want %d and %d", test.path, device, errno, test.device, test.errno)
		}
	}
}

func TestDeviceStatusIsACharacterDevice(t *testing.T) {
	for device, number := range map[Device]uint64{DeviceNull: 0x103, DeviceZero: 0x105, DeviceTTY: 0x500} {
		status := DeviceStatus(device)
		if status.Mode != abi.ModeCharacter|0o666 || status.RawDevice != number || status.Inode == 0 || status.Device != DeviceFilesystem || status.Size != 0 {
			t.Errorf("device %d: %+v", device, status)
		}
	}
}

func TestDeviceOpenRules(t *testing.T) {
	plain, _ := DecodeOpen(uint64(abi.OpenReadWrite))
	directory, _ := DecodeOpen(uint64(abi.OpenReadOnly | abi.OpenDirectory))
	exclusive, _ := DecodeOpen(uint64(abi.OpenWriteOnly | abi.OpenCreate | abi.OpenExclusive))
	created, _ := DecodeOpen(uint64(abi.OpenWriteOnly | abi.OpenCreate | abi.OpenTruncate))
	for _, test := range []struct {
		device   Device
		intent   OpenIntent
		trailing bool
		want     abi.Errno
	}{
		{DeviceNull, plain, false, 0},
		{DeviceZero, plain, false, 0},
		{DeviceNull, created, false, 0},
		{DeviceNull, plain, true, abi.ENOTDIR},
		{DeviceNull, directory, false, abi.ENOTDIR},
		{DeviceZero, exclusive, false, abi.EEXIST},
		{DeviceTTY, plain, false, abi.ENXIO},
		{DeviceTTY, directory, false, abi.ENOTDIR},
	} {
		if errno := DeviceOpenErrno(test.device, test.intent, test.trailing); errno != test.want {
			t.Errorf("device %d %+v: errno=%d, want %d", test.device, test.intent, errno, test.want)
		}
	}
}

func TestDevicePathErrno(t *testing.T) {
	for path, want := range map[string]abi.Errno{"/dev/null": abi.EACCES, "/dev/tty": abi.EACCES, "/dev/zero/x": abi.ENOTDIR, "/tmp/null": 0} {
		if errno := DevicePathErrno([]byte(path), abi.EACCES); errno != want {
			t.Errorf("%q: errno=%d, want %d", path, errno, want)
		}
	}
}

func TestDeviceOfReadsOnlySyntheticDescriptions(t *testing.T) {
	if DeviceOf(FileDescription{Kind: FDSynthetic, Object: uint64(DeviceZero)}) != DeviceZero {
		t.Fatal("a synthetic description lost its device")
	}
	if DeviceOf(FileDescription{Kind: FDVFSFile, Object: uint64(DeviceZero)}) != DeviceNone ||
		DeviceOf(FileDescription{Kind: FDSynthetic, Object: 99}) != DeviceNone {
		t.Fatal("a foreign description names a device")
	}
}
