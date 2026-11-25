package remotepolicy

import (
	"strings"
	"testing"
)

func TestPlanMigrationValid(t *testing.T) {
	first, second := testPublic(1), testPublic(2)
	data := []byte(keyLine(t, first) + keyLine(t, second))
	plan, err := PlanMigration("legacy", "/home/legacy", data)
	if err != nil {
		t.Fatalf("PlanMigration: %v", err)
	}
	if plan.Name != "legacy" || plan.Home != "/home/legacy" || len(plan.Keys) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	if string(plan.Keys[0]) != string(first) || string(plan.Keys[1]) != string(second) {
		t.Fatal("keys reordered or corrupted")
	}
}

func TestPlanMigrationRejectsInvalidCandidate(t *testing.T) {
	key := keyLine(t, testPublic(1))
	for _, test := range []struct {
		name, user, home string
		keys             string
	}{
		{"bad user", "bad/name", "/home/user", key},
		{"empty user", "", "/home/user", key},
		{"home not under /home", "user", "/state/user", key},
		{"home root", "user", "/home", key},
		{"corrupt keys", "user", "/home/user", "not-a-key-line\n"},
		{"oversize keys", "user", "/home/user", strings.Repeat(key, MaxAuthorizedKeys+1)},
	} {
		if _, err := PlanMigration(test.user, test.home, []byte(test.keys)); err == nil {
			t.Fatalf("%s: invalid candidate accepted", test.name)
		}
	}
}

func TestPlanMigrationEmptyKeysIsValid(t *testing.T) {
	plan, err := PlanMigration("user", "/home/user", nil)
	if err != nil {
		t.Fatalf("empty authorized keys rejected: %v", err)
	}
	if plan.Name != "user" || plan.Home != "/home/user" || len(plan.Keys) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

// PlanMigration is deterministic and re-runnable: the same candidate yields the
// same keys in file order, so a caller can import them idempotently (identityd
// dedups by fingerprint).
func TestPlanMigrationDeterministic(t *testing.T) {
	first, second := testPublic(3), testPublic(4)
	data := []byte(keyLine(t, first) + keyLine(t, second))
	one, err := PlanMigration("user", "/home/user", data)
	if err != nil {
		t.Fatal(err)
	}
	two, err := PlanMigration("user", "/home/user", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Keys) != len(two.Keys) {
		t.Fatal("key count changed across re-runs")
	}
	for index := range one.Keys {
		if string(one.Keys[index]) != string(two.Keys[index]) {
			t.Fatalf("key %d changed across re-runs", index)
		}
	}
}
