package credentials

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// All values in this file are fictional test fixtures.
const (
	fakeCookie    = "sid=fake-session; csrf=fake-csrf"
	fakeCSRF      = "fake-csrf"
	rotatedCookie = "sid=fake-rotated"
	rotatedCSRF   = "fake-csrf-rotated"
)

func privateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "credentials-test")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	return dir
}

func writeSecret(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGetReadsFreshSnapshotPerRequest(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", fakeCookie)
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)
	source := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)

	got, err := source.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Cookie != fakeCookie || got.CSRFToken != fakeCSRF {
		t.Errorf("Get = %+v, want the fake snapshot", got)
	}

	if err := os.WriteFile(cookiePath, []byte(rotatedCookie+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = source.Get()
	if err != nil {
		t.Fatalf("Get after rewrite: %v", err)
	}
	if got.Cookie != rotatedCookie {
		t.Errorf("Get after rewrite = %q, want the rotated value", got.Cookie)
	}
}

func TestGetFailureHidesPathAndContent(t *testing.T) {
	dir := privateDir(t)
	source := NewFileCredentialSource(filepath.Join(dir, "absent-cookie"), "", nil, 30)
	_, err := source.Get()
	if err == nil || err.Error() != "WPS operation failed: read credential file" {
		t.Fatalf("Get = %v, want the fixed read credential file error", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Error("error leaked the file path")
	}
}

func TestCredentialsFormattingIsRedacted(t *testing.T) {
	credentials := Credentials{Cookie: "sid=super-secret", CSRFToken: "csrf-secret"}
	for _, rendered := range []string{
		fmt.Sprintf("%v", credentials),
		fmt.Sprintf("%+v", credentials),
		fmt.Sprintf("%#v", credentials),
		fmt.Sprintf("%s", credentials),
	} {
		if strings.Contains(rendered, "super-secret") || strings.Contains(rendered, "csrf-secret") {
			t.Errorf("formatting leaked values: %s", rendered)
		}
	}
}

func TestCSRFCookieFallback(t *testing.T) {
	cases := map[string]string{
		"sid=a; csrf=tok": "tok",
		"sid=a; CSRF=tok": "tok",
		// ";" is the pair separator, so it never lands in the value -
		// Python's split(";") behaves the same.
		"csrf = spaced = x":    "spaced = x",
		"sid=a":                "",
		"":                     "",
		"a=1; csrf=with space": "with space",
	}
	for cookie, want := range cases {
		if got := CSRFFromCookie(cookie); got != want {
			t.Errorf("CSRFFromCookie(%q) = %q, want %q", cookie, got, want)
		}
	}
	if got := CSRFFromCookie("sid=a; csrf= tok "); got != "tok" {
		t.Errorf("trimmed csrf = %q", got)
	}
}

func TestCookieMap(t *testing.T) {
	values, order := cookieMap("sid=a; theme=dark; SID=last; broken; =empty; has space=1; final=")
	if len(order) != 3 || order[0] != "sid" || order[1] != "theme" || order[2] != "final" {
		t.Errorf("order = %v, want [sid theme final]", order)
	}
	if values["sid"] != "last" || values["theme"] != "dark" {
		t.Errorf("values = %v, want folded duplicate with last value", values)
	}
	if _, ok := values["final"]; !ok {
		t.Error("empty value must still be kept")
	}
}

func TestStoreSetCookieHeadersRotatesAndSyncsCSRF(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", fakeCookie)
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)
	source := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)

	headers := http.Header{}
	headers.Add("Set-Cookie", "sid=fake-rotated; Path=/")
	changed, err := source.StoreSetCookieHeaders(headers)
	if err != nil || !changed {
		t.Fatalf("StoreSetCookieHeaders = (%v, %v), want rotation", changed, err)
	}
	cookie, _ := os.ReadFile(cookiePath)
	if string(cookie) != "sid=fake-rotated; csrf=fake-csrf\n" {
		t.Errorf("cookie file = %q, want rotated sid kept csrf", cookie)
	}
	csrf, _ := os.ReadFile(csrfPath)
	if string(csrf) != fakeCSRF {
		t.Errorf("csrf file = %q, want untouched", csrf)
	}

	headers = http.Header{}
	headers.Add("Set-Cookie", "csrf="+rotatedCSRF)
	changed, err = source.StoreSetCookieHeaders(headers)
	if err != nil || !changed {
		t.Fatalf("csrf rotation = (%v, %v)", changed, err)
	}
	csrf, _ = os.ReadFile(csrfPath)
	if string(csrf) != rotatedCSRF+"\n" {
		t.Errorf("csrf file = %q, want rotated", csrf)
	}
}

