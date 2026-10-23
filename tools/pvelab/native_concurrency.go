package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// nativeConcurrencyScenario checks the runtime, the storage driver's
// goroutine interrupt waits, region rendezvous and fenced filesystem recovery
// in one validation image with VFS_PROBE=1.
type nativeConcurrencyScenario struct{}

func (nativeConcurrencyScenario) Name() string { return "native-concurrency" }

func (nativeConcurrencyScenario) Run(ctx context.Context, s *Session) (Result, error) {
	g := s.Guest
	boot := strings.ReplaceAll(string(g.con.Tail(1<<20)), "\r", "")
	for _, marker := range []string{
		"VFS_READPOOL_LOCAL_OVERLAP_OK", "VFS_READPOOL_PERSISTENT_LOCAL_OK",
		"VFS_READPOOL_WRITE_BARRIER_OK", "VFS_READPOOL_CONTROL_BARRIER_OK",
		"VFS_READPOOL_PEER_CLEANUP_OK", "VFS_READPOOL_OK",
	} {
		if err := goroutineMarker(boot, marker); err != nil {
			return "", fail(ClassScenario, "vfs-readpool", err)
		}
	}
	g.pass("vfs-readpool")
	if err := driverIRQMarker(boot); err != nil {
		return "", fail(ClassScenario, "driver-irq-wait", err)
	}
	g.pass("driver-irq-wait")
	if result, err := (goroutinesScenario{}).Run(ctx, s); err != nil || result != ResultPass {
		return result, err
	}
	for _, check := range []struct {
		command string
		rounds  uint64
	}{
		{"threadprobe region-window 1 16", 16},
		{"threadprobe region-window 4 24", 24},
	} {
		out, err := g.Run(ctx, check.command, commandTimeout)
		if err != nil {
			return "", fail(ClassScenario, "region-window", err)
		}
		if err := g.Clean("region-window", out); err != nil {
			return "", err
		}
		if err := nativeRegionMarker(out, check.rounds); err != nil {
			return "", fail(ClassScenario, "region-window", err)
		}
		g.pass(check.command)
	}
	if err := nativeRecoveryCheck(ctx, g); err != nil {
		return "", err
	}
	return ResultPass, nil
}

func nativeRecoveryCheck(ctx context.Context, g *Guest) error {
	const path = "/home/.native-concurrency-check"
	const data = "native-concurrency-persistent-data"
	if out, err := g.Run(ctx, "echo "+data+" > "+path, commandTimeout); err != nil {
		return err
	} else if err := g.Clean("cache-recovery-create", out); err != nil {
		return err
	}
	before := strings.ReplaceAll(string(g.con.Tail(1<<20)), "\r", "")
	completed := strings.Count(before, "SYSTEM_REATTACHED_OK")
	if _, err := g.Run(ctx, "cd /persist/.validation-fat32-stall", commandTimeout); err != nil {
		return fail(ClassScenario, "cache-recovery-stall", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	for {
		after := strings.ReplaceAll(string(g.con.Tail(1<<20)), "\r", "")
		if strings.Count(after, "SYSTEM_REATTACHED_OK") > completed {
			if err := nativeRecoveryMarkers(before, after); err != nil {
				return fail(ClassScenario, "cache-recovery", err)
			}
			break
		}
		which, _, err := g.con.ReadUntilAny(waitCtx, []byte("SYSTEM_REATTACHED_OK"), []byte(kernelPanic))
		if err != nil {
			return g.readFailure(ClassScenario, "cache-recovery", "SYSTEM_REATTACHED_OK", err)
		}
		if which == 1 {
			return failf(ClassBoot, "cache-recovery", "kernel panic:\n%s", tailText(g.con))
		}
	}
	for _, command := range []string{"cat " + path, "echo " + data + " > " + path, "cat " + path} {
		out, err := g.Run(ctx, command, commandTimeout)
		if err != nil {
			return fail(ClassScenario, "cache-recovery-io", err)
		}
		if err := g.Clean("cache-recovery-io", out); err != nil {
			return err
		}
		if strings.HasPrefix(command, "cat ") {
			if err := g.Expect("cache-recovery-data", out, data); err != nil {
				return err
			}
		}
	}
	g.pass("cache-recovery")
	return nil
}

func nativeRecoveryMarkers(before, after string) error {
	for _, marker := range []string{"FAT32_STALLED", "VFS_CONTROL_REPLACED_OK", "FAT32_STALL_REPLACING", "FILE_CACHE_RECOVERED_OK", "FAT32_RESTARTED_OK"} {
		if strings.Count(after, marker) != strings.Count(before, marker)+1 {
			return fmt.Errorf("expected one new %s marker", marker)
		}
	}
	for _, marker := range []string{"FAT32_STALL_FAILED", "FILE_CACHE_RECOVERY_LIMIT", "VFS_SERVICE_OK"} {
		if strings.Count(after, marker) != strings.Count(before, marker) {
			return fmt.Errorf("unexpected new %s marker", marker)
		}
	}
	return nil
}

func nativeRegionMarker(out string, rounds uint64) error {
	matches := 0
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.HasPrefix(line, "THREADPROBE_FAIL") || strings.HasPrefix(line, "fatal error:") {
			return fmt.Errorf("fixture reported: %s", line)
		}
		const prefix = "THREADPROBE_REGION_WINDOW_OK "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		matches++
		values := map[string]uint64{}
		for _, field := range strings.Fields(strings.TrimPrefix(line, prefix)) {
			key, text, ok := strings.Cut(field, "=")
			value, err := strconv.ParseUint(text, 10, 64)
			if !ok || err != nil {
				return fmt.Errorf("invalid region-window field %q", field)
			}
			if _, duplicate := values[key]; duplicate {
				return fmt.Errorf("duplicate region-window field %q", key)
			}
			values[key] = value
		}
		if values["rounds"] != rounds || values["rejected"] != rounds*4 || values["region_retries"] < rounds*4 || values["collections"] != rounds*3 {
			return fmt.Errorf("incomplete region-window evidence: %s", line)
		}
	}
	if matches != 1 {
		return fmt.Errorf("expected one region-window marker, got %d", matches)
	}
	return nil
}

// driverIRQMarker checks the validation driver's proof that its serving
// goroutine parked on disk interrupts while a peer ran. A driver replaced
// after the injected fault may print it again; every copy must be whole.
func driverIRQMarker(boot string) error {
	matches := 0
	for _, line := range strings.Split(strings.ReplaceAll(boot, "\r", ""), "\n") {
		const prefix = "VIRTIO_BLK_G_IRQ_OK "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		matches++
		values := map[string]uint64{}
		for _, field := range strings.Fields(strings.TrimPrefix(line, prefix)) {
			key, text, ok := strings.Cut(field, "=")
			value, err := strconv.ParseUint(text, 10, 64)
			if !ok || err != nil {
				return fmt.Errorf("invalid driver interrupt field %q", field)
			}
			if _, duplicate := values[key]; duplicate {
				return fmt.Errorf("duplicate driver interrupt field %q", key)
			}
			values[key] = value
		}
		if len(values) != 3 || values["waits"] != 4 || values["tries"] < values["waits"] || values["peer"] == 0 {
			return fmt.Errorf("incomplete driver interrupt evidence: %s", line)
		}
	}
	if matches == 0 {
		return fmt.Errorf("no driver interrupt-wait marker")
	}
	return nil
}
