package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Class is the kind of failure; it decides the exit status.
type Class string

const (
	ClassConfig   Class = "config-error"
	ClassInfra    Class = "infra-error"
	ClassBoot     Class = "boot-failure"
	ClassScenario Class = "workload-failure"
	ClassCleanup  Class = "cleanup-failure"
)

// Result is the outcome a report records.
type Result string

const (
	ResultPass            Result = "pass"
	ResultExpectedFailure Result = "expected-failure"
)

// Exit statuses. An expected failure of a diagnostic scenario exits 0: the
// run did its job, which was to record the failure.
const (
	ExitPass     = 0
	ExitScenario = 1
	ExitConfig   = 2
	ExitInfra    = 3
	ExitCleanup  = 4
)

// stageError is an error with the stage it happened in and its class.
type stageError struct {
	class Class
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

func fail(class Class, stage string, err error) error {
	if err == nil {
		return nil
	}
	var se *stageError
	if errors.As(err, &se) {
		return err
	}
	return &stageError{class: class, stage: stage, err: err}
}

func failf(class Class, stage, format string, args ...any) error {
	return &stageError{class: class, stage: stage, err: fmt.Errorf(format, args...)}
}

func classOf(err error) Class {
	var se *stageError
	if errors.As(err, &se) {
		return se.class
	}
	return ClassInfra
}

func exitCode(class Class) int {
	switch class {
	case ClassConfig:
		return ExitConfig
	case ClassInfra:
		return ExitInfra
	case ClassCleanup:
		return ExitCleanup
	}
	return ExitScenario
}

type ProxmoxInfo struct {
	Node    string `json:"node"`
	Version string `json:"version"`
	VMID    int    `json:"vmid"`
	Pool    string `json:"pool"`
}

type VMInfo struct {
	Machine   string `json:"machine"`
	BIOS      string `json:"bios"`
	CPU       string `json:"cpu"`
	Sockets   int    `json:"sockets"`
	Cores     int    `json:"cores"`
	MemoryMiB int    `json:"memory_mib"`
}

// Workload is what a diagnostic workload did.
type Workload struct {
	Command   string   `json:"command"`
	Completed bool     `json:"completed"`
	Ending    string   `json:"ending"`
	Faults    []string `json:"faults,omitempty"`
}

type Report struct {
	Tool       string    `json:"tool"`
	RunID      string    `json:"run_id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMS int64     `json:"duration_ms"`

	Git       GitInfo     `json:"git"`
	Proxmox   ProxmoxInfo `json:"proxmox"`
	VM        VMInfo      `json:"vm"`
	Artifacts struct {
		ISOSHA256  string `json:"iso_sha256"`
		DiskSHA256 string `json:"disk_sha256"`
	} `json:"artifacts"`

	Scenario string   `json:"scenario"`
	Result   string   `json:"result"`
	Stage    string   `json:"stage,omitempty"`
	Error    string   `json:"error,omitempty"`
	Checks   []string `json:"checks,omitempty"`

	Workload *Workload  `json:"workload,omitempty"`
	Linux    *DiagLinux `json:"linux,omitempty"`

	Kept    bool   `json:"kept"`
	Cleanup string `json:"cleanup"`
}

func (r *Report) Write(dir string) error {
	r.FinishedAt = time.Now().UTC()
	r.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	return writeFileAtomic(filepath.Join(dir, "report.json"), jsonIndent(r))
}

// writeRunFile stores one of the run's JSON files next to the state.
func writeRunFile(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, name), jsonIndent(v))
}
