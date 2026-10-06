package netapi

// Text returns short lowercase text for a result, or "" for an unknown one.
func (r Result) Text() string {
	switch r {
	case ResultOK:
		return "ok"
	case ResultDown:
		return "interface down"
	case ResultNoAddress:
		return "no address"
	case ResultNoRoute:
		return "no route"
	case ResultNeighborUnreachable:
		return "neighbor unreachable"
	case ResultTimeout:
		return "timeout"
	case ResultBusy:
		return "busy"
	case ResultReconfigured:
		return "reconfigured"
	case ResultPermissionDenied:
		return "permission denied"
	case ResultInvalidArgument:
		return "invalid argument"
	case ResultDriverFault:
		return "driver fault"
	case ResultConfigInvalid:
		return "invalid configuration"
	case ResultRemoteUnreachable:
		return "destination unreachable"
	case ResultTimeExceeded:
		return "time exceeded"
	}
	return ""
}

func (s State) Text() string {
	switch s {
	case StateDown:
		return "down"
	case StateConfiguring:
		return "configuring"
	case StateReady:
		return "ready"
	case StateFailed:
		return "failed"
	}
	return ""
}

func (m Mode) Text() string {
	switch m {
	case ModeDHCP:
		return "dhcp"
	case ModeStatic:
		return "static"
	}
	return ""
}

func (l Link) Text() string {
	switch l {
	case LinkDown:
		return "down"
	case LinkUp:
		return "up"
	}
	return ""
}

// Key names the configuration key a Reload validation error refers to, or
// "" for FieldNone and unknown values.
func (f Field) Key() string {
	switch f {
	case FieldMode:
		return "system.net.mode"
	case FieldAddress:
		return "system.net.ipv4.address"
	case FieldPrefix:
		return "system.net.ipv4.prefix"
	case FieldGateway:
		return "system.net.ipv4.gateway"
	}
	return ""
}
