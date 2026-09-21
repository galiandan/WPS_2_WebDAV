package storage

import (
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func TestSearchRefreshDropsMetadataForEveryMountedSpace(t *testing.T) {
	for _, mounted := range []bool{false, true} {
		multi, factory := newTestMulti(t, nil, func(config *MultiSpaceConfig) {
			config.StaticGroupID, config.SingleRootID = "group", "root"
			config.Space.CacheTTLSeconds = 60
			if mounted {
				config.StaticMounts = []Mount{{Name: "A", GroupID: "one", RootID: "root"}, {Name: "B", GroupID: "two", RootID: "root"}}
			}
		})
		paths := []string{"/"}
		if mounted {
			paths = []string{"/A", "/B"}
		}
		for _, path := range paths {
			if _, err := multi.ListPath(path); err != nil {
				t.Fatal(err)
			}
		}
		for _, lister := range factory.listers {
			lister.children["root"] = []model.RemoteEntry{{ID: "fresh", Name: "new-account.txt", Kind: model.KindFile}}
		}
		multi.InvalidateMetadataCache()
		for _, path := range paths {
			entries, err := multi.ListPath(path)
			if err != nil || len(entries) != 1 || entries[0].Name != "new-account.txt" {
				t.Fatalf("mounted=%t path=%s retained old cached entries: %+v, %v", mounted, path, entries, err)
			}
		}
	}
}
