package supervisor

// ShutdownGrace bounds the time PID 1 gives one requesting SSH session to
// close before reboot or power-off.
type ShutdownGrace struct {
	Session  uint64
	Deadline uint64
}

func (grace *ShutdownGrace) Begin(session, now, timeout uint64) bool {
	if session == 0 || timeout == 0 || now > ^uint64(0)-timeout || grace.Session != 0 {
		return false
	}
	grace.Session, grace.Deadline = session, now+timeout
	return true
}

func (grace ShutdownGrace) Ready(now uint64, requesterPresent bool) bool {
	return grace.Session != 0 && (!requesterPresent || now >= grace.Deadline)
}

func (grace ShutdownGrace) Remaining(now uint64) (uint64, bool) {
	if grace.Session == 0 {
		return 0, false
	}
	if now >= grace.Deadline {
		return 1, true
	}
	return grace.Deadline - now, true
}
