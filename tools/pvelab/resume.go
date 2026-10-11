package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ResumePlan holds read-only, externally verified source evidence checked
// before pvelab attaches to a retained VM. Nothing in Source is written by
// resume.
type ResumePlan struct {
	Source         *State
	SourceReport   *Report
	SourceVMConfig map[string]string
	Artifacts      Artifacts
	Evidence       map[string][]byte
	EvidenceSHA256 string
	Candidate      GitInfo
}

// readCleanGit requires a completely clean candidate, including untracked
// files. A retained guest can validate only its original build inputs.
func readCleanGit(root string) (GitInfo, error) {
	git, err := ReadGit(root)
	if err != nil {
		return GitInfo{}, err
	}
	status, err := gitOutput(root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return GitInfo{}, err
	}
	if status != "" {
		return GitInfo{}, errors.New("the working tree has uncommitted or untracked changes")
	}
	return git, nil
}

func readReport(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &report, nil
}

func readArtifacts(path string) (Artifacts, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Artifacts{}, err
	}
	return decodeArtifacts(path, data)
}

func decodeArtifacts(path string, data []byte) (Artifacts, error) {
	var artifacts Artifacts
	if err := json.Unmarshal(data, &artifacts); err != nil {
		return Artifacts{}, fmt.Errorf("%s: %w", path, err)
	}
	return artifacts, nil
}

func decodeState(path, runID string, data []byte) (*State, error) {
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if state.RunID != runID {
		return nil, fmt.Errorf("%s names run %q", path, state.RunID)
	}
	state.path = path
	return &state, nil
}

func decodeReport(path string, data []byte) (*Report, error) {
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &report, nil
}

