package cache

import (
	"fmt"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// Cycle through capacity+1 folders so every operation needs an eviction.
func BenchmarkMetadataEviction(b *testing.B) {
	for _, capacity := range []int{128, 1024, 8192} {
		b.Run(fmt.Sprint(capacity), func(b *testing.B) {
			c, _ := New(Options{TTL: time.Hour, MaxFolders: capacity})
			keys := make([]Key, capacity+1)
			load := func() ([]model.RemoteEntry, error) { return nil, nil }
			for i := range keys {
				keys[i] = testKey(fmt.Sprint(i))
			}
			for _, key := range keys[:capacity] {
				c.GetOrLoad(key, load)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.GetOrLoad(keys[(capacity+i)%len(keys)], load)
			}
		})
	}
}
