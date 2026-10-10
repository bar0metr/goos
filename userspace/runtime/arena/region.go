//go:build goos_userspace

package arena

import "unsafe"

// Region owns a fixed slab allocated before any scope enters it. Clear is valid
// only after the caller has dropped every pointer into the previous generation.
// Enter may nest other regions, but never the same region twice. Leave is LIFO.
type Region struct {
	backing []byte
	state   regionPosition
}
type Scope struct{ state regionScope }

var scopedRegions regionStack

func NewRegion(size uint32) *Region {
	if size < 4096 || size > 4<<20 {
		panic("arena: region size out of bounds")
	}
	backing := make([]byte, int(size)+15)
	first := (uintptr(unsafe.Pointer(&backing[0])) + 15) &^ uintptr(15)
	return &Region{backing: backing, state: regionPosition{position: allocatorPosition{start: first, next: first, end: first + uintptr(size), high: first}}}
}
func position() allocatorPosition {
	return allocatorPosition{start: start, next: next, end: end, high: high}
}
func setPosition(p allocatorPosition) { start, next, end, high = p.start, p.next, p.end, p.high }
func (r *Region) Enter() Scope {
	current, scope := enterRegion(position(), &r.state, &scopedRegions)
	setPosition(current)
	return Scope{state: scope}
}
func (s Scope) Leave() { setPosition(leaveRegion(position(), s.state, &scopedRegions)) }
func (r *Region) Clear() {
	if r.state.active {
		panic("arena: cannot clear active region")
	}
	r.state.position.next = r.state.position.start
	r.state.position.high = r.state.position.start
}
func (r *Region) Used() uint64 {
	if r.state.active {
		return uint64(next - start)
	}
	return uint64(r.state.position.next - r.state.position.start)
}