func decodeVMConfig(path string, data []byte) (map[string]string, error) {
	var config map[string]string
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

var sourceEvidenceFiles = [...]string{"state.json", "artifacts.json", "vm-config.json", "report.json"}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func writeDigestField(hash hash.Hash, data []byte) error {
	if err := binary.Write(hash, binary.BigEndian, uint64(len(data))); err != nil {
		return err
	}
	_, err := hash.Write(data)
	return err
}

// readSourceEvidence reads the fixed, canonical source bundle exactly once.
// Symlinks are rejected so the externally captured digest names these bytes,
// not a path whose target can change during resume.
func readSourceEvidence(dir string) (map[string][]byte, string, error) {
	hash := sha256.New()
	evidence := make(map[string][]byte, len(sourceEvidenceFiles))
	for _, name := range sourceEvidenceFiles {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, "", fmt.Errorf("source %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("source %s is not a regular file", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("source %s: %w", name, err)
		}
		if err := writeDigestField(hash, []byte(name)); err != nil {
			return nil, "", err
		}
		if err := writeDigestField(hash, data); err != nil {
			return nil, "", err
		}
		evidence[name] = data
	}
	return evidence, hex.EncodeToString(hash.Sum(nil)), nil
}

func sourceEvidenceSHA256(dir string) (string, error) {
	_, digest, err := readSourceEvidence(dir)
	return digest, err
}

func loadVerifiedSourceEvidence(root, runID, expected string) (string, map[string][]byte, error) {
	if !runIDPattern.MatchString(runID) {
		return "", nil, fmt.Errorf("%q is not a run ID", runID)
	}
	if !validSHA256(expected) {
		return "", nil, errors.New("source evidence SHA-256 is invalid")
	}
	dir := runDir(root, runID)
	evidence, digest, err := readSourceEvidence(dir)
	if err != nil {
		return "", nil, err
	}
	if digest != expected {
		return "", nil, errors.New("source evidence SHA-256 does not match trusted anchor")
	}
	return dir, evidence, nil
}

func sourceArtifactPath(root string, artifact Artifact) string {
	if filepath.IsAbs(artifact.Path) {
		return artifact.Path
	}
	return filepath.Join(root, artifact.Path)
}

// allowedResumeDifference is intentionally an allowlist. Changes outside
// host-lab tooling, documentation, and test-only sources could have produced
// different guest artifacts even when files with the old paths still exist.
func allowedResumeDifference(path string) bool {
	return strings.HasPrefix(path, "docs/") ||
		strings.HasPrefix(path, "tools/pvelab/") ||
		strings.HasPrefix(path, "tests/")
}

func verifySourceEquivalence(root, sourceSHA string) error {
	cmd := exec.Command("git", "diff", "--name-only", "--no-renames", sourceSHA, "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("compare source %s with candidate: %w", sourceSHA, err)
	}
	for _, path := range strings.Fields(string(out)) {
		if !allowedResumeDifference(path) {
			return fmt.Errorf("candidate changes native build input %q since source %s", path, sourceSHA)
		}
	}
	return nil
}

func resumeStateProblems(st *State) []string {
	var problems []string
	if !st.Keep || !st.Kept || st.Phase != PhaseKept {
		problems = append(problems, "run was not retained with --keep")
	}
	if st.Cleaned {
		problems = append(problems, "run is already cleaned")
	}
	if st.VMID == 0 || st.VMName == "" || st.ISOVolume == "" {
		problems = append(problems, "retained VM, ISO, or VM name is missing")
	}
	if st.GitSHA == "" {
		problems = append(problems, "source git SHA is missing")
	}
	return problems
}

func resumeReportProblems(st *State, report *Report, artifacts Artifacts) []string {
	var problems []string
	if report.RunID != st.RunID {
		problems = append(problems, "source report names another run")
	}
	if report.Git.SHA != st.GitSHA {
		problems = append(problems, "source report git SHA differs from state")
	}
	if report.Artifacts.ISOSHA256 == "" || report.Artifacts.DiskSHA256 == "" {
		problems = append(problems, "source report lacks artifact hashes")
	}
	if artifacts.ISO.SHA256 == "" || artifacts.Disk.SHA256 == "" {
		problems = append(problems, "source artifact manifest lacks hashes")
	}
	if report.Artifacts.ISOSHA256 != artifacts.ISO.SHA256 || report.Artifacts.DiskSHA256 != artifacts.Disk.SHA256 {
		problems = append(problems, "source artifact manifest differs from source report")
	}
	return problems
}

// prepareResume verifies read-only source evidence before it parses any of
// it or contacts Proxmox. The source state and report are never rewritten.
func prepareResume(root, runID string, candidate GitInfo, expectedEvidenceSHA256 string) (*ResumePlan, error) {
	dir, evidence, err := loadVerifiedSourceEvidence(root, runID, expectedEvidenceSHA256)
	if err != nil {
		return nil, err
	}
	st, err := decodeState(filepath.Join(dir, "state.json"), runID, evidence["state.json"])
	if err != nil {
		return nil, err
	}
	if problems := resumeStateProblems(st); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	report, err := decodeReport(filepath.Join(dir, "report.json"), evidence["report.json"])
	if err != nil {
		return nil, fmt.Errorf("source report: %w", err)
	}
	artifacts, err := decodeArtifacts(filepath.Join(dir, "artifacts.json"), evidence["artifacts.json"])
	if err != nil {
		return nil, fmt.Errorf("source artifacts: %w", err)
	}
	vmConfig, err := decodeVMConfig(filepath.Join(dir, "vm-config.json"), evidence["vm-config.json"])
	if err != nil {
		return nil, fmt.Errorf("source VM config: %w", err)
	}
	if problems := resumeReportProblems(st, report, artifacts); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	current, err := ManifestArtifacts(root, sourceArtifactPath(root, artifacts.ISO), sourceArtifactPath(root, artifacts.Disk))
	if err != nil {
		return nil, fmt.Errorf("current source artifacts: %w", err)
	}
	if current.ISO.SHA256 != report.Artifacts.ISOSHA256 || current.Disk.SHA256 != report.Artifacts.DiskSHA256 {
		return nil, errors.New("current artifact hashes differ from retained source report")
	}
	if err := verifySourceEquivalence(root, st.GitSHA); err != nil {
		return nil, err
	}
	return &ResumePlan{
		Source: st, SourceReport: report, SourceVMConfig: vmConfig, Artifacts: artifacts,
		Evidence: evidence, EvidenceSHA256: expectedEvidenceSHA256, Candidate: candidate,
	}, nil
}

// resumeOwnershipProblems is deliberately stricter than cleanup's ownership
// check: a resume must attach only to the exact original description, not a
// VM whose marker was edited to resemble the source run.
func resumeOwnershipProblems(st *State, config map[string]string, pool string) []string {
	problems := ownershipProblems(st, config, pool)
	want := strings.TrimSpace(ownershipDescription(st.RunID, st.GitSHA))
	if strings.TrimSpace(config["description"]) != want {
		problems = append(problems, "description differs from the original ownership record")
	}
	return problems
}

func readVMConfig(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config map[string]string
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

// resumeVolumeProblems compares the full recorded VM configuration. Proxmox's
// digest changes after legitimate reads and is the only transient field.
func resumeVolumeProblems(st *State, source, current map[string]string) []string {
	var problems []string
	for key, want := range source {
		if key == "digest" {
			continue
		}
		if got, ok := current[key]; !ok {
			problems = append(problems, "current VM config lacks "+key)
		} else if got != want {
			problems = append(problems, fmt.Sprintf("%s changed from %q to %q", key, want, got))
		}
	}
	for key := range current {
		if key != "digest" {
			if _, ok := source[key]; !ok {
				problems = append(problems, "current VM config adds "+key)
			}
		}
	}
	if volumeID(source["ide2"]) != st.ISOVolume {
		problems = append(problems, "source VM config ISO differs from state")
	}
	return problems
}

func (l *Lab) verifyResumeLayout(ctx context.Context, plan *ResumePlan) error {
	st := plan.Source
	vm, err := l.api.ClusterVM(ctx, st.VMID)
	if err != nil {
		return err
	}
	if vm == nil {
		return errors.New("retained VM is gone")
	}
	current, err := l.api.VMConfig(ctx, st.VMID)
	if err != nil {
		return err
	}
	if problems := resumeOwnershipProblems(st, current, vm.Pool); len(problems) > 0 {
		return fmt.Errorf("%w: %s", errNotOwned, strings.Join(problems, "; "))
	}
	if problems := resumeVolumeProblems(st, plan.SourceVMConfig, current); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (l *Lab) verifyResumeVM(ctx context.Context, plan *ResumePlan) error {
	if err := l.verifyResumeLayout(ctx, plan); err != nil {
		return err
	}
	status, err := l.api.VMStatus(ctx, plan.Source.VMID)
	if err != nil {
		return err
	}
	if status.Status != "running" && status.Status != "stopped" {
		return fmt.Errorf("retained VM status is %q, want running or stopped", status.Status)
	}
	return nil
}

func (l *Lab) stopRetainedVM(ctx context.Context, vmid int) error {
	status, err := l.api.VMStatus(ctx, vmid)
	if err != nil {
		return err
	}
	if status.Status == "stopped" {
		return nil
	}
	if status.Status != "running" {
		return fmt.Errorf("retained VM status is %q, want running or stopped", status.Status)
	}
	upid, err := l.api.VMAction(ctx, vmid, "stop")
	if err != nil {
		return err
	}
	tctx, cancel := stageCtx(ctx, taskTimeout)
	defer cancel()
	if err := l.api.WaitTask(tctx, upid); err != nil {
		return err
	}
	status, err = l.api.VMStatus(ctx, vmid)
	if err != nil {
		return err
	}
	if status.Status != "stopped" {
		return fmt.Errorf("retained VM status is %q after stop, want stopped", status.Status)
	}
	return nil
}

// recoverResumeStart protects a kept VM if resume fails after it stopped the
// source but before the replacement boot has begun. Verification is repeated
// before a best-effort start; a failure stays attached to the original error.
func recoverResumeStart(ctx context.Context, verify func(context.Context) error, status func(context.Context) (string, error), start func(context.Context) error) error {
	if err := verify(ctx); err != nil {
		return err
	}
	state, err := status(ctx)
	if err != nil {
		return err
	}
	switch state {
	case "running":
		return nil
	case "stopped":
		return start(ctx)
	default:
		return fmt.Errorf("retained VM status is %q during recovery", state)
	}
}

func (l *Lab) recoverRetainedVM(plan *ResumePlan, runErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	err := recoverResumeStart(ctx, func(ctx context.Context) error {
		return l.verifyResumeLayout(ctx, plan)
	}, func(ctx context.Context) (string, error) {
		status, err := l.api.VMStatus(ctx, plan.Source.VMID)
		return status.Status, err
	}, func(ctx context.Context) error {
		return l.startRetainedVM(ctx, plan.Source.VMID)
	})
	if err != nil {
		return errors.Join(runErr, fail(ClassInfra, "resume-recovery", err))
	}
	l.log.Infof("resume recovery restored retained VM")
	return runErr
}

func (l *Lab) startRetainedVM(ctx context.Context, vmid int) error {
	upid, err := l.api.VMAction(ctx, vmid, "start")
	if err != nil {
		return err
	}
	tctx, cancel := stageCtx(ctx, taskTimeout)
	defer cancel()
	return l.api.WaitTask(tctx, upid)
}

func copyResumeEvidence(plan *ResumePlan, evidenceDir string) error {
	for _, name := range sourceEvidenceFiles {
		dest := name
		if name == "report.json" {
			dest = "source-report.json"
		}
		if err := writeFileAtomic(filepath.Join(evidenceDir, dest), plan.Evidence[name]); err != nil {
			return fmt.Errorf("evidence %s: %w", dest, err)
		}
	}
	// Host information is diagnostic metadata only. It is deliberately not
	// used for ownership, artifact, or source-equivalence decisions.
	hostInfo := filepath.Join(plan.Source.Dir(), "host-info.json")
	info, err := os.Lstat(hostInfo)
	if err != nil {
		return fmt.Errorf("source host-info.json: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("source host-info.json is not a regular file")
	}
	data, err := os.ReadFile(hostInfo)
	if err != nil {
		return fmt.Errorf("source host-info.json: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(evidenceDir, "host-info.json"), data); err != nil {
		return fmt.Errorf("evidence host-info.json: %w", err)
	}
	return nil
}

func newResumeEvidenceDir(st *State) (string, string, error) {
	id, err := NewRunID(time.Now())
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(st.Dir(), "resumes", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	return id, dir, nil
}

// Resume reruns exactly the source report's scenario on its already-running,
// retained VM. It has no provision, upload, import, configuration, or cleanup
// path and never changes the source run's state, report, disks, ISO, or EFI.
func (l *Lab) Resume(ctx context.Context, runID, expectedEvidenceSHA256 string) int {
	candidate, err := readCleanGit(l.cfg.RepoRoot)
	if err != nil {
		l.log.Infof("error: resume candidate: %v", err)
		return ExitConfig
	}
	plan, err := prepareResume(l.cfg.RepoRoot, runID, candidate, expectedEvidenceSHA256)
	if err != nil {
		l.log.Infof("error: resume %s: %v", runID, err)
		return ExitConfig
	}
	if plan.Source.Node != l.cfg.Node || plan.Source.Pool != l.cfg.Pool {
		l.log.Infof("error: source run %s was made on node %s pool %s; configuration names node %s pool %s",
			runID, plan.Source.Node, plan.Source.Pool, l.cfg.Node, l.cfg.Pool)
		return ExitConfig
	}
	if _, ok := scenarios[plan.SourceReport.Scenario]; !ok {
		l.log.Infof("error: source report has unsupported scenario %q", plan.SourceReport.Scenario)
		return ExitConfig
	}
	if err := l.verifyResumeVM(ctx, plan); err != nil {
		l.log.Infof("error: resume %s: %v", runID, err)
		return ExitConfig
	}

	resumeID, dir, err := newResumeEvidenceDir(plan.Source)
	if err != nil {
		l.log.Infof("error: resume evidence: %v", err)
		return ExitInfra
	}
	report := &Report{
		Tool: "pvelab", RunID: plan.Source.RunID, StartedAt: time.Now().UTC(), Git: candidate,
		Proxmox: plan.SourceReport.Proxmox, VM: plan.SourceReport.VM, Scenario: plan.SourceReport.Scenario,
		Resume: &ResumeInfo{
			ID: resumeID, SourceRunID: plan.Source.RunID, SourceGitSHA: plan.Source.GitSHA,
			SourceISOSHA256: plan.SourceReport.Artifacts.ISOSHA256, SourceDiskSHA256: plan.SourceReport.Artifacts.DiskSHA256,
			SourceEvidenceSHA256: plan.EvidenceSHA256, CandidateGitSHA: candidate.SHA,
		},
		Kept: true, Cleanup: "skipped: resumed kept source",
	}
	report.Proxmox.VMID = plan.Source.VMID
	report.Artifacts.ISOSHA256 = plan.SourceReport.Artifacts.ISOSHA256
	report.Artifacts.DiskSHA256 = plan.SourceReport.Artifacts.DiskSHA256

	finish := func(result Result, runErr error) int {
		code := ExitPass
		if runErr != nil {
			class := classOf(runErr)
			report.Result, report.Error, code = string(class), runErr.Error(), exitCode(class)
			var stage *stageError
			if errors.As(runErr, &stage) {
				report.Stage = stage.stage
			}
			l.log.Infof("resume %s %s: %v", report.Scenario, strings.ToUpper(string(class)), runErr)
		} else {
			report.Result = string(result)
			l.reportScenario(scenarios[report.Scenario], result, report)
		}
		report.ToolExitStatus = code
		report.ExitStatusNote = "tool_exit_status is pvelab's inner status; record the outer Make/go run status separately"
		if err := report.Write(dir); err != nil {
			l.log.Infof("error: writing resume report: %v", err)
			code = ExitInfra
		}
		l.log.Infof("resume report: %s", relPath(l.cfg.RepoRoot, filepath.Join(dir, "report.json")))
		l.log.Infof("resume serial: %s", relPath(l.cfg.RepoRoot, filepath.Join(dir, "serial.log")))
		return code
	}
	if err := copyResumeEvidence(plan, dir); err != nil {
		return finish("", fail(ClassInfra, "resume-evidence", err))
	}
	// The retained VM was left running. Stop and start that same verified VM so
	// the new serial log independently records a normal boot before any test.
	if err := l.stopRetainedVM(ctx, plan.Source.VMID); err != nil {
		return finish("", fail(ClassInfra, "resume-stop", err))
	}
	console, err := AttachSerial(l.ssh, plan.Source.VMID, filepath.Join(dir, "serial.log"))
	if err != nil {
		return finish("", l.recoverRetainedVM(plan, fail(ClassInfra, "resume-console", err)))
	}
	guest := &Guest{con: console, logf: l.log.Debugf}
	closeConsole := func(runErr error) error {
		if err := console.Close(); err != nil {
			return errors.Join(runErr, fail(ClassInfra, "serial-evidence", err))
		}
		return runErr
	}
	if err := l.verifyResumeLayout(ctx, plan); err != nil {
		report.Checks = guest.checks
		runErr := closeConsole(fail(ClassInfra, "resume-ownership", err))
		return finish("", l.recoverRetainedVM(plan, runErr))
	}
	if err := l.startRetainedVM(ctx, plan.Source.VMID); err != nil {
		report.Checks = guest.checks
		runErr := closeConsole(fail(ClassInfra, "resume-start", err))
		return finish("", l.recoverRetainedVM(plan, runErr))
	}
	if err := guest.WaitBoot(ctx, l.cfg.Timeout); err != nil {
		report.Checks = guest.checks
		return finish("", closeConsole(err))
	}
	l.log.Infof("resume boot mode normal")
	l.log.Infof("resume shell ready")

	reconnectSerial := func(ctx context.Context) error {
		if err := console.Close(); err != nil {
			return err
		}
		next, err := AttachSerial(l.ssh, plan.Source.VMID, filepath.Join(dir, "serial.log"))
		if err != nil {
			return err
		}
		console = next
		guest.con = console
		if err := console.WriteLine(""); err != nil {
			return err
		}
		probeCtx, cancel := context.WithTimeout(ctx, l.cfg.Timeout)
		defer cancel()
		which, _, err := console.ReadUntilAny(probeCtx, []byte("\n"+shellPrompt), []byte(kernelPanic))
		if err == nil && which == 1 {
			return failf(ClassBoot, "serial-reconnect", "kernel panic after reconnect")
		}
		return err
	}
	startRetained := func(ctx context.Context) error {
		if err := l.verifyResumeLayout(ctx, plan); err != nil {
			return err
		}
		status, err := l.api.VMStatus(ctx, plan.Source.VMID)
		if err != nil {
			return err
		}
		if status.Status != "stopped" {
			return fmt.Errorf("retained VM status is %q, want stopped before restart", status.Status)
		}
		return l.startRetainedVM(ctx, plan.Source.VMID)
	}
	session := &Session{
		Guest: guest, Report: report, RequirePass: false, KeepRunning: true, WorkloadTimeout: 60 * time.Second,
		PowerState: func(ctx context.Context) (string, error) {
			status, err := l.api.VMStatus(ctx, plan.Source.VMID)
			return status.Status, err
		},
		ReconnectSerial: reconnectSerial, StartRetained: startRetained, EvidenceDir: dir,
		HostRunner: runHostCommand, OpenTerminal: openGuestTerminal, logf: l.log.Debugf,
	}
	result, runErr := scenarios[report.Scenario].Run(ctx, session)
	if runErr != nil && ctx.Err() != nil {
		runErr = failf(ClassInfra, "resume", "interrupted")
	}
	report.Checks = guest.checks
	return finish(result, closeConsole(runErr))

}
