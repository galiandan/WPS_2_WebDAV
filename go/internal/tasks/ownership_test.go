package tasks

import (
	"context"
	"strings"
	"testing"
)

func TestOwnedHistoryKeepsAccountAndRootPoliciesSeparate(t *testing.T) {
	m := managerFor(t, func(context.Context, Spec, Binding) Result { return Result{Status: 200} })
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	specs := []Spec{taskSpec("/admin"), taskSpec("/same-name"), taskSpec("/same-name"), taskSpec("/old-root-secret")}
	specs[1].OwnerID, specs[1].PolicyVersion = a, 2
	specs[2].OwnerID, specs[2].PolicyVersion = b, 1
	specs[3].OwnerID, specs[3].PolicyVersion = a, 1
	ids := make([]string, len(specs))
	for i, spec := range specs {
		task, err := m.Submit(spec)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = task.ID
		awaitFinal(t, m, task.ID)
	}
	for _, tc := range []struct {
		owner    string
		version  uint64
		expected string
	}{{"installation", 0, ids[0]}, {a, 2, ids[1]}, {b, 1, ids[2]}, {a, 1, ids[3]}} {
		list, failed := m.ListOwned(tc.owner, tc.version)
		if failed || len(list) != 1 || list[0].ID != tc.expected {
			t.Fatalf("owner=%q version=%d: %+v", tc.owner, tc.version, list)
		}
	}
	spec, err := m.Spec(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	spec.Sources[0].Path = "/tampered"
	again, _ := m.Spec(ids[1])
	if again.Sources[0].Path == "/tampered" {
		t.Fatal("private spec snapshot aliases persisted state")
	}
	if OwnerMatches(again, b, 2) || OwnerMatches(again, a, 1) {
		t.Fatal("foreign account/policy matched")
	}
}

func TestTaskOwnerBindingRejectsMalformedOrUnversionedMembers(t *testing.T) {
	for _, owner := range []string{"../other", "user", strings.Repeat("a", 32)} {
		spec := taskSpec("/file")
		spec.OwnerID = owner
		if validateSpec(spec) == nil {
			t.Fatalf("accepted unsafe owner %q", owner)
		}
	}
}