func TestStoreSetCookieHeadersDeletesExpired(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", "sid=a; theme=dark")
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)
	source := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)

	headers := http.Header{}
	headers.Add("Set-Cookie", "sid=; Max-Age=0")
	changed, err := source.StoreSetCookieHeaders(headers)
	if err != nil || !changed {
		t.Fatalf("delete via Max-Age = (%v, %v)", changed, err)
	}
	cookie, _ := os.ReadFile(cookiePath)
	if string(cookie) != "theme=dark\n" {
		t.Errorf("cookie file = %q, want sid removed", cookie)
	}

	past := time.Now().UTC().Add(-time.Hour).Format("Mon, 02 Jan 2006 15:04:05 GMT")
	headers = http.Header{}
	headers.Add("Set-Cookie", "theme=x; Expires="+past)
	changed, err = source.StoreSetCookieHeaders(headers)
	if err != nil || !changed {
		t.Fatalf("delete via Expires = (%v, %v)", changed, err)
	}
	cookie, _ = os.ReadFile(cookiePath)
	if string(cookie) != "\n" {
		t.Errorf("cookie file = %q, want empty", cookie)
	}

	future := time.Now().UTC().Add(time.Hour).Format("Mon, 02 Jan 2006 15:04:05 GMT")
	headers = http.Header{}
	headers.Add("Set-Cookie", "theme=dark; Expires="+future)
	changed, err = source.StoreSetCookieHeaders(headers)
	if err != nil || !changed {
		t.Fatalf("future Expires = (%v, %v)", changed, err)
	}
	cookie, _ = os.ReadFile(cookiePath)
	if string(cookie) != "theme=dark\n" {
		t.Errorf("cookie file = %q, want theme kept", cookie)
	}
}

func TestStoreSetCookieHeadersMergesCaseInsensitively(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", "SID=old")
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)
	source := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)

	headers := http.Header{}
	headers.Add("Set-Cookie", "sid=new")
	headers.Add("Set-Cookie", "extra=1")
	if _, err := source.StoreSetCookieHeaders(headers); err != nil {
		t.Fatalf("StoreSetCookieHeaders: %v", err)
	}
	cookie, _ := os.ReadFile(cookiePath)
	if string(cookie) != "SID=new; extra=1\n" {
		t.Errorf("cookie file = %q, want stored spelling kept and new name appended", cookie)
	}
}

func TestStoreSetCookieHeadersNoChangeWritesNothing(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", "sid=a")
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)
	source := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)

	headers := http.Header{}
	headers.Add("Set-Cookie", "sid=a")
	changed, err := source.StoreSetCookieHeaders(headers)
	if err != nil || changed {
		t.Fatalf("no-change rotation = (%v, %v), want false", changed, err)
	}
	// Invalid headers are skipped entirely.
	headers = http.Header{}
	headers.Add("Set-Cookie", "not a cookie at all")
	if changed, err = source.StoreSetCookieHeaders(headers); err != nil || changed {
		t.Fatalf("invalid header = (%v, %v), want skipped", changed, err)
	}
	cookie, _ := os.ReadFile(cookiePath)
	if string(cookie) != "sid=a" {
		t.Errorf("cookie file = %q, want untouched", cookie)
	}
}

