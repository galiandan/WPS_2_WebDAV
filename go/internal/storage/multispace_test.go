package storage

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
)

// groupLister records which parent ids one space's storage asked for.
type groupLister struct {
	groupID  string
	children map[string][]model.RemoteEntry
	calls    []string
}

func (g *groupLister) IterEntries(parentID string, _ wps.IterOptions) ([]model.RemoteEntry, error) {
	g.calls = append(g.calls, parentID)
	return g.children[parentID], nil
}

// factorySpy mirrors Python's per-mount child client construction: one full
// client surface per group, remembered so tests can inspect each space.
type factorySpy struct {
	groups  []string
	listers map[string]*groupLister
	clients map[string]*fakeClient
}

func newFactorySpy() *factorySpy {
	return &factorySpy{listers: map[string]*groupLister{}, clients: map[string]*fakeClient{}}
}

func (f *factorySpy) build(groupID string) (SpaceClients, error) {
	f.groups = append(f.groups, groupID)
	client := newFakeClient()
	lister := &groupLister{groupID: groupID, children: client.children}
	f.clients[groupID] = client
	f.listers[groupID] = lister
	return SpaceClients{Lister: lister, Writer: client, Downloader: client}, nil
}

func multiBudget(t *testing.T) *budget.Budget {
	t.Helper()
	b, err := budget.New(budget.Config{MaxUploads: 1, MaxDownloads: 1, MaxConnections: 4, TransferWaitTimeout: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestMulti(t *testing.T, transferBudget *budget.Budget, mutate func(*MultiSpaceConfig)) (*MultiSpace, *factorySpy) {
	t.Helper()
	spy := newFactorySpy()
	config := MultiSpaceConfig{SpaceFactory: spy.build}
	if mutate != nil {
		mutate(&config)
	}
	if transferBudget == nil {
		transferBudget = multiBudget(t)
	}
	multi, err := NewMultiSpace(transferBudget, config)
	if err != nil {
		t.Fatal(err)
	}
	return multi, spy
}

func TestSingleFallbackWithoutMounts(t *testing.T) {
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticGroupID = "group-1"
	})
	// The full path reaches the single storage unchanged; its default root
	// id is "0" (SingleRootID), so the listing asks for that parent.
	if _, err := multi.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	if got := spy.listers[""].calls; fmt.Sprint(got) != "[0]" {
		t.Fatalf("single fallback listing = %v", got)
	}
	_, err := multi.Metadata("/missing.txt")
	if err == nil || err.Error() != "entry not found: missing.txt" {
		t.Fatalf("single metadata error = %v", err)
	}
	if got := spy.listers[""].calls; fmt.Sprint(got) != "[0]" {
		// The metadata call reuses the cached root listing from the
		// listing above, so no second upstream call appears.
		t.Fatalf("single metadata resolution = %v", got)
	}
}

func TestPendingGroupWithoutMountsFails(t *testing.T) {
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticGroupID = ""
	})
	// The root listing is a silent empty tuple in the reference; only
	// non-root paths report the unconfigured workspace.
	entries, err := multi.ListPath("/")
	if err != nil || len(entries) != 0 {
		t.Fatalf("pending root listing = %v, %v", entries, err)
	}
	_, err = multi.Metadata("/x.txt")
	if err == nil || err.Error() != "WPS workspace is not configured" {
		t.Fatalf("pending metadata error = %v", err)
	}
	kind, ok := model.AsStorageError(err)
	if !ok || kind.Kind != model.KindEntryNotFound {
		t.Fatalf("pending kind = %v", err)
	}
	_, err = multi.UploadPath(context.Background(), "/x.txt", sizedReader("x"), UploadOptions{})
	if err == nil || err.Error() != "WPS workspace is not configured" {
		t.Fatalf("pending upload error = %v", err)
	}
}

