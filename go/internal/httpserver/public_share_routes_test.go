package httpserver

import (
	"strings"
	"testing"
)

func TestShareAuthExemptionsOnlyMatchLiteralReadRoutes(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, tc := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "/share/" + id, true}, {"HEAD", "/share/" + id, true},
		{"POST", "/api/share/" + id + "/unlock", true},
		{"GET", "/api/share/" + id + "/entries", true}, {"GET", "/api/share/" + id + "/download", true},
		{"GET", "/api/share/" + id + "/preview", true}, {"GET", "/api/share/" + id + "/thumbnail", true},
		{"GET", "/api/share/" + id + "/tasks", false}, {"DELETE", "/api/share/" + id + "/entries", false},
		{"PUT", "/api/share/" + id + "/download", false}, {"POST", "/share/" + id, false},
		{"GET", "/share/" + id + "/", false}, {"GET", "/api/share/" + id + "//entries", false},
		{"GET", "/api/share/" + id + "/%64ownload", false}, {"GET", "/api/share/" + id + "/download/../users", false},
		{"GET", "/api/share/" + id + "x/download", false}, {"GET", "/share/" + strings.Repeat("A", 32), false},
	} {
		_, _, ok := PublicShareRoute(tc.method, tc.path)
		if ok != tc.ok {
			t.Fatalf("%s %s exemption=%v", tc.method, tc.path, ok)
		}
	}
}
