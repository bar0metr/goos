package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func resumeFixture(t *testing.T) (string, *State, Artifacts, string) {
	t.Helper()
	root := t.TempDir()
	testGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "add", "README")
	testGit(t, root, "commit", "-qm", "fixture")
	sha := testGit(t, root, "rev-parse", "HEAD")

	input := filepath.Join(root, "build", "pve", "input")
	if err := os.MkdirAll(input, 0o700); err != nil {
		t.Fatal(err)
	}
	iso, disk := filepath.Join(input, "goos.iso"), filepath.Join(input, "goos-disk.img")
	if err := os.WriteFile(iso, []byte("source ISO"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(disk, []byte("source disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := ManifestArtifacts(root, iso, disk)
	if err != nil {
		t.Fatal(err)
	}
	id := "20261011T162559Z-478f6d50"
	st, err := NewState(root, id)
	if err != nil {
		t.Fatal(err)
	}
	st.Node, st.Pool, st.GitSHA = "clean27c86f3", "goos-lab", sha
	st.VMID, st.VMName, st.ISOVolume = 145, vmName(id), "Images:iso/"+isoName(id)
	st.DiskVolumes = []string{"VMs:145/vm-145-disk-1.raw", "VMs:145/vm-145-disk-0.raw"}
	st.Keep, st.Kept, st.Phase = true, true, PhaseKept
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	report := &Report{Tool: "pvelab", RunID: id, Git: GitInfo{SHA: sha}, Scenario: "smoke", Kept: true}
	report.Artifacts.ISOSHA256, report.Artifacts.DiskSHA256 = artifacts.ISO.SHA256, artifacts.Disk.SHA256
	if err := report.Write(st.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := writeRunFile(st.Dir(), "artifacts.json", artifacts); err != nil {
		t.Fatal(err)
	}
	if err := writeRunFile(st.Dir(), "vm-config.json", map[string]string{
		"virtio0":  "VMs:145/vm-145-disk-1.raw",
		"ide2":     st.ISOVolume + ",media=cdrom",
		"efidisk0": "VMs:145/vm-145-disk-0.raw,efitype=4m",
	}); err != nil {
		t.Fatal(err)
	}
	return root, st, artifacts, sha
}

func TestPrepareResumeKeepsSourceAndWritesFreshEvidence(t *testing.T) {
	root, st, _, sha := resumeFixture(t)
	stateBefore, err := os.ReadFile(filepath.Join(st.Dir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	reportBefore, err := os.ReadFile(filepath.Join(st.Dir(), "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := sourceEvidenceSHA256(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := prepareResume(root, st.RunID, GitInfo{SHA: sha}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source.RunID != st.RunID || plan.SourceReport.Scenario != "smoke" || plan.Artifacts.ISO.SHA256 == "" {
		t.Fatalf("plan %+v", plan)
	}
	resumeID, evidence, err := newResumeEvidenceDir(plan.Source)
	if err != nil {
		t.Fatal(err)
	}
	if resumeID == st.RunID || filepath.Dir(filepath.Dir(evidence)) != st.Dir() {
		t.Fatalf("evidence %q for source %q", evidence, st.Dir())
	}
	stateAfter, _ := os.ReadFile(filepath.Join(st.Dir(), "state.json"))
	reportAfter, _ := os.ReadFile(filepath.Join(st.Dir(), "report.json"))
	if !bytes.Equal(stateBefore, stateAfter) || !bytes.Equal(reportBefore, reportAfter) {
		t.Fatal("resume preparation changed retained source evidence")
	}
}

func TestPrepareResumeRejectsArtifactHashMismatch(t *testing.T) {
	root, st, _, sha := resumeFixture(t)
	anchor, err := sourceEvidenceSHA256(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build", "pve", "input", "goos.iso"), []byte("different ISO"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = prepareResume(root, st.RunID, GitInfo{SHA: sha}, anchor)
	if err == nil || !strings.Contains(err.Error(), "hashes differ") {
		t.Fatalf("err = %v, want retained-hash rejection", err)
	}
}

func TestResumeRejectsEditedOwnershipAndVolumes(t *testing.T) {
	_, st, _, _ := resumeFixture(t)
	config := map[string]string{
		"name":        st.VMName,
		"tags":        ownerTag,
		"description": ownershipDescription(st.RunID, "another-sha"),
	}
	if problems := resumeOwnershipProblems(st, config, st.Pool); len(problems) == 0 {
		t.Fatal("edited ownership description accepted")
	}
	source := map[string]string{
		"virtio0": "VMs:145/vm-145-disk-1.raw", "ide2": st.ISOVolume + ",media=cdrom", "efidisk0": "VMs:145/vm-145-disk-0.raw",
	}
	current := map[string]string{
		"virtio0": "VMs:145/other.raw", "ide2": source["ide2"], "efidisk0": source["efidisk0"],
	}
	if problems := resumeVolumeProblems(st, source, current); len(problems) == 0 {
		t.Fatal("changed retained disk accepted")
	}
	current = map[string]string{
		"virtio0": source["virtio0"], "ide2": source["ide2"], "efidisk0": source["efidisk0"],
		"boot": "order=virtio0", "unused0": "VMs:145/injected.raw",
	}
	source["boot"] = "order=ide2"
	if problems := resumeVolumeProblems(st, source, current); len(problems) == 0 {
		t.Fatal("boot-order or extra-drive drift accepted")
	}
	current = map[string]string{"virtio0": source["virtio0"] + ",iothread=1", "ide2": source["ide2"], "efidisk0": source["efidisk0"], "boot": source["boot"]}
	if problems := resumeVolumeProblems(st, source, current); len(problems) == 0 {
		t.Fatal("drive-option drift accepted")
	}
}

func TestPrepareResumeRejectsJointlyTamperedEvidence(t *testing.T) {
	root, st, _, sha := resumeFixture(t)
	anchor, err := sourceEvidenceSHA256(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state.json", "artifacts.json", "vm-config.json", "report.json"} {
		path := filepath.Join(st.Dir(), name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prepareResume(root, st.RunID, GitInfo{SHA: sha}, anchor); err == nil || !strings.Contains(err.Error(), "trusted anchor") {
		t.Fatalf("err = %v, want source-evidence anchor rejection", err)
	}
}

func TestResumeAttachFailureRecoveryStartsStoppedSource(t *testing.T) {
	var verified, started bool
	err := recoverResumeStart(context.Background(), func(context.Context) error {
		verified = true
		return nil
	}, func(context.Context) (string, error) {
		return "stopped", nil
	}, func(context.Context) error {
		started = true
		return nil
	})
	if err != nil || !verified || !started {
		t.Fatalf("recovery verified=%v started=%v err=%v", verified, started, err)
	}
}

func TestResumeSourceEquivalenceAllowsOnlyHostEvidenceChanges(t *testing.T) {
	root, _, _, sha := resumeFixture(t)
	for _, path := range []string{"docs/check.txt", "tools/pvelab/check.go", "tests/check.txt"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("host evidence\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, root, "add", "docs", "tools/pvelab", "tests")
	testGit(t, root, "commit", "-qm", "host-only candidate")
	if err := verifySourceEquivalence(root, sha); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(root, "kernel", "changed.go")
	if err := os.MkdirAll(filepath.Dir(kernel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kernel, []byte("package kernel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "add", "kernel")
	testGit(t, root, "commit", "-qm", "native input")
	if err := verifySourceEquivalence(root, sha); err == nil || !strings.Contains(err.Error(), "native build input") {
		t.Fatalf("err = %v, want native-input rejection", err)
	}
}
