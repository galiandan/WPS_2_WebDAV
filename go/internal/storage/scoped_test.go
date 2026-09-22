package storage

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

const scopedTestRoot = "/private-space/member-folder"

type scopedFake struct {
	entries       map[string]model.RemoteEntry
	children      map[string][]model.RemoteEntry
	calls         []string
	invalidations int
	writeErr      error
	lastUpload    UploadOptions
}

func scopedFixture() *scopedFake {
	file := model.RemoteEntry{ID: "file", Name: "note.txt", Kind: model.KindFile, ParentID: model.Ptr("physical-parent"), LinkID: model.Ptr("private-link")}
	return &scopedFake{entries: map[string]model.RemoteEntry{
		scopedTestRoot:               {ID: "pinned-root", Name: "member-folder", Kind: model.KindFolder, ParentID: model.Ptr("secret-parent")},
		scopedTestRoot + "/note.txt": file,
		scopedTestRoot + "/folder":   {ID: "folder", Name: "folder", Kind: model.KindFolder},
	}, children: map[string][]model.RemoteEntry{scopedTestRoot: {file}}}
}

func (f *scopedFake) InvalidateMetadataCache() { f.invalidations++ }
func (f *scopedFake) Metadata(p string) (model.RemoteEntry, error) {
	f.calls = append(f.calls, "metadata:"+p)
	if entry, ok := f.entries[p]; ok {
		return entry, nil
	}
	return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "private backing path "+p)
}
func (f *scopedFake) ListPath(p string) ([]model.RemoteEntry, error) {
	f.calls = append(f.calls, "list:"+p)
	return f.children[p], nil
}
func (f *scopedFake) OpenPath(ctx context.Context, p string, offset int64, length *int64) (DownloadStream, error) {
	f.calls = append(f.calls, "open:"+p)
	return &fakeStream{}, nil
}
func (f *scopedFake) UploadPath(ctx context.Context, p string, source io.Reader, options UploadOptions) (model.RemoteEntry, error) {
	f.lastUpload = options
	return f.write("upload", p)
}
func (f *scopedFake) CreateFolderPath(p string) (model.RemoteEntry, error) {
	return f.write("mkdir", p)
}
func (f *scopedFake) DeletePath(p string) error { _, err := f.write("delete", p); return err }
func (f *scopedFake) RenamePath(p, name string) (model.RemoteEntry, error) {
	return f.write("rename", p+":"+name)
}
func (f *scopedFake) MovePath(from, to string) (model.RemoteEntry, error) {
	return f.write("move", from+":"+to)
}
func (f *scopedFake) MoveToParentPath(from, to string) (model.RemoteEntry, error) {
	return f.write("move-parent", from+":"+to)
}
func (f *scopedFake) CopyPath(ctx context.Context, from, to string, options CopyOptions) (model.RemoteEntry, error) {
	return f.write("copy", from+":"+to)
}
func (f *scopedFake) ApplyBoundBatch(ctx context.Context, op, from, to, id, parent string) error {
	_, err := f.write("bound-"+op, from+":"+to+":"+id+":"+parent)
	return err
}
func (f *scopedFake) write(operation, p string) (model.RemoteEntry, error) {
	f.calls = append(f.calls, operation+":"+p)
	return model.RemoteEntry{ID: "result", Name: path.Base(p), Kind: model.KindFile, ParentID: model.Ptr("hidden"), LinkID: model.Ptr("hidden")}, f.writeErr
}

func newScopedFixture(t *testing.T, mutate func(*ScopedConfig)) (*Scoped, *scopedFake) {
	t.Helper()
	base := scopedFixture()
	config := ScopedConfig{RootPath: scopedTestRoot, RootID: "pinned-root", Read: true, Upload: true, Delete: true}
	if mutate != nil {
		mutate(&config)
	}
	scope, err := NewScoped(base, config)
	if err != nil {
		t.Fatal(err)
	}
	return scope, base
}