func TestRootListingReturnsOnlyMounts(t *testing.T) {
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{
			{Name: "alpha", GroupID: "gA", RootID: "root"},
			{Name: "beta", GroupID: "gB", RootID: "rootB"},
		}
	})
	entries, err := multi.ListPath("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("root listing = %d entries", len(entries))
	}
	for i, want := range []struct {
		id    string
		name  string
		group string
	}{{"space:gA", "alpha", "gA"}, {"space:gB", "beta", "gB"}} {
		entry := entries[i]
		if entry.ID != want.id || entry.Name != want.name || entry.Kind != model.KindFolder {
			t.Fatalf("mount entry %d = %+v", i, entry)
		}
		if entry.ParentID == nil || *entry.ParentID != "multi-space-root" {
			t.Fatalf("mount entry %d parent = %v", i, entry.ParentID)
		}
	}
	// Building the spaces touches no upstream listing.
	for _, lister := range spy.listers {
		if len(lister.calls) != 0 {
			t.Fatalf("root listing hit upstream: %v", lister.calls)
		}
	}
}

func TestFirstSegmentRoutesToSpace(t *testing.T) {
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{
			{Name: "alpha", GroupID: "gA", RootID: "root"},
			{Name: "beta", GroupID: "gB", RootID: "root"},
		}
	})
	// A bare space name resolves to the virtual space entry.
	entry, err := multi.Metadata("/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != "space:gA" || entry.Name != "alpha" || entry.Kind != model.KindFolder {
		t.Fatalf("space entry = %+v", entry)
	}
	// Deeper paths drop the first segment and resolve inside the space.
	if _, err := multi.ListPath("/beta"); err != nil {
		t.Fatal(err)
	}
	if got := spy.listers["gB"].calls; fmt.Sprint(got) != "[root]" {
		t.Fatalf("beta calls = %v", got)
	}
	if len(spy.listers["gA"].calls) != 0 {
		t.Fatalf("alpha unexpectedly called: %v", spy.listers["gA"].calls)
	}
	_, err = multi.Metadata("/alpha/docs/readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := spy.listers["gA"].calls; fmt.Sprint(got) != "[root docs]" {
		t.Fatalf("alpha resolution = %v", got)
	}
	_, err = multi.Metadata("/nope/x.txt")
	if err == nil || err.Error() != "WPS space not found: nope" {
		t.Fatalf("unknown space error = %v", err)
	}
}

func TestRootWritesRejected(t *testing.T) {
	ctx := context.Background()
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{{Name: "alpha", GroupID: "gA", RootID: "root"}}
	})
	_, err := multi.UploadPath(ctx, "/", sizedReader("x"), UploadOptions{})
	if err == nil || err.Error() != "WPS space not found: " {
		t.Fatalf("mount root upload error = %v", err)
	}
	if err := multi.DeletePath("/"); err == nil || err.Error() != "WPS space not found: " {
		t.Fatalf("mount root delete error = %v", err)
	}
	if _, err := multi.CreateFolderPath("/"); err == nil || err.Error() != "WPS space not found: " {
		t.Fatalf("mount root create error = %v", err)
	}
	if _, err := multi.RenamePath("/", "x"); err == nil || err.Error() != "WPS space not found: " {
		t.Fatalf("mount root rename error = %v", err)
	}
	if _, err := multi.MovePath("/", "/alpha/x"); err == nil || err.Error() != "WPS space not found: " {
		t.Fatalf("mount root move error = %v", err)
	}

	single, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticGroupID = "group-1"
	})
	_, err = single.UploadPath(ctx, "/", sizedReader("x"), UploadOptions{})
	if err == nil || err.Error() != "the root cannot be used as a file name" {
		t.Fatalf("single root upload error = %v", err)
	}
	if err := single.DeletePath("/"); err == nil || err.Error() != "the root cannot be deleted" {
		t.Fatalf("single root delete error = %v", err)
	}
	if _, err := single.CreateFolderPath("/"); err == nil || err.Error() != "the root cannot be used as a file name" {
		t.Fatalf("single root create error = %v", err)
	}
}

