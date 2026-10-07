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
	case FieldTTL:
		return "system.net.ipv4.default-ttl"
	case FieldProbeCount:
		return "system.net.ipv4.probe-count"
	case FieldProbeWait:
		return "system.net.ipv4.probe-wait-ms"
	case FieldProbeMin:
		return "system.net.ipv4.probe-min-ms"
	case FieldProbeMax:
		return "system.net.ipv4.probe-max-ms"
	case FieldAnnounceCount:
		return "system.net.ipv4.announce-count"
	case FieldAnnounceInterval:
		return "system.net.ipv4.announce-interval-ms"
	case FieldDefendInterval:
		return "system.net.ipv4.defend-interval-ms"
	case FieldMaxConflicts:
		return "system.net.ipv4.max-conflicts"
	case FieldConflictRateLimit:
		return "system.net.ipv4.conflict-rate-limit-ms"
	case FieldARPCacheTimeout:
		return "system.net.arp.cache-timeout-ms"
	case FieldARPResolveInterval:
		return "system.net.arp.resolve-interval-ms"
	case FieldARPResolveAttempts:
		return "system.net.arp.resolve-attempts"
	case FieldEchoReplyRate:
		return "system.net.icmp.echo-reply-rate"
	case FieldDHCPInitialRetry:
		return "system.net.dhcp.initial-retry-ms"
	case FieldDHCPMaxRetry:
		return "system.net.dhcp.max-retry-ms"
	case FieldDHCPConflictRetry:
		return "system.net.dhcp.conflict-retry-ms"
	case FieldUDPEphemeralMin:
		return "system.net.udp.ephemeral-port-min"
	case FieldUDPEphemeralMax:
		return "system.net.udp.ephemeral-port-max"
	case FieldUDPReceiveQueue:
		return "system.net.udp.recv-queue-depth"
	case FieldUDPMaxEndpoints:
		return "system.net.udp.max-endpoints"
	case FieldUDPSessionEndpoints:
		return "system.net.udp.max-endpoints-per-session"
	case FieldUDPMaxPayload:
		return "system.net.udp.max-payload"
	}
	return ""
}