func isScopedDenied(err error) bool {
	value, ok := model.AsStorageError(err)
	return ok && value.Kind == model.KindPermissionDenied
}

func TestScopedReadPathsAndMetadataPrivacy(t *testing.T) {
	scope, base := newScopedFixture(t, nil)
	root, err := scope.Metadata("/")
	if err != nil || root.Name != "我的文件" || root.ParentID != nil || root.ID != "pinned-root" {
		t.Fatalf("root=%+v %v", root, err)
	}
	entries, err := scope.ListPath("/")
	if err != nil || len(entries) != 1 || entries[0].ParentID != nil || entries[0].LinkID != nil {
		t.Fatalf("entries=%+v %v", entries, err)
	}
	_, err = scope.ListChildren("/folder", model.RemoteEntry{ID: "outside-root", Kind: model.KindFolder})
	if err != nil {
		t.Fatal(err)
	}
	if last := base.calls[len(base.calls)-1]; last != "list:"+scopedTestRoot+"/folder" {
		t.Fatalf("forged traversal ID escaped: %s", last)
	}
	stream, err := scope.OpenPath(context.Background(), "/note.txt", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if last := base.calls[len(base.calls)-1]; last != "open:"+scopedTestRoot+"/note.txt" {
		t.Fatalf("download = %s", last)
	}
	_, err = scope.Metadata("/missing.txt")
	if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "member-folder") {
		t.Fatalf("backing error leaked: %v", err)
	}
	if base.invalidations != 5 {
		t.Fatalf("expected fresh root check every operation: %d", base.invalidations)
	}
}

func TestScopedNoSecondDecodeOrTraversal(t *testing.T) {
	scope, base := newScopedFixture(t, nil)
	for _, bad := range []string{"relative", "/../secret", "/a/../../secret", "//outside", "/a\\b", "/bad\x00name"} {
		before := len(base.calls)
		if _, err := scope.Metadata(bad); err == nil || len(base.calls) != before {
			t.Fatalf("invalid visible path reached backing store: %q %v", bad, err)
		}
	}
	for _, literal := range []string{"/%2F", "/%2e%2e", "/中文 %25", "/a/"} {
		mapped, err := scope.LockPath(literal)
		if err != nil {
			t.Fatal(err)
		}
		if mapped != scopedTestRoot+strings.TrimSuffix(literal, "/") {
			t.Fatalf("path decoded or changed: %q", mapped)
		}
	}
}

func TestScopedRootReplacementAndPolicyRevocationFailClosed(t *testing.T) {
	revoked := false
	scope, base := newScopedFixture(t, func(c *ScopedConfig) {
		c.Validate = func() error {
			if revoked {
				return errors.New("private user record")
			}
			return nil
		}
	})
	if _, err := scope.ListPath("/"); err != nil {
		t.Fatal(err)
	}
	base.entries[scopedTestRoot] = model.RemoteEntry{ID: "replacement", Kind: model.KindFolder}
	if _, err := scope.ListPath("/"); !isScopedDenied(err) {
		t.Fatalf("replacement exposed: %v", err)
	}
	if err := scope.DeletePath("/note.txt"); !isScopedDenied(err) {
		t.Fatalf("replacement mutated: %v", err)
	}
	base.entries[scopedTestRoot] = model.RemoteEntry{ID: "pinned-root", Kind: model.KindFolder}
	revoked = true
	before := len(base.calls)
	if _, err := scope.OpenPath(context.Background(), "/note.txt", 0, nil); !isScopedDenied(err) || len(base.calls) != before {
		t.Fatalf("revoked grant still used: %v", err)
	}
}

func TestScopedRevocationDuringRootResolutionStopsOperation(t *testing.T) {
	checks := 0
	scope, base := newScopedFixture(t, func(config *ScopedConfig) {
		config.Validate = func() error {
			checks++
			if checks > 1 {
				return errors.New("policy changed during root lookup")
			}
			return nil
		}
	})
	if _, err := scope.ListPath("/"); !isScopedDenied(err) {
		t.Fatalf("revocation during resolution ignored: %v", err)
	}
	for _, call := range base.calls {
		if strings.HasPrefix(call, "list:") {
			t.Fatal("revoked request reached directory contents")
		}
	}
}

