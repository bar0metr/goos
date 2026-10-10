package main

import (
	"goos/userspace/remoteapi"
	"goos/userspace/sshserver"
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
