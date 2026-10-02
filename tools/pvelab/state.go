package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Phase is where a run stands. Cleanup reads the resource fields, not the
// phase, so a run interrupted in any phase can be cleaned.
type Phase string

const (
	PhaseNew              Phase = "new"
	PhasePreflighted      Phase = "preflighted"
	PhaseArtifactsReady   Phase = "artifacts-ready"
	PhaseVMAllocated      Phase = "vm-allocated"
	PhaseISOUploaded      Phase = "iso-uploaded"
	PhaseDiskImported     Phase = "disk-imported"
	PhaseVMConfigured     Phase = "vm-configured"
	PhaseVMStarted        Phase = "vm-started"
	PhaseConsoleAttached  Phase = "console-attached"
	PhaseScenarioRunning  Phase = "scenario-running"
	PhaseScenarioComplete Phase = "scenario-complete"
	PhaseCleaning         Phase = "cleaning"
	PhaseDone             Phase = "done"
	PhaseKept             Phase = "kept"
)

// State is the run journal. It is saved after every change on the Proxmox
// host so pvelab destroy can finish the job after a crash. It never holds a
// credential.
type State struct {
	RunID     string    `json:"run_id"`
	Phase     Phase     `json:"phase"`
	Node      string    `json:"node"`
	Pool      string    `json:"pool"`
	GitSHA    string    `json:"git_sha"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// VMID is recorded before the create call, so a VM whose creation was
	// interrupted is still found; VMCreated says the call succeeded.
	VMID      int    `json:"vmid,omitempty"`
	VMName    string `json:"vm_name,omitempty"`
	VMCreated bool   `json:"vm_created"`
	VMStarted bool   `json:"vm_started"`

	ISOStorage       string `json:"iso_storage,omitempty"`
	ISOVolume        string `json:"iso_volume,omitempty"`
	ISOUploadStarted bool   `json:"iso_upload_started,omitempty"`

	RemoteStaging string   `json:"remote_staging,omitempty"`
	DiskVolumes   []string `json:"disk_volumes,omitempty"`

	Keep    bool `json:"keep"`
	Kept    bool `json:"kept"`
	Cleaned bool `json:"cleaned"`

	path string
}

var runIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

// NewRunID is the UTC time and 32 random bits: sortable and collision-safe.
func NewRunID(now time.Time) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

// shortRunID is the random part, used in names Proxmox shows.
func shortRunID(runID string) string { return runID[len(runID)-8:] }

func vmName(runID string) string { return "goos-pvelab-" + shortRunID(runID) }

func isoName(runID string) string { return "goos-pvelab-" + runID + ".iso" }

func runsDir(root string) string { return filepath.Join(root, "build", "pve", "runs") }

func runDir(root, runID string) string { return filepath.Join(runsDir(root), runID) }

func NewState(root, runID string) (*State, error) {
	dir := runDir(root, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	s := &State{RunID: runID, Phase: PhaseNew, CreatedAt: now, path: filepath.Join(dir, "state.json")}
	return s, s.Save()
}

// LoadState reads a run journal by run ID.
func LoadState(root, runID string) (*State, error) {
	if !runIDPattern.MatchString(runID) {
		return nil, fmt.Errorf("%q is not a run ID", runID)
	}
	path := filepath.Join(runDir(root, runID), "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.RunID != runID {
		return nil, fmt.Errorf("%s names run %q", path, s.RunID)
	}
	s.path = path
	return &s, nil
}

func (s *State) Dir() string { return filepath.Dir(s.path) }

// Advance records a new phase and saves.
func (s *State) Advance(p Phase) error {
	s.Phase = p
	return s.Save()
}

// Save writes the journal atomically: a temporary file, synced, renamed.
func (s *State) Save() error {
	s.UpdatedAt = time.Now().UTC()
	return writeFileAtomic(s.path, jsonIndent(s))
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}