func TestScopedPermissionsAndDAVUploadNewFile(t *testing.T) {
	readonly, _ := newScopedFixture(t, func(c *ScopedConfig) { c.Upload = false; c.Delete = false })
	for _, operation := range []func() error{
		func() error {
			_, e := readonly.UploadPath(context.Background(), "/new.txt", strings.NewReader("x"), UploadOptions{})
			return e
		},
		func() error { _, e := readonly.CreateFolderPath("/new"); return e },
		func() error { return readonly.DeletePath("/note.txt") },
		func() error { _, e := readonly.MovePath("/note.txt", "/new.txt"); return e },
		func() error {
			_, e := readonly.CopyPath(context.Background(), "/note.txt", "/copy.txt", CopyOptions{})
			return e
		},
	} {
		if err := operation(); !isScopedDenied(err) {
			t.Fatalf("read-only operation allowed: %v", err)
		}
	}
	upload, base := newScopedFixture(t, func(c *ScopedConfig) { c.Delete = false })
	if _, err := upload.UploadPath(context.Background(), "/new.txt", strings.NewReader("x"), UploadOptions{Overwrite: true}); err != nil || base.lastUpload.Overwrite {
		t.Fatalf("new DAV file denied or overwrite enabled: %v %+v", err, base.lastUpload)
	}
	if _, err := upload.UploadPath(context.Background(), "/note.txt", strings.NewReader("x"), UploadOptions{Overwrite: true}); !isScopedDenied(err) {
		t.Fatalf("existing file overwritten: %v", err)
	}
	if _, err := upload.UploadPath(context.Background(), "/note.txt", strings.NewReader("x"), UploadOptions{ExpectedID: "file"}); !isScopedDenied(err) {
		t.Fatalf("editor overwrite allowed: %v", err)
	}
	if _, err := upload.RenamePath("/note.txt", "new.txt"); !isScopedDenied(err) {
		t.Fatalf("upload-only rename allowed: %v", err)
	}
	noRead, _ := newScopedFixture(t, func(c *ScopedConfig) { c.Read = false })
	if _, err := noRead.Metadata("/note.txt"); !isScopedDenied(err) {
		t.Fatal("read denied ignored")
	}
	if _, err := noRead.ListPath("/"); !isScopedDenied(err) {
		t.Fatal("list denied ignored")
	}
	if _, err := noRead.OpenPath(context.Background(), "/note.txt", 0, nil); !isScopedDenied(err) {
		t.Fatal("download denied ignored")
	}
	if _, err := noRead.CreateFolderPath("/new"); err != nil {
		t.Fatalf("upload permission incorrectly requires read: %v", err)
	}
}

func TestScopedAllMutationsProtectRootAndMapBothEndpoints(t *testing.T) {
	scope, base := newScopedFixture(t, nil)
	for _, operation := range []func() error{
		func() error {
			_, e := scope.UploadPath(context.Background(), "/", strings.NewReader("x"), UploadOptions{})
			return e
		},
		func() error { _, e := scope.CreateFolderPath("/"); return e },
		func() error { return scope.DeletePath("/") },
		func() error { _, e := scope.RenamePath("/", "other"); return e },
		func() error { _, e := scope.MovePath("/note.txt", "/"); return e },
		func() error { _, e := scope.MoveToParentPath("/", "/folder"); return e },
		func() error { _, e := scope.CopyPath(context.Background(), "/", "/copy", CopyOptions{}); return e },
	} {
		if err := operation(); err == nil {
			t.Fatal("root mutation allowed")
		}
	}
	if _, err := scope.MoveToParentPath("/note.txt", "/"); err != nil {
		t.Fatal(err)
	}
	if got := base.calls[len(base.calls)-1]; got != "move-parent:"+scopedTestRoot+"/note.txt:"+scopedTestRoot {
		t.Fatalf("move endpoints: %s", got)
	}
	if err := scope.ApplyBoundBatch(context.Background(), "copy", "/note.txt", "/folder/note.txt", "file", "folder"); err != nil {
		t.Fatal(err)
	}
	if got := base.calls[len(base.calls)-1]; got != "bound-copy:"+scopedTestRoot+"/note.txt:"+scopedTestRoot+"/folder/note.txt:file:folder" {
		t.Fatalf("bound endpoints: %s", got)
	}
}

