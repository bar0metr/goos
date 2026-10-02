package main

import (
	"strings"
	"testing"
)

func ownedConfig(runID string) map[string]string {
	return map[string]string{
		"name":        vmName(runID),
		"tags":        ownerTag,
		"description": ownershipDescription(runID, "4514a64c"),
	}
}

func TestOwnershipGuard(t *testing.T) {
	const id = "20261002T130501Z-a1b2c3d4"
	st := &State{RunID: id, VMName: vmName(id), Pool: "goos-lab"}
	if p := ownershipProblems(st, ownedConfig(id), "goos-lab"); len(p) != 0 {
		t.Fatalf("own VM refused: %v", p)
	}
	cases := map[string]func(map[string]string) (map[string]string, string){
		"other run": func(c map[string]string) (map[string]string, string) {
			c["description"] = ownershipDescription("20261002T130501Z-ffffffff", "x")
			return c, "goos-lab"
		},
		"no marker": func(c map[string]string) (map[string]string, string) {
			c["description"] = "run-id=" + id
			return c, "goos-lab"
		},
		"no tag": func(c map[string]string) (map[string]string, string) {
			c["tags"] = "prod"
			return c, "goos-lab"
		},
		"renamed": func(c map[string]string) (map[string]string, string) {
			c["name"] = "database"
			return c, "goos-lab"
		},
		"other pool": func(c map[string]string) (map[string]string, string) { return c, "prod" },
		"no pool":    func(c map[string]string) (map[string]string, string) { return c, "" },
		"unrelated VM": func(map[string]string) (map[string]string, string) {
			return map[string]string{"name": "goos-test", "description": "goos"}, "goos-lab"
		},
	}
	for name, mutate := range cases {
		config, pool := mutate(ownedConfig(id))
		if p := ownershipProblems(st, config, pool); len(p) == 0 {
			t.Errorf("%s: accepted as owned", name)
		}
	}
}

func TestHasTag(t *testing.T) {
	if !hasTag("prod;goos-pvelab", ownerTag) || !hasTag("goos-pvelab", ownerTag) || hasTag("goos-pvelab-x", ownerTag) {
		t.Error("tag matching wrong")
	}
}

func TestNewUnusedVolume(t *testing.T) {
	before := map[string]string{"efidisk0": "VMs:154/vm-154-disk-0.raw,efitype=4m", "unused0": "VMs:154/old.raw"}
	after := map[string]string{"efidisk0": before["efidisk0"], "unused0": "VMs:154/old.raw", "unused1": "VMs:154/vm-154-disk-1.raw"}
	got, err := newUnusedVolume(before, after)
	if err != nil || got != "VMs:154/vm-154-disk-1.raw" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := newUnusedVolume(after, after); err == nil {
		t.Error("no new volume accepted")
	}
	after["unused2"] = "VMs:154/vm-154-disk-2.raw"
	if _, err := newUnusedVolume(before, after); err == nil || !strings.Contains(err.Error(), "several") {
		t.Errorf("two new volumes: %v", err)
	}
}

func TestRequiredPrivileges(t *testing.T) {
	need := requiredPrivileges(&Config{Pool: "lab", Node: "n", DiskStorage: "VMs", EFIStorage: "VMs", ISOStorage: "Images"})
	if !contains(need["/storage/Images"], "Datastore.AllocateTemplate") || !contains(need["/pool/lab"], "VM.Allocate") {
		t.Errorf("privileges %v", need)
	}
	if n := len(need["/storage/VMs"]); n != 2 {
		t.Errorf("shared disk and EFI storage listed %d privileges, want 2", n)
	}
}
