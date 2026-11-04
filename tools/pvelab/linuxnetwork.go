package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// linuxNetworkScenario runs Linux IPv4 sockets under KVM (ADR 0210): the
// netprobe loopback groups, its loopback benchmarks, BusyBox httpd serving
// BusyBox wget and curl by name through /etc/hosts, and a linuxsockd fault
// that a new Linux launch must not notice. The guest takes a DHCP lease on
// the lab bridge first, so the socket path runs with net0 configured. The
// caller places busybox and curl in /system/linux and builds a validation
// image for the fault command.
type linuxNetworkScenario struct{}

// netprobeTimeout bounds a netprobe run: sixteen groups or the benchmarks.
const netprobeTimeout = 5 * time.Minute

// netprobeGroups are the lines a passing netprobe prints.
var netprobeGroups = []string{"create", "sockaddr", "bind", "listen", "connect", "stream", "shutdown", "sigpipe",
	"poll", "select", "udp", "dup", "exec", "eintr", "options", "file", "netprobe"}

func (linuxNetworkScenario) Name() string { return "linux-network" }

func (linuxNetworkScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	if s.Report.VM.NetBridge == "" {
		return "", failf(ClassInfra, "linux-network", "PVE_NET_BRIDGE is empty, so the VM has no network function")
	}
	listing, err := g.Run(ctx, "ls /system/linux", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "linux-network", err)
	}
	for _, program := range []string{"busybox", "curl", "netprobe"} {
		if !strings.Contains(listing, program) {
			return "", failf(ClassConfig, "linux-network", "/system/linux/%s is missing; build with make test-pve-linux-network", program)
		}
	}
	status, err := awaitLease(ctx, g)
	if err != nil {
		return "", err
	}
	s.logf("address %s", statusFields(status)["address"])

	if err := runNetprobe(ctx, g, "netprobe-groups"); err != nil {
		return "", err
	}
	bench, err := g.Run(ctx, "/system/linux/netprobe bench", netprobeTimeout)
	if err != nil {
		return "", fail(ClassScenario, "netprobe-bench", err)
	}
	if err := g.Expect("netprobe-bench", bench, "bench ok"); err != nil {
		return "", err
	}
	var measured []string
	for _, line := range strings.Split(bench, "\n") {
		if strings.HasPrefix(line, "bench ") && line != "bench ok" {
			measured = append(measured, line)
			s.logf("%s", line)
		}
	}
	if s.EvidenceDir != "" {
		if err := os.WriteFile(filepath.Join(s.EvidenceDir, "linux-network-bench.txt"), []byte(strings.Join(measured, "\n")+"\n"), 0o600); err != nil {
			return "", fail(ClassInfra, "netprobe-bench", err)
		}
	}

	want, err := fixtureDigest("userspace/linux/prebuilt/netprobe")
	if err != nil {
		return "", fail(ClassConfig, "linux-network", err)
	}
	if _, err := g.Run(ctx, "/system/linux/busybox httpd -f -p 8090 -h /system/linux &", commandTimeout); err != nil {
		return "", fail(ClassScenario, "httpd", err)
	}
	// httpd prints nothing once it listens; wget retries until it answers.
	var fetched string
	for attempt := 0; attempt < 10; attempt++ {
		fetched, err = g.Run(ctx, "/system/linux/busybox sh -c '/system/linux/busybox wget -q -O - http://localhost:8090/netprobe | /system/linux/busybox sha256sum'", commandTimeout)
		if err != nil {
			return "", fail(ClassScenario, "wget", err)
		}
		if strings.Contains(fetched, want) {
			break
		}
		time.Sleep(time.Second)
	}
	if err := g.Expect("busybox-wget", fetched, want); err != nil {
		return "", err
	}
	out, err := g.Run(ctx, "/system/linux/busybox sh -c '/system/linux/curl -sS -o /tmp/pvelab-curl.out http://localhost:8090/netprobe; echo curl-exit=$?; /system/linux/busybox sha256sum /tmp/pvelab-curl.out'", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "curl", err)
	}
	if err := g.Expect("curl-exit", out, "curl-exit=0"); err != nil {
		return "", err
	}
	if err := g.Expect("curl-sha256", out, want); err != nil {
		return "", err
	}

	services, err := g.Run(ctx, "services", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "services", err)
	}
	for _, name := range []string{"netd", "virtio-net", "resolved", "linuxsockd"} {
		state, restarts, ok := serviceState(services, name)
		if !ok || state != "running" || restarts != 0 {
			return "", failf(ClassScenario, "network stability", "%s state=%q restarts=%d; want running with no restarts:\n%s", name, state, restarts, services)
		}
	}
	g.pass("linux-network-services-stable")

	out, err = g.Run(ctx, "fault linuxsockd", commandTimeout)
	if err != nil {
		return "", fail(ClassScenario, "fault linuxsockd", err)
	}
	if err := g.Expect("fault-linuxsockd", out, "LINUXSOCKD_FAULT_INJECTED"); err != nil {
		return "", err
	}
	deadline := time.Now().Add(networkReadyTimeout)
	for {
		services, err = g.Run(ctx, "services", commandTimeout)
		if err != nil {
			return "", fail(ClassScenario, "services", err)
		}
		if state, restarts, ok := serviceState(services, "linuxsockd"); ok && state == "running" && restarts >= 1 {
			g.pass("linuxsockd-restarted")
			break
		}
		if time.Now().After(deadline) {
			return "", failf(ClassScenario, "linuxsockd-restarted", "linuxsockd did not come back:\n%s", services)
		}
		time.Sleep(2 * time.Second)
	}
	if err := runNetprobe(ctx, g, "netprobe-after-fault"); err != nil {
		return "", err
	}
	return ResultPass, nil
}

// runNetprobe runs the netprobe groups and requires every one.
func runNetprobe(ctx context.Context, g *Guest, check string) error {
	out, err := g.Run(ctx, "/system/linux/netprobe", netprobeTimeout)
	if err != nil {
		return fail(ClassScenario, check, err)
	}
	for _, group := range netprobeGroups {
		if err := g.Expect(check, out, group+" ok"); err != nil {
			return err
		}
	}
	if strings.Contains(out, "failed") {
		return failf(ClassScenario, check, "output reports a failure:\n%s", out)
	}
	return nil
}

// fixtureDigest is the SHA-256 of a file the image carries, read from the
// repository the image was built from.
func fixtureDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
