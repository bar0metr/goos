package shell

import "goos/kernel/vfs"

// Unbounded is the MaxArgs of a command that takes any number of arguments.
const Unbounded = -1

// CommandInfo documents one shell builtin. The executables in /system/bin are
// described by their own manifests and the command index built from them
// (ADR 0080). MinArgs and MaxArgs count the arguments after the name.
type CommandInfo struct {
	Name, Usage, Summary string
	MinArgs, MaxArgs     int
}

// Commands lists the builtins in the order `help` prints them.
var Commands = [...]CommandInfo{
	{Name: "help", Usage: "help [COMMAND]", Summary: "list commands, or describe one", MaxArgs: 1},
	{Name: "cd", Usage: "cd [DIRECTORY]", Summary: "change the working directory (default /)", MaxArgs: 1},
	{Name: "clear", Usage: "clear", Summary: "clear the screen"},
	{Name: "time", Usage: "time COMMAND [ARGUMENT...]", Summary: "run a command line and report how long it took", MinArgs: 1, MaxArgs: Unbounded},
	{Name: "jobs", Usage: "jobs", Summary: "list background jobs started with &"},
	{Name: "kill", Usage: "kill JOB", Summary: "stop a background job, named as N or %N", MinArgs: 1, MaxArgs: 1},
	{Name: "services", Usage: "services", Summary: "show supervised services and their state"},
	{Name: "handles", Usage: "handles", Summary: "list the shell's handles"},
	{Name: "caps", Usage: "caps", Summary: "list the shell's capabilities and rights"},
	{Name: "config", Usage: "config [list] | config get KEY | config set KEY VALUE", Summary: "read and change typed settings", MaxArgs: 3},
	{Name: "log", Usage: "log", Summary: "print the system log"},
	{Name: "diag", Usage: "diag", Summary: "dump kernel diagnostics (diagnostic builds only)"},
	{Name: "reboot", Usage: "reboot", Summary: "flush storage and restart the machine"},
}

// LookupCommand finds a builtin by name.
func LookupCommand(name []byte) (CommandInfo, bool) {
	for index := range Commands {
		if equal(name, Commands[index].Name) {
			return Commands[index], true
		}
	}
	return CommandInfo{}, false
}

// Accepts reports whether count arguments after the name are allowed.
func (command CommandInfo) Accepts(count int) bool {
	return count >= command.MinArgs && (command.MaxArgs == Unbounded || count <= command.MaxArgs)
}

// IsHelpFlag reports whether argument asks for a command's usage.
func IsHelpFlag(argument []byte) bool {
	return equal(argument, "-h") || equal(argument, "--help")
}

// VFSReason describes a refused filesystem request. PermissionDenied is what a
// read-only mount answers to a mutation, which is the only way a user meets it
// through a utility, so a mutating caller passes mutation to say so.
func VFSReason(status vfs.Status, mutation bool) string {
	switch status {
	case vfs.StatusOK:
		return "ok"
	case vfs.StatusInvalidArgument:
		return "invalid path or argument"
	case vfs.StatusNotFound:
		return "no such file or directory"
	case vfs.StatusAlreadyExists:
		return "already exists"
	case vfs.StatusPermissionDenied:
		if mutation {
			return "read-only filesystem"
		}
		return "permission denied"
	case vfs.StatusResourceExhausted:
		return "no space or too many open files"
	case vfs.StatusBackendError:
		return "storage error"
	case vfs.StatusNotDirectory:
		return "not a directory"
	case vfs.StatusIsDirectory:
		return "is a directory"
	case vfs.StatusNotEmpty:
		return "directory not empty"
	case vfs.StatusBusy:
		return "a mount depends on it"
	default:
		return "filesystem error"
	}
}

// FormatUptime renders seconds as "up [D day(s), ]H:MM:SS" into target and
// returns the used prefix. It does not allocate.
func FormatUptime(seconds uint64, target *[32]byte) []byte {
	out := target[:0]
	out = append(out, "up "...)
	days := seconds / 86400
	if days != 0 {
		out = appendUint(out, days)
		if days == 1 {
			out = append(out, " day, "...)
		} else {
			out = append(out, " days, "...)
		}
	}
	out = appendUint(out, seconds/3600%24)
	out = append(out, ':')
	out = appendTwoDigits(out, seconds/60%60)
	out = append(out, ':')
	return appendTwoDigits(out, seconds%60)
}

// FormatSize renders a byte count in the largest of KiB, MiB and GiB that keeps
// it at least one, with one decimal, into target. It does not allocate.
func FormatSize(bytes uint64, target *[24]byte) []byte {
	unit, name := uint64(1024), " KiB"
	if bytes >= 1<<30 {
		unit, name = 1<<30, " GiB"
	} else if bytes >= 1<<20 {
		unit, name = 1<<20, " MiB"
	}
	tenths := (bytes%unit*10 + unit/2) / unit
	whole := bytes / unit
	if tenths == 10 {
		whole, tenths = whole+1, 0
	}
	out := appendUint(target[:0], whole)
	out = append(out, '.', byte('0'+tenths))
	return append(out, name...)
}

func appendUint(out []byte, value uint64) []byte {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(out, digits[position:]...)
}

func appendTwoDigits(out []byte, value uint64) []byte {
	return append(out, byte('0'+value/10), byte('0'+value%10))
}
