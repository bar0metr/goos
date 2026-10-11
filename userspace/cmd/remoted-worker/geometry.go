package main

import (
	"goos/userspace/remoteapi"
	"goos/userspace/sshserver"
	"goos/userspace/tty"
)

func geometryForEvent(current remoteapi.Geometry, event sshserver.Event) (remoteapi.Geometry, bool) {
	var term []byte
	switch event.Kind {
	case sshserver.PTY:
		if int(event.TermLen) > len(event.Term) {
			return remoteapi.Geometry{}, false
		}
		term = event.Term[:event.TermLen]
	case sshserver.Resize:
		if !current.Valid() {
			return remoteapi.Geometry{}, false
		}
		term = current.Term[:current.TermSize]
	default:
		return remoteapi.Geometry{}, false
	}
	return remoteapi.MakeGeometry(event.Columns, event.Rows, term)
}

// installGeometry assigns the engine sequence before PID 1 receives a resize.
func installGeometry(engine *tty.Engine, geometry *remoteapi.Geometry) bool {
	if !engine.SetRemoteGeometry(int(geometry.Columns), int(geometry.Rows), geometry.Term[:geometry.TermSize]) {
		return false
	}
	geometry.Sequence = engine.CurrentGeometry().Sequence
	return true
}