func TestWritesRouteIntoSpaces(t *testing.T) {
	ctx := context.Background()
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{
			{Name: "alpha", GroupID: "gA", RootID: "root"},
			{Name: "beta", GroupID: "gB", RootID: "root"},
		}
	})
	result, err := multi.UploadPath(ctx, "/alpha/new.txt", sizedReader("hello"), UploadOptions{Size: model.Ptr(int64(5))})
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "new.txt" {
		t.Fatalf("upload result = %+v", result)
	}
	if got := spy.clients["gA"].uploadCalls; len(got) != 1 || got[0].name != "new.txt" || string(got[0].body) != "hello" {
		t.Fatalf("alpha upload = %+v", got)
	}
	if len(spy.clients["gB"].uploadCalls) != 0 {
		t.Fatal("beta received alpha's upload")
	}
	if _, err := multi.CreateFolderPath("/beta/folder"); err != nil {
		t.Fatal(err)
	}
	if got := spy.clients["gB"].folderCalls; fmt.Sprint(got) != "[[root folder]]" {
		t.Fatalf("beta folder calls = %v", got)
	}
	if err := multi.DeletePath("/alpha/top.txt"); err != nil {
		t.Fatal(err)
	}
	if got := spy.clients["gA"].deleteCalls; fmt.Sprint(got) != "[top]" {
		t.Fatalf("alpha delete calls = %v", got)
	}
	stream, err := multi.OpenPath(ctx, "/beta/top.txt", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if got := spy.clients["gB"].downloadCalls; len(got) != 1 || got[0].entryID != "top" {
		t.Fatalf("beta download calls = %+v", got)
	}
}

func TestSameSpaceAndCrossSpaceMoves(t *testing.T) {
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{
			{Name: "alpha", GroupID: "gA", RootID: "root"},
			{Name: "beta", GroupID: "gB", RootID: "root"},
		}
	})
	// Same-space move with the name preserved.
	result, err := multi.MovePath("/alpha/top.txt", "/alpha/docs/top.txt")
	if err != nil {
		t.Fatal(err)
	}
	if result.ParentID == nil || *result.ParentID != "docs" {
		t.Fatalf("same-space move parent = %v", result.ParentID)
	}
	if got := spy.clients["gA"].moveCalls; fmt.Sprint(got) != "[[top root docs]]" {
		t.Fatalf("alpha move calls = %v", got)
	}
	// Cross-space by full destination path.
	_, err = multi.MovePath("/alpha/top.txt", "/beta/other.txt")
	if err == nil || err.Error() != "cross-space move is not supported" {
		t.Fatalf("cross-space move error = %v", err)
	}
	kind, ok := model.AsStorageError(err)
	if !ok || kind.Kind != model.KindUnsupportedOperation {
		t.Fatalf("cross-space move kind = %v", err)
	}
	// Cross-space by destination parent.
	_, err = multi.MoveToParentPath("/alpha/top.txt", "/beta")
	if err == nil || err.Error() != "cross-space move is not supported" {
		t.Fatalf("cross-space move-to-parent error = %v", err)
	}
	if got := spy.clients["gB"].moveCalls; len(got) != 0 {
		t.Fatalf("beta move calls = %v", got)
	}
	// Same-space move to another parent by parent path.
	if _, err := multi.MoveToParentPath("/beta/top.txt", "/beta/docs"); err != nil {
		t.Fatal(err)
	}
	if got := spy.clients["gB"].moveCalls; fmt.Sprint(got) != "[[top root docs]]" {
		t.Fatalf("beta move calls = %v", got)
	}
}

