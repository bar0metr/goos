package amd64

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"goos/kernel/sched"
)

// TestAssemblySlotBoundsMatchTheScheduler keeps io.S's per-slot tables sized
// by the scheduler's bounds: a table shorter than sched.MaxTasks would let a
// high slot write past it.
func TestAssemblySlotBoundsMatchTheScheduler(t *testing.T) {
	wire, err := os.ReadFile("io.S")
	if err != nil {
		t.Fatal(err)
	}
	source := string(wire)
	for name, want := range map[string]int{"GOOS_MAX_TASKS": sched.MaxTasks, "GOOS_MAX_CPUS": sched.MaxCPUs} {
		match := regexp.MustCompile(`(?m)^\.set ` + name + `, (\d+)$`).FindStringSubmatch(source)
		if len(match) != 2 {
			t.Fatalf("io.S does not set %s", name)
		}
		if got, _ := strconv.Atoi(match[1]); got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}
	// Every per-slot table takes its size from the constants.
	for label, size := range map[string]string{
		"goos_task_contexts:":           "8 * GOOS_SLOT_COUNT",
		"goos_task_roots:":              "8 * GOOS_SLOT_COUNT",
		"goos_task_fs_bases:":           "8 * GOOS_SLOT_COUNT",
		"goos_task_rsp0:":               "8 * GOOS_SLOT_COUNT",
		"goos_task_ticks:":              "8 * GOOS_SLOT_COUNT",
		"goos_task_exit_codes:":         "8 * GOOS_MAX_TASKS",
		"goos_task_fault_addresses:":    "8 * GOOS_MAX_TASKS",
		"goos_user_call_rsp_baselines:": "8 * GOOS_MAX_TASKS",
		"goos_task_fpu:":                "GOOS_SLOT_COUNT << GOOS_FPU_AREA_SHIFT",
		"goos_linux_user_fpu:":          "GOOS_MAX_TASKS << GOOS_FPU_AREA_SHIFT",
	} {
		body := assemblyBody(t, source, "\n"+label+"\n", "\n")
		if strings.TrimSpace(body) != ".zero "+size {
			t.Fatalf("%s reserves %q, want .zero %s", label, strings.TrimSpace(body), size)
		}
	}
	if !strings.Contains(source, "\n.set GOOS_FPU_AREA_SHIFT, 9\n") || FPUAreaSize != 1<<9 {
		t.Fatalf("FPU area size %d does not match GOOS_FPU_AREA_SHIFT", FPUAreaSize)
	}
	// The bounds checks in the assembly accessors use the same names.
	for _, accessor := range []string{"goos_task_ticks_read:", "goos_task_fault_address_take:", "goos_task_ticks_clear:"} {
		body := assemblyBody(t, source, "\n"+accessor+"\n", "retq")
		if !strings.Contains(body, "cmpq $GOOS_SLOT_COUNT, %rcx") && !strings.Contains(body, "cmpq $GOOS_MAX_TASKS, %rcx") {
			t.Fatalf("%s checks a literal bound:\n%s", accessor, body)
		}
	}
}