func TestReplaceCredentials(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", fakeCookie)
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)
	source := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)

	ok, err := source.ReplaceCredentials(Credentials{Cookie: rotatedCookie, CSRFToken: rotatedCSRF})
	if err != nil || !ok {
		t.Fatalf("ReplaceCredentials = (%v, %v)", ok, err)
	}
	cookie, _ := os.ReadFile(cookiePath)
	if string(cookie) != rotatedCookie+"\n" {
		t.Errorf("cookie file = %q", cookie)
	}
	csrf, _ := os.ReadFile(csrfPath)
	if string(csrf) != rotatedCSRF+"\n" {
		t.Errorf("csrf file = %q", csrf)
	}

	// Same values are a no-op success.
	ok, err = source.ReplaceCredentials(Credentials{Cookie: rotatedCookie, CSRFToken: rotatedCSRF})
	if err != nil || !ok {
		t.Errorf("idempotent replace = (%v, %v)", ok, err)
	}

	// Missing half of the pair or empty values are rejected without writes.
	if ok, _ := NewFileCredentialSource(cookiePath, "", nil, 30).
		ReplaceCredentials(Credentials{Cookie: "x", CSRFToken: "y"}); ok {
		t.Error("missing csrf path must be rejected")
	}
	if ok, _ := source.ReplaceCredentials(Credentials{Cookie: "x", CSRFToken: ""}); ok {
		t.Error("empty value must be rejected")
	}
}

// A broad csrf parent fails the pre-write snapshot in Python and here, and
// neither half is touched; the deeper half-written rollback lives in the
// securefile pair tests.
func TestReplaceCredentialsSnapshotFailureWritesNothing(t *testing.T) {
	cookieDir := privateDir(t)
	csrfDir := privateDir(t)
	cookiePath := writeSecret(t, cookieDir, "cookie", fakeCookie)
	csrfPath := writeSecret(t, csrfDir, "csrf", fakeCSRF)
	if err := os.Chmod(csrfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)

	_, err := source.ReplaceCredentials(Credentials{Cookie: rotatedCookie, CSRFToken: rotatedCSRF})
	if err == nil || err.Error() != "WPS operation failed: read credential file" {
		t.Fatalf("ReplaceCredentials = %v, want read credential file failure", err)
	}
	cookie, _ := os.ReadFile(cookiePath)
	if string(cookie) != fakeCookie {
		t.Errorf("cookie file = %q, want untouched", cookie)
	}
	csrf, _ := os.ReadFile(csrfPath)
	if string(csrf) != fakeCSRF {
		t.Errorf("csrf file = %q, want untouched", csrf)
	}
}

func TestRefreshRunsCommandAndDetectsChanges(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", fakeCookie)
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)
	// The helper rewrites the cookie file, then exits 0.
	source := NewFileCredentialSource(cookiePath, csrfPath,
		[]string{"sh", "-c", "printf 'sid=fake-refreshed\\n' > " + cookiePath}, 30)

	if _, err := source.Get(); err != nil {
		t.Fatalf("Get: %v", err)
	}
	refreshed, err := source.Refresh()
	if err != nil || !refreshed {
		t.Fatalf("Refresh = (%v, %v), want change detected", refreshed, err)
	}
	got, err := source.Get()
	if err != nil || got.Cookie != "sid=fake-refreshed" {
		t.Errorf("Get after refresh = (%+v, %v)", got, err)
	}

	// A second refresh with no further change reports false.
	refreshed, err = source.Refresh()
	if err != nil || refreshed {
		t.Errorf("second Refresh = (%v, %v), want false", refreshed, err)
	}
}