func TestHotMountUpdateReplacesRoutes(t *testing.T) {
	mounts := []Mount{{Name: "alpha", GroupID: "gA", RootID: "root"}}
	source := func() ([]Mount, string, error) { return mounts, "group-1", nil }
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.MountsSource = source
	})
	entries, err := multi.ListPath("/")
	if err != nil || len(entries) != 1 {
		t.Fatalf("before update: %v, %v", entries, err)
	}
	// A login-side remap adds a second space.
	mounts = append(mounts, Mount{Name: "beta", GroupID: "gB", RootID: "root"})
	entries, err = multi.ListPath("/")
	if err != nil || len(entries) != 2 {
		t.Fatalf("after update: %v, %v", entries, err)
	}
	if entries[1].ID != "space:gB" {
		t.Fatalf("new mount entry = %+v", entries[1])
	}
	if _, ok := spy.listers["gB"]; !ok {
		t.Fatal("new space was not built")
	}
	if _, err := multi.Metadata("/beta"); err != nil {
		t.Fatalf("new space metadata: %v", err)
	}
	// Removing a space drops its routing.
	mounts = []Mount{{Name: "beta", GroupID: "gB", RootID: "root"}}
	_, err = multi.Metadata("/alpha")
	if err == nil || err.Error() != "WPS space not found: alpha" {
		t.Fatalf("removed space error = %v", err)
	}
	entries, err = multi.ListPath("/")
	if err != nil || len(entries) != 1 || entries[0].Name != "beta" {
		t.Fatalf("after removal: %v, %v", entries, err)
	}
}

func TestHotUpdateWithPendingGroupBuildsSingle(t *testing.T) {
	mounts := []Mount{}
	group := ""
	source := func() ([]Mount, string, error) { return mounts, group, nil }
	multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.MountsSource = source
		c.SingleSelection = func() (string, string, bool, error) {
			return group, "selected-root", true, nil
		}
	})
	// No mounts and no group yet: non-root paths report unconfigured,
	// and the root listing is silently empty (reference behaviour).
	if _, err := multi.Metadata("/x.txt"); err == nil || err.Error() != "WPS workspace is not configured" {
		t.Fatalf("pre-config error = %v", err)
	}
	entries, err := multi.ListPath("/")
	if err != nil || len(entries) != 0 {
		t.Fatalf("pre-config root listing = %v, %v", entries, err)
	}
	// The login flow resolves a group: the single storage appears.
	group = "group-9"
	if _, err := multi.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	if got := spy.listers[""].calls; fmt.Sprint(got) != "[selected-root]" {
		t.Fatalf("single built with workspace root: %v", got)
	}
}

func TestDuplicateMountNamesRejected(t *testing.T) {
	spy := newFactorySpy()
	_, err := NewMultiSpace(multiBudget(t), MultiSpaceConfig{
		SpaceFactory: spy.build,
		StaticMounts: []Mount{
			{Name: "alpha", GroupID: "gA", RootID: "root"},
			{Name: "alpha", GroupID: "gB", RootID: "root"},
		},
	})
	if err == nil || err.Error() != "WPS space names must be unique" {
		t.Fatalf("duplicate names error = %v", err)
	}
}

func TestHotUpdateFailureKeepsOldRoutes(t *testing.T) {
	mounts := []Mount{{Name: "alpha", GroupID: "gA", RootID: "root"}}
	reject := false
	source := func() ([]Mount, string, error) {
		if reject {
			// A workspace write racing the read can yield duplicates.
			return []Mount{
				{Name: "dup", GroupID: "gX", RootID: "root"},
				{Name: "dup", GroupID: "gY", RootID: "root"},
			}, "group-1", nil
		}
		return mounts, "group-1", nil
	}
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.MountsSource = source
	})
	reject = true
	if _, err := multi.ListPath("/"); err == nil || err.Error() != "WPS space names must be unique" {
		t.Fatalf("hot duplicate error = %v", err)
	}
	// The previous routing still serves and the next good update rebuilds.
	reject = false
	if _, err := multi.Metadata("/alpha"); err != nil {
		t.Fatalf("old routing lost after failed update: %v", err)
	}
}

func TestStatusRootID(t *testing.T) {
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{
			{Name: "alpha", GroupID: "gA", RootID: "rootA"},
			{Name: "beta", GroupID: "gB", RootID: "rootB"},
		}
	})
	got, err := multi.StatusRootID()
	if err != nil || got != "rootA" {
		t.Fatalf("mounts status root = %q, %v", got, err)
	}
	single, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticGroupID = "group-1"
		c.SingleRootID = "fixed-root"
	})
	got, err = single.StatusRootID()
	if err != nil || got != "fixed-root" {
		t.Fatalf("single status root = %q, %v", got, err)
	}
	empty, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticGroupID = ""
	})
	got, err = empty.StatusRootID()
	if err != nil || got != "0" {
		t.Fatalf("empty status root = %q, %v", got, err)
	}
}

