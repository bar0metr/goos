package main

import (
	"goos/internal/version"
	"goos/userspace/config"
	"goos/userspace/remotepolicy"
)

// declareSchema is the set of settings this image publishes. A key must be
// declared before it can be set, so the schema is what makes a write checkable
// and a typo a failure rather than a new setting nothing reads.
func declareSchema(target *config.Store) config.Status {
	name, _ := config.Text([]byte(version.Name))
	release, _ := config.Text([]byte(version.Release))
	machine, _ := config.Text([]byte(version.Machine))
	declarations := [...]struct {
		key      string
		value    config.Value
		readOnly bool
	}{
		// Published by the running image and writable by nobody: a stored copy
		// would be a second source of truth about what is executing.
		{"system.build.name", name, true},
		{"system.build.release", release, true},
		{"system.build.machine", machine, true},
		{"system.hostname", mustText("goos"), false},
		{"system.log.persist", config.Bool(true), false},
		// The network service's settings (ADR 0191) and timer policy (ADR 0192).
		// The service validates them as a whole when it loads them; the store
		// only checks their types. Times are in milliseconds.
		{"system.net.mode", mustText("dhcp"), false},
		{"system.net.ipv4.address", mustText("0.0.0.0"), false},
		{"system.net.ipv4.prefix", config.Uint(24), false},
		{"system.net.ipv4.gateway", mustText("0.0.0.0"), false},
		{"system.net.ipv4.default-ttl", config.Uint(64), false},
		{"system.net.ipv4.probe-count", config.Uint(3), false},
		{"system.net.ipv4.probe-wait-ms", config.Uint(1000), false},
		{"system.net.ipv4.probe-min-ms", config.Uint(1000), false},
		{"system.net.ipv4.probe-max-ms", config.Uint(2000), false},
		{"system.net.ipv4.announce-count", config.Uint(2), false},
		{"system.net.ipv4.announce-interval-ms", config.Uint(2000), false},
		{"system.net.ipv4.defend-interval-ms", config.Uint(10000), false},
		{"system.net.ipv4.max-conflicts", config.Uint(10), false},
		{"system.net.ipv4.conflict-rate-limit-ms", config.Uint(60000), false},
		{"system.net.arp.cache-timeout-ms", config.Uint(60000), false},
		{"system.net.arp.resolve-interval-ms", config.Uint(1000), false},
		{"system.net.arp.resolve-attempts", config.Uint(3), false},
		{"system.net.icmp.echo-reply-rate", config.Uint(20), false},
		{"system.net.dhcp.initial-retry-ms", config.Uint(4000), false},
		{"system.net.dhcp.max-retry-ms", config.Uint(64000), false},
		{"system.net.dhcp.conflict-retry-ms", config.Uint(10000), false},
		{"system.net.udp.ephemeral-port-min", config.Uint(49152), false},
		{"system.net.udp.ephemeral-port-max", config.Uint(65535), false},
		{"system.net.udp.recv-queue-depth", config.Uint(8), false},
		{"system.net.udp.max-endpoints", config.Uint(32), false},
		{"system.net.udp.max-endpoints-per-session", config.Uint(8), false},
		{"system.net.udp.max-payload", config.Uint(4096), false},
		{"system.net.tcp.ephemeral-port-min", config.Uint(49152), false},
		{"system.net.tcp.ephemeral-port-max", config.Uint(65535), false},
		{"system.net.tcp.max-endpoints", config.Uint(32), false},
		{"system.net.tcp.max-endpoints-per-session", config.Uint(8), false},
		{"system.net.tcp.listen-backlog", config.Uint(8), false},
		{"system.net.tcp.send-buffer-bytes", config.Uint(16384), false},
		{"system.net.tcp.recv-buffer-bytes", config.Uint(16384), false},
		{"system.net.tcp.connect-timeout-ms", config.Uint(30000), false},
		{"system.net.tcp.rto-min-ms", config.Uint(1000), false},
		{"system.net.tcp.rto-max-ms", config.Uint(60000), false},
		{"system.net.tcp.time-wait-ms", config.Uint(60000), false},
		// The resolver service's settings (ADR 0194). 0.0.0.0 marks an unused
		// server slot.
		{"system.dns.server1", mustText("0.0.0.0"), false},
		{"system.dns.server2", mustText("0.0.0.0"), false},
		{"system.dns.server3", mustText("0.0.0.0"), false},
		{"system.dns.server4", mustText("0.0.0.0"), false},
		{"system.dns.cache.entries", config.Uint(64), false},
		{"system.dns.cache.max-ttl-sec", config.Uint(86400), false},
		{"system.dns.cache.max-negative-sec", config.Uint(3600), false},
		{"system.dns.failure-cache-sec", config.Uint(5), false},
		{"system.dns.query-timeout-ms", config.Uint(3000), false},
		{"system.dns.attempts", config.Uint(2), false},
		{"system.shell.prompt", mustText("goos> "), false},
		{"user.name", mustText("goos"), false},
	}
	for _, declaration := range declarations {
		if status := target.Declare(declaration.key, declaration.value, declaration.readOnly); status != config.StatusOK {
			return status
		}
	}
	return remotepolicy.DeclareDefaults(target)
}

func mustText(value string) config.Value {
	result, _ := config.Text([]byte(value))
	return result
}
