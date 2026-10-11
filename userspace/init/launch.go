package supervisor

import (
	"goos/userspace/commands"
	execproto "goos/userspace/exec"
)

// ChildLaunch decides what a command an application asked PID 1 to run is
// granted (ADR 0130). It inherits a terminal the shell has put in raw mode,
// so only a command that declares GrantTerminal may run. It gets the rest of
// its manifest's grants except GrantLaunch, so nesting stops at one level and
// an application cannot reach PID 1 through a chain of children.
//
// lookup returns the grants of the command at a path in the index that
// describes that path, and false for one no index describes.
func ChildLaunch(request execproto.Request, lookup func(path []byte) (commands.Grant, bool)) ([]byte, commands.Grant, LaunchStatus) {
	command, ok := request.LaunchCommand()
	if !ok {
		return nil, 0, LaunchCapabilityDenied
	}
	granted, found := lookup(request.Stages[0].Path)
	if !found {
		return nil, 0, LaunchNotFound
	}
	if granted&commands.GrantTerminal == 0 {
		return nil, 0, LaunchCapabilityDenied
	}
	return command, granted &^ commands.GrantLaunch, LaunchOK
}

// LaunchesChildren reports whether a stage of request is given a launch
// channel: the only stage of a foreground job, whose manifest asks for one.
func LaunchesChildren(request execproto.Request, granted commands.Grant) bool {
	return request.Count == 1 && request.ErrorSource == 0 && !request.Background && granted&commands.GrantLaunch != 0
}

// ResizesTerminal grants resize only to one trusted raw foreground stage.
func ResizesTerminal(request execproto.Request, granted commands.Grant) bool {
	return request.Count == 1 && request.ErrorSource == 0 && !request.Background && granted&commands.GrantTerminal != 0
}