func TestRefreshFailureModes(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", fakeCookie)
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)

	nonzero := NewFileCredentialSource(cookiePath, csrfPath, []string{"sh", "-c", "exit 3"}, 30)
	if refreshed, err := nonzero.Refresh(); refreshed || err != nil {
		t.Errorf("non-zero helper = (%v, %v), want false, nil", refreshed, err)
	}
	missing := NewFileCredentialSource(cookiePath, csrfPath, []string{"definitely-not-a-binary-xyz"}, 30)
	if refreshed, err := missing.Refresh(); refreshed || err != nil {
		t.Errorf("missing helper = (%v, %v), want false, nil", refreshed, err)
	}
	slow := NewFileCredentialSource(cookiePath, csrfPath, []string{"sleep", "2"}, 0.2)
	start := time.Now()
	if refreshed, err := slow.Refresh(); refreshed || err != nil {
		t.Errorf("slow helper = (%v, %v), want false, nil", refreshed, err)
	} else if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("slow helper took %v, want the timeout to bound it", elapsed)
	}
	badTimeout := NewFileCredentialSource(cookiePath, csrfPath, []string{"true"}, 0)
	if _, err := badTimeout.Refresh(); err == nil || err.Error() != "refresh_timeout must be positive" {
		t.Errorf("bad timeout = %v, want the Python ValueError wording", err)
	}
	noCommand := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)
	if refreshed, err := noCommand.Refresh(); err != nil {
		t.Errorf("commandless refresh error: %v", err)
	} else if refreshed {
		t.Error("commandless refresh without change must be false")
	}
}

func TestStaticCredentialSource(t *testing.T) {
	static := &StaticCredentialSource{Credentials: Credentials{Cookie: fakeCookie, CSRFToken: fakeCSRF}}
	got, err := static.Get()
	if err != nil || got.Cookie != fakeCookie {
		t.Errorf("Get = (%+v, %v)", got, err)
	}
	if refreshed, _ := static.Refresh(); refreshed {
		t.Error("static Refresh must be false")
	}
	if changed, _ := static.StoreSetCookieHeaders(http.Header{}); changed {
		t.Error("static Store must be false")
	}
	if ok, _ := static.ReplaceCredentials(Credentials{Cookie: "a", CSRFToken: "b"}); ok {
		t.Error("static Replace must be false")
	}
}

func TestValidateValues(t *testing.T) {
	if err := ValidateValues(Credentials{Cookie: fakeCookie, CSRFToken: fakeCSRF}); err != nil {
		t.Errorf("clean values: %v", err)
	}
	if err := ValidateValues(Credentials{Cookie: "a\tb", CSRFToken: ""}); err == nil ||
		err.Error() != "WPS operation failed: credential value contains a control character" {
		t.Errorf("control char = %v", err)
	}
	if err := ValidateValues(Credentials{Cookie: strings.Repeat("a", 4<<20+1), CSRFToken: ""}); err == nil ||
		err.Error() != "WPS operation failed: credential value is too large" {
		t.Errorf("oversized = %v", err)
	}
}

// TestConcurrentAccessIsRaceFree exercises the source from many goroutines;
// it is meaningful under -race, per the B305 gate.
func TestConcurrentAccessIsRaceFree(t *testing.T) {
	dir := privateDir(t)
	cookiePath := writeSecret(t, dir, "cookie", fakeCookie)
	csrfPath := writeSecret(t, dir, "csrf", fakeCSRF)
	source := NewFileCredentialSource(cookiePath, csrfPath, nil, 30)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				_, _ = source.Get()
				_, _ = source.Refresh()
				headers := http.Header{}
				headers.Add("Set-Cookie", fmt.Sprintf("sid=fake-%d-%d", i, round))
				_, _ = source.StoreSetCookieHeaders(headers)
				_, _ = source.ReplaceCredentials(Credentials{Cookie: "sid=fake-x", CSRFToken: "fake-y"})
			}
		}(i)
	}
	wg.Wait()
	if _, err := source.Get(); err != nil {
		t.Errorf("final Get: %v", err)
	}
}
