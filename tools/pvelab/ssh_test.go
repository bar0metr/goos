package main

import (
	"os/exec"
	"testing"
)

func TestShellQuoteRoundTrip(t *testing.T) {
	words := []string{"plain", "", "a b", "it's", `"dq"`, "$(id)", "`id`", "a;rm -rf /", "x\ny", "*", "~", "VMs:154/vm-154-disk-1.raw"}
	for _, w := range words {
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(w)).Output()
		if err != nil {
			t.Fatalf("%q: %v", w, err)
		}
		if string(out) != w {
			t.Errorf("%q came back as %q", w, out)
		}
	}
}

func TestShellJoin(t *testing.T) {
	if got := shellJoin("qm", "disk", "import", "154", "/var/tmp/goos-pvelab/x y/disk.img"); got != `qm disk import 154 '/var/tmp/goos-pvelab/x y/disk.img'` {
		t.Errorf("got %s", got)
	}
}

func TestStagingPathGuard(t *testing.T) {
	const id = "20261002T130501Z-a1b2c3d4"
	if err := checkStagingPath(stagingDir(id), id); err != nil {
		t.Fatalf("own staging refused: %v", err)
	}
	bad := []struct{ dir, id string }{
		{"/", id},
		{"/var/tmp", id},
		{stagingRoot, id},
		{stagingRoot + "/", id},
		{stagingRoot + "/other", id},
		{stagingRoot + "/" + id + "/..", id},
		{stagingRoot + "/../" + id, id},
		{"/var/tmp/goos-pvelab-evil/" + id, id},
		{stagingRoot + "/x/" + id, id},
		{stagingDir(id), ""},
		{stagingRoot + "/..", ".."},
	}
	for _, b := range bad {
		if err := checkStagingPath(b.dir, b.id); err == nil {
			t.Errorf("removal of %q for run %q allowed", b.dir, b.id)
		}
	}
}

func TestRootPrefix(t *testing.T) {
	if got := (SSH{Sudo: true}).Root("qm", "list"); len(got) != 4 || got[0] != "sudo" || got[1] != "-n" {
		t.Errorf("sudo: %v", got)
	}
	if got := (SSH{}).Root("qm", "list"); len(got) != 2 {
		t.Errorf("root: %v", got)
	}
	if commandName([]string{"sudo", "-n", "qm", "disk", "import"}) != "qm disk" {
		t.Error("commandName ignores sudo badly")
	}
}
