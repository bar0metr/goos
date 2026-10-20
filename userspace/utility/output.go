package utility

import (
	"goos/userspace/runtime/lock"
	"goos/userspace/terminal"
)

// streamWriter moves a prefix of a record to a stream and returns how many
// bytes it took, at least one. The native one ends the process when the
// stream is gone; host tests record what they get.
type streamWriter interface {
	write(target uint64, value []byte) int
}

// output is the process's standard output and standard error.
//
// A record is one line or one diagnostic, assembled by its caller in one
// buffer. writeRecord sends it under the output lock, so a record that needs
// several stream writes is never interleaved with another record of the
// same process, while records from different goroutines may come out in any
// order. One lock covers both streams because they usually reach the same
// terminal. The lock is held across a wait for the stream to drain: the
// stream is the serialized resource, and a record that has started must
// finish before the next one starts.
//
// The handles are set once by Run, before the command's main function runs,
// and only read afterwards.
type output[W streamWriter] struct {
	mu     lock.Mutex
	stdout uint64
	stderr uint64
	io     W
}

// writeRecord sends record to target whole.
func (o *output[W]) writeRecord(target uint64, record []byte) {
	o.mu.Lock()
	for len(record) != 0 {
		record = record[o.io.write(target, record):]
	}
	o.mu.Unlock()
}

// tryRecord is the fatal/OOM path: never wait for a lock which this goroutine
// (or one waiting on it) may already hold. Losing a diagnostic is preferable to
// preventing process exit. The supplied writer must not wait and returns zero
// when it cannot make progress; a partial diagnostic is then discarded.
func (o *output[W]) tryRecord(target uint64, record []byte, write func(uint64, []byte) int) bool {
	if !o.mu.TryLock() {
		return false
	}
	defer o.mu.Unlock()
	for len(record) != 0 {
		size := write(target, record)
		if size <= 0 || size > len(record) {
			return false
		}
		record = record[size:]
	}
	return true
}

// appendLine is shared by Line.Print and host tests. Empty storage is an
// unbuffered line: every append is a record, rather than an infinite copy loop.
func appendLine(storage []byte, size int, value []byte, write func([]byte)) int {
	if len(storage) == 0 {
		if len(value) != 0 {
			write(value)
		}
		return 0
	}
	for len(value) != 0 {
		if size == len(storage) {
			write(storage[:size])
			size = 0
		}
		copied := copy(storage[size:], value)
		size += copied
		value = value[copied:]
	}
	return size
}

// errorTarget is standard error, or standard output when there is none.
func (o *output[W]) errorTarget() uint64 {
	if o.stderr == 0 {
		return o.stdout
	}
	return o.stderr
}

// appendFailure appends "command: path: reason\n", or "command: reason\n"
// when path is nil, to target and returns the record. Every copy stops one
// byte short of the end of target so the newline always fits; target must
// have room for at least that byte. Controls in path are shown as
// terminal.Printable shows them, not sent.
func appendFailure(target []byte, command, path []byte, reason string) []byte {
	limit := len(target) - 1
	size := copy(target[:limit], command)
	size += copy(target[size:limit], ": ")
	if path != nil {
		for index := 0; index < len(path) && size < limit; {
			var character []byte
			character, index = terminal.Printable(path, index)
			size += copy(target[size:limit], character)
		}
		size += copy(target[size:limit], ": ")
	}
	size += copy(target[size:limit], reason)
	target[size] = '\n'
	return target[:size+1]
}

// appendUsage appends "usage: synopsis\n" to target, as appendFailure does.
func appendUsage(target []byte, synopsis string) []byte {
	limit := len(target) - 1
	size := copy(target[:limit], "usage: ")
	size += copy(target[size:limit], synopsis)
	target[size] = '\n'
	return target[:size+1]
}
