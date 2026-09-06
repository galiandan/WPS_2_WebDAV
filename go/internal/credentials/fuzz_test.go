package credentials

import (
	"testing"
)

// Fuzz target for 06-testing-risk-gates.md 10.1 entry 8: Set-Cookie merge
// and expiry parsing. Invariants: no panic and a bounded update set.
func FuzzSetCookieMerge(f *testing.F) {
	seeds := []string{
		"bench-session=abc; Domain=.kdocs.cn; Path=/",
		"bench-session=; Max-Age=0",
		"csrf=tok; Expires=Wed, 21 Oct 2026 07:28:00 GMT",
		"rtk=r; Expires=invalid",
		"=novalue; a=b=c",
		"bench-session=x; bench-session=y",
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, blob []byte) {
		headers := []string{string(blob)}
		updates, _, _ := parseSetCookieUpdates(headers)
		if len(updates) > len(headers)*2 {
			t.Fatalf("unbounded update set: %d from %d headers", len(updates), len(headers))
		}
	})
}
