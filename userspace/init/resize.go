package supervisor

import (
	"goos/userspace/commands"
	execproto "goos/userspace/exec"
)

// ResizeOwner selects one raw foreground receiver in a terminal session.
// A nested child suspends its parent's receiver until the child is reaped.
type ResizeOwner struct {
	parent uint64
	child  bool
}

func (owner *ResizeOwner) Start(token uint64, request execproto.Request, granted commands.Grant) bool {
	if token == 0 || !ResizesTerminal(request, granted) {
		return false
	}
	owner.parent, owner.child = token, false
	return true
}

func (owner *ResizeOwner) Handoff(token uint64) bool {
	if !owner.Allows(token, false) {
		return false
	}
	owner.child = true
	return true
}

func (owner *ResizeOwner) Restore(token uint64) bool {
	if !owner.Allows(token, true) {
		return false
	}
	owner.child = false
	return true
}

func (owner *ResizeOwner) Allows(token uint64, child bool) bool {
	return token != 0 && owner.parent == token && owner.child == child
}

func (owner *ResizeOwner) Release(token uint64) {
	if owner.parent == token {
		*owner = ResizeOwner{}
	}
}
