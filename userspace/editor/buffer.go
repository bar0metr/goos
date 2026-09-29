package editor

// Buffer is a gap buffer over caller-owned storage. The text is
// storage[:gapStart] followed by storage[gapEnd:]; the bytes between are free.
// An insertion or deletion happens at the gap, so typing costs nothing but
// the new bytes. Moving the gap to another offset copies only the bytes
// between the old and the new position, never the whole text.
type Buffer struct {
	storage          []byte
	gapStart, gapEnd int
}

// Reset takes storage whose first length bytes are the text. The rest of
// storage is the gap.
func (buffer *Buffer) Reset(storage []byte, length int) bool {
	if length < 0 || length > len(storage) {
		return false
	}
	buffer.storage = storage
	buffer.gapStart, buffer.gapEnd = length, len(storage)
	return true
}

// Len is the length of the text.
func (buffer *Buffer) Len() int { return len(buffer.storage) - (buffer.gapEnd - buffer.gapStart) }

// Free is how many bytes can still be inserted.
func (buffer *Buffer) Free() int { return buffer.gapEnd - buffer.gapStart }

// At returns the text byte at index, which must be below Len.
func (buffer *Buffer) At(index int) byte {
	if index < buffer.gapStart {
		return buffer.storage[index]
	}
	return buffer.storage[index+buffer.gapEnd-buffer.gapStart]
}

// Insert puts value into the text at offset at. It refuses what does not fit
// and leaves the text unchanged.
func (buffer *Buffer) Insert(at int, value []byte) bool {
	if at < 0 || at > buffer.Len() || len(value) > buffer.Free() {
		return false
	}
	buffer.moveGap(at)
	buffer.gapStart += copy(buffer.storage[buffer.gapStart:buffer.gapEnd], value)
	return true
}

// Delete removes count bytes of text from offset at.
func (buffer *Buffer) Delete(at, count int) bool {
	if at < 0 || count < 0 || at > buffer.Len()-count {
		return false
	}
	buffer.moveGap(at)
	buffer.gapEnd += count
	return true
}

// Copy copies text from offset into target and returns how many bytes it
// copied: fewer than len(target) only at the end of the text.
func (buffer *Buffer) Copy(target []byte, offset int) int {
	if offset < 0 || offset >= buffer.Len() {
		return 0
	}
	copied := 0
	if offset < buffer.gapStart {
		copied = copy(target, buffer.storage[offset:buffer.gapStart])
	}
	if copied < len(target) {
		physical := offset + copied + buffer.gapEnd - buffer.gapStart
		copied += copy(target[copied:], buffer.storage[physical:])
	}
	return copied
}

// moveGap makes the gap start at text offset at, copying the bytes between
// its old and its new place across it.
func (buffer *Buffer) moveGap(at int) {
	switch {
	case at < buffer.gapStart:
		count := buffer.gapStart - at
		copy(buffer.storage[buffer.gapEnd-count:buffer.gapEnd], buffer.storage[at:buffer.gapStart])
		buffer.gapStart -= count
		buffer.gapEnd -= count
	case at > buffer.gapStart:
		count := at - buffer.gapStart
		copy(buffer.storage[buffer.gapStart:], buffer.storage[buffer.gapEnd:buffer.gapEnd+count])
		buffer.gapStart += count
		buffer.gapEnd += count
	}
}