func TestOneAndManySpacesRoute(t *testing.T) {
	for _, count := range []int{1, 128} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			mounts := make([]Mount, 0, count)
			for i := 0; i < count; i++ {
				mounts = append(mounts, Mount{Name: "space-" + strconv.Itoa(i), GroupID: "g" + strconv.Itoa(i), RootID: "root"})
			}
			multi, spy := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
				c.StaticMounts = mounts
			})
			entries, err := multi.ListPath("/")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != count {
				t.Fatalf("root listing = %d entries", len(entries))
			}
			for i, entry := range entries {
				if entry.Name != "space-"+strconv.Itoa(i) {
					t.Fatalf("entry %d out of order: %+v", i, entry)
				}
			}
			mid := "space-" + strconv.Itoa(count/2)
			entry, err := multi.Metadata("/" + mid)
			if err != nil {
				t.Fatal(err)
			}
			if entry.ID != "space:g"+strconv.Itoa(count/2) {
				t.Fatalf("mid space entry = %+v", entry)
			}
			if _, err := multi.ListPath("/" + mid); err != nil {
				t.Fatal(err)
			}
			if got := spy.listers["g"+strconv.Itoa(count/2)].calls; fmt.Sprint(got) != "[root]" {
				t.Fatalf("mid space calls = %v", got)
			}
		})
	}
}

func TestAllSpacesShareTheProcessBudget(t *testing.T) {
	shared := multiBudget(t)
	multi, _ := newTestMulti(t, shared, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{
			{Name: "alpha", GroupID: "gA", RootID: "root"},
			{Name: "beta", GroupID: "gB", RootID: "root"},
		}
	})
	if multi.spaces["alpha"].budget != shared || multi.spaces["beta"].budget != shared {
		t.Fatal("spaces did not receive the shared budget")
	}
	held, err := shared.AcquireUpload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	// Space A holds the only slot, so space B must wait on the same pool.
	_, err = multi.UploadPath(context.Background(), "/beta/x.txt", sizedReader("x"), UploadOptions{})
	if err == nil || err.Error() != "too many uploads are active" {
		t.Fatalf("cross-space budget error = %v", err)
	}
}

func TestMultiSetRootDelegation(t *testing.T) {
	single, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticGroupID = "group-1"
	})
	if err := single.SetRootName("Drive"); err != nil {
		t.Fatal(err)
	}
	root, err := single.Root()
	if err != nil || root.Name != "Drive" {
		t.Fatalf("renamed root = %+v, %v", root, err)
	}
	// The factory builds the single storage against the fake "root" table;
	// switching the id moves the listing to that key.
	if err := single.SetRootID("docs"); err != nil {
		t.Fatal(err)
	}
	if got, err := single.StatusRootID(); err != nil || got != "docs" {
		t.Fatalf("status root after switch = %q, %v", got, err)
	}

	mounted, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{{Name: "alpha", GroupID: "gA", RootID: "root"}}
	})
	// Named spaces keep their own roots: SetRootID is a no-op.
	if err := mounted.SetRootID("other"); err != nil {
		t.Fatal(err)
	}
	entry, err := mounted.Root()
	if err != nil || entry.ID != "multi-space-root" {
		t.Fatalf("mounted root = %+v, %v", entry, err)
	}
	if err := mounted.SetRootName("Named"); err != nil {
		t.Fatal(err)
	}
	if entry, err = mounted.Root(); err != nil || entry.Name != "Named" {
		t.Fatalf("renamed mounted root = %+v, %v", entry, err)
	}
	if err := mounted.SetRootName(""); err == nil || err.Error() != "root_name is required" {
		t.Fatalf("empty name error = %v", err)
	}
	if err := single.SetRootID(""); err == nil || err.Error() != "root_id is required" {
		t.Fatalf("empty root error = %v", err)
	}
}

var _ io.Reader = (*byteReader)(nil)
