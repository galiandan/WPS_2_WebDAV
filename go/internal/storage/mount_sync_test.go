package storage

import (
	"sync/atomic"
	"testing"
)

func TestMountSyncSerializesSnapshotAndRebuild(t *testing.T) {
	var revision atomic.Int32
	oldEntered, release := make(chan struct{}), make(chan struct{})
	var sourceCalls atomic.Int32
	config := MultiSpaceConfig{
		MountsSource: func() ([]Mount, string, error) {
			sourceCalls.Add(1)
			id := []string{"initial", "old", "new"}[revision.Load()]
			return []Mount{{Name: "drive", GroupID: id, RootID: "0"}}, id, nil
		},
		SpaceFactory: func(group string) (SpaceClients, error) {
			if group == "old" {
				close(oldEntered)
				<-release
			}
			return SpaceClients{Lister: &groupLister{}}, nil
		},
	}
	m, err := NewMultiSpace(multiBudget(t), config)
	if err != nil {
		t.Fatal(err)
	}
	revision.Store(1)
	done := make(chan error, 2)
	go func() { done <- m.syncMounts() }()
	<-oldEntered
	// The rebuild must hold the synchronization lock across the snapshot read
	// and publish; otherwise a second caller can publish a newer route first.
	if m.syncMu.TryLock() {
		m.syncMu.Unlock()
		t.Error("rebuild is not serialized")
	}
	revision.Store(2)
	go func() { done <- m.syncMounts() }()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if m.mounts[0].GroupID != "new" {
		t.Fatalf("routing rolled back to %q", m.mounts[0].GroupID)
	}
	if sourceCalls.Load() != 3 {
		t.Fatalf("snapshot reads=%d", sourceCalls.Load())
	}
}

func TestMountSyncKeepsUnchangedStorageAndCache(t *testing.T) {
	mounts := []Mount{{Name: "a", GroupID: "a", RootID: "0"}}
	m, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.MountsSource = func() ([]Mount, string, error) { return mounts, "", nil }
	})
	if _, err := m.ListPath("/a"); err != nil {
		t.Fatal(err)
	}
	original := m.spaces["a"]
	mounts = append(mounts, Mount{Name: "b", GroupID: "b", RootID: "0"})
	if err := m.syncMounts(); err != nil {
		t.Fatal(err)
	}
	if m.spaces["a"] != original {
		t.Fatal("unchanged space was rebuilt")
	}
	if len(spy.groups) != 2 {
		t.Fatalf("factory calls=%v", spy.groups)
	}
	if _, err := m.ListPath("/a"); err != nil {
		t.Fatal(err)
	}
	if len(spy.listers["a"].calls) != 1 {
		t.Fatal("unchanged space lost cached listing")
	}
	// Mutating the source slice must not silently change the published snapshot.
	mounts[0].RootID = "other"
	if m.mounts[0].RootID != "0" {
		t.Fatal("published mounts alias source")
	}
	if err := m.syncMounts(); err != nil {
		t.Fatal(err)
	}
	if m.spaces["a"] == original {
		t.Fatal("changed root reused old storage")
	}
}
