package storage

import (
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func TestDAVViewMapsOneSubtreeWithoutHidingOtherBrowserSpaces(t *testing.T) {
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{
			{Name: "A", GroupID: "group-a", RootID: "root-a"},
			{Name: "B", GroupID: "group-b", RootID: "root-b"},
		}
	})
	spy.listers["group-a"].children["root-a"] = []model.RemoteEntry{
		{ID: "web", Name: "web", Kind: model.KindFolder, ParentID: model.Ptr("root-a")},
	}
	spy.listers["group-a"].children["web"] = []model.RemoteEntry{
		{ID: "a-file", Name: "a.txt", Kind: model.KindFile, ParentID: model.Ptr("web")},
	}
	spy.listers["group-b"].children["root-b"] = []model.RemoteEntry{
		{ID: "b-file", Name: "b.txt", Kind: model.KindFile, ParentID: model.Ptr("root-b")},
	}

	view, err := NewDAVView(multi, func() (string, error) { return "/A/web", nil })
	if err != nil {
		t.Fatal(err)
	}
	entries, err := view.ListPath("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("DAV root entries = %+v, want A/web contents", entries)
	}
	if len(spy.listers["group-b"].calls) != 0 {
		t.Fatalf("DAV view touched B: %v", spy.listers["group-b"].calls)
	}

	// The browser view remains a two-space view; DAV scoping is independent.
	rootEntries, err := multi.ListPath("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(rootEntries) != 2 || rootEntries[0].Name != "A" || rootEntries[1].Name != "B" {
		t.Fatalf("browser root entries = %+v", rootEntries)
	}
}

func TestDAVViewRejectsInvalidPrefix(t *testing.T) {
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{{Name: "A", GroupID: "group-a", RootID: "root-a"}}
	})
	view, err := NewDAVView(multi, func() (string, error) { return "relative", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.Metadata("/"); err == nil {
		t.Fatal("invalid DAV prefix was accepted")
	}
}

func TestDAVViewSnapshotKeepsLockAndOperationPathsOnOneRoot(t *testing.T) {
	prefix := "/A/web"
	view, err := NewDAVView(&MultiSpace{}, func() (string, error) { return prefix, nil })
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := view.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	prefix = "/B/other"
	for _, path := range []string{"/file", "/100%25 + 笔记.txt", "/"} {
		locked, err := pinned.LockPath(path)
		if err != nil {
			t.Fatal(err)
		}
		mapped, err := pinned.mapPath(path)
		if err != nil || locked != mapped || !strings.HasPrefix(locked, "/A/web") {
			t.Fatalf("lock=%q operation=%q err=%v", locked, mapped, err)
		}
	}
	next, err := view.LockPath("/file")
	if err != nil || next != "/B/other/file" {
		t.Fatalf("next=%q err=%v", next, err)
	}
}