func TestScopedErrorsNeverRevealBackingPaths(t *testing.T) {
	scope, base := newScopedFixture(t, nil)
	for _, err := range []error{
		model.NewStorageError(model.KindAlreadyExists, "already exists: "+scopedTestRoot),
		model.NewWpsAPIError("download "+scopedTestRoot, 503, model.WpsCategoryUpstream),
		errors.New("local error " + scopedTestRoot),
	} {
		base.writeErr = err
		_, result := scope.CreateFolderPath("/new")
		if result == nil || strings.Contains(result.Error(), scopedTestRoot) {
			t.Fatalf("leaked error: %v", result)
		}
	}
}

func TestScopedMultiSpaceRootUsesConcreteID(t *testing.T) {
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{{Name: "Private", GroupID: "group", RootID: "root"}}
	})
	scope, err := NewScoped(multi, ScopedConfig{RootPath: "/Private", RootID: "root", Read: true})
	if err != nil {
		t.Fatal(err)
	}
	root, err := scope.Metadata("/")
	if err != nil || root.ID != "root" || root.Name != "我的文件" || root.ParentID != nil {
		t.Fatalf("virtual space leaked into member root: %+v %v", root, err)
	}
	entries, err := scope.ListChildren("/docs", model.RemoteEntry{ID: "outside"})
	if err != nil || len(entries) != 1 || entries[0].Name != "readme.txt" || entries[0].ParentID != nil {
		t.Fatalf("scoped mounted traversal: %+v %v", entries, err)
	}
}

func TestScopedWholeRootRequiresConcreteSingleSpace(t *testing.T) {
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticGroupID, c.SingleRootID = "group", "root"
	})
	scope, err := NewScoped(multi, ScopedConfig{RootPath: "/", RootID: "root", Read: true})
	if err != nil {
		t.Fatal(err)
	}
	root, err := scope.Metadata("/")
	if err != nil || root.ID != "root" {
		t.Fatalf("concrete single root failed: %+v %v", root, err)
	}
	mounts, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{{Name: "A", GroupID: "g1", RootID: "root"}, {Name: "B", GroupID: "g2", RootID: "root"}}
	})
	virtual, err := NewScoped(mounts, ScopedConfig{RootPath: "/", RootID: "multi-space-root", Read: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := virtual.Metadata("/"); !isScopedDenied(err) {
		t.Fatalf("synthetic multi-space root accepted as binding: %v", err)
	}
}

func TestScopedRootZeroBindingRejectsSameNamedSpaceRemap(t *testing.T) {
	group := "original-group"
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.MountsSource = func() ([]Mount, string, error) {
			return []Mount{{Name: "Same", GroupID: group, RootID: "0"}}, group, nil
		}
	})
	binding, err := multi.ScopeBinding("/Same")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewScoped(multi, ScopedConfig{RootPath: "/Same", RootID: "0", Binding: binding, Read: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scope.Metadata("/"); err != nil {
		t.Fatal(err)
	}
	if again, err := multi.ScopeBinding("/Same"); err != nil || again != binding {
		t.Fatal("unchanged namespace binding unstable")
	}
	group = "replacement-group"
	if _, err := scope.Metadata("/"); !isScopedDenied(err) {
		t.Fatalf("same root ID in new group bypassed binding: %v", err)
	}
	if _, err := multi.ScopeBinding("/"); !isScopedDenied(err) {
		t.Fatalf("virtual root received namespace binding: %v", err)
	}
}
