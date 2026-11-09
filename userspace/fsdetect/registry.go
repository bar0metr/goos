package fsdetect

import "goos/kernel/ramfs"

// Driver is one filesystem driver the system can start for a volume.
type Driver struct {
	Type ramfs.FilesystemType
	// Name is what mount commands and configuration call the type.
	Name string
	// Executable is the driver program. Drivers come from the boot archive:
	// the VFS waits on a driver, so a driver cannot page its code in through
	// the VFS as a program on the volume does (ADR 0121, ADR 0222).
	Executable string
	// Writable says whether the driver may be given a writable slice for an
	// additional volume. The persistent volume follows its own policy.
	Writable bool
	// Additional says whether the driver serves additional volumes. The
	// FAT32 service serves the persistent volume only: it maps its cache at
	// a fixed address and expects a writable slice.
	Additional bool
}

// Drivers is the static registry. A type with no entry here is never
// started, whatever a probe or a mount command says.
var Drivers = [...]Driver{
	{Type: ramfs.FilesystemFAT32, Name: "fat32", Executable: "/bootstrap/fat32", Writable: true},
	{Type: ramfs.FilesystemExt4, Name: "ext4", Executable: "/bootstrap/ext4", Additional: true},
}

// DriverFor returns the registry entry for a type.
func DriverFor(kind ramfs.FilesystemType) (Driver, bool) {
	for _, driver := range Drivers {
		if driver.Type == kind {
			return driver, true
		}
	}
	return Driver{}, false
}

// TypeNamed resolves a type name from a mount command or configuration.
// "auto" returns zero and true: the probe decides.
func TypeNamed(name []byte) (ramfs.FilesystemType, bool) {
	if string(name) == "auto" {
		return 0, true
	}
	for _, driver := range Drivers {
		if driver.Name == string(name) {
			return driver.Type, true
		}
	}
	return 0, false
}

// Choose combines a probe with the type an administrator asked for, for an
// additional volume. A
// requested type that differs from what the probe found is refused rather
// than forced: the driver would only refuse the volume later, or worse,
// misread it.
func Choose(result *Result, requested ramfs.FilesystemType) (Driver, Status, bool) {
	if result.Status != Recognized {
		return Driver{}, result.Status, false
	}
	if requested != 0 && requested != result.Type {
		return Driver{}, Unknown, false
	}
	driver, ok := DriverFor(result.Type)
	if !ok || !driver.Additional {
		return Driver{}, Unknown, false
	}
	return driver, Recognized, true
}
