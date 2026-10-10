package arena

// Region bookkeeping is independent of native allocation and can be host tested.
type allocatorPosition struct{ start, next, end, high uintptr }
type regionPosition struct {
	position allocatorPosition
	active   bool
}
type regionStack struct {
	serial, top uint64
	depth       uint32
}
type regionScope struct {
	saved            allocatorPosition
	previous, cookie uint64
	region           *regionPosition
}

func enterRegion(current allocatorPosition, region *regionPosition, stack *regionStack) (allocatorPosition, regionScope) {
	if region.active || region.position.start == 0 || region.position.end <= region.position.start || region.position.next < region.position.start || region.position.next > region.position.end {
		panic("arena: invalid or active region")
	}
	stack.serial++
	if stack.serial == 0 {
		panic("arena: scope cookie overflow")
	}
	scope := regionScope{saved: current, previous: stack.top, cookie: stack.serial, region: region}
	stack.top = scope.cookie
	stack.depth++
	region.active = true
	return region.position, scope
}
func leaveRegion(current allocatorPosition, scope regionScope, stack *regionStack) allocatorPosition {
	if scope.region == nil || !scope.region.active || stack.depth == 0 || stack.top != scope.cookie || current.start != scope.region.position.start || current.end != scope.region.position.end || current.next < current.start || current.next > current.end {
		panic("arena: region scopes must leave in LIFO order")
	}
	scope.region.position = current
	scope.region.active = false
	stack.top = scope.previous
	stack.depth--
	return scope.saved
}
func validReset(current allocatorPosition, mark uintptr) bool {
	return mark >= current.start && mark <= current.next
}
