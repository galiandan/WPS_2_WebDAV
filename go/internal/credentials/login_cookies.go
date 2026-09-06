package credentials

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Session-import cookie handling, ported from login.py. The server-side
// import route re-validates every cookie field itself — it must never trust
// that the login assistant already filtered the snapshot.

const (
	// DefaultCookieDomainSuffix mirrors DEFAULT_COOKIE_DOMAIN_SUFFIX.
	DefaultCookieDomainSuffix = "kdocs.cn"
	// MaxCookieSnapshotBytes mirrors MAX_COOKIE_SNAPSHOT_BYTES.
	MaxCookieSnapshotBytes = 4 * 1024 * 1024
	// maxCookiePartBytes mirrors the per-part cap in _safe_cookie_part.
	maxCookiePartBytes = 64 * 1024
)

// LoginError mirrors login.py's user-facing error type. It deliberately
// extends nothing HTTP-mappable: the import route answers a fixed 500 for
// it (Python's RuntimeError falls through to the generic handler), keeping
// the message for logs and the interactive helper only.
type LoginError struct{ Msg string }

func (e *LoginError) Error() string { return e.Msg }

func loginErrorf(format string, args ...any) error {
	return &LoginError{Msg: fmt.Sprintf(format, args...)}
}

// CookieSnapshot is the selected credential material plus the names that
// survived selection, mirroring credentials_from_cookies' return value.
type CookieSnapshot struct {
	Credentials Credentials
	Names       []string
}

// CredentialsFromCookies validates and selects browser cookie objects into
// a credential snapshot, mirroring login.py's credentials_from_cookies with
// the default require_refresh_cookie=True. Cookies are limited to the WPS
// domain suffix and to cookies that match the drive host; csrf and rtk must
// both survive.
func CredentialsFromCookies(cookies []any, baseURL string) (CookieSnapshot, error) {
	return credentialsFromCookies(cookies, baseURL, DefaultCookieDomainSuffix, true)
}

func credentialsFromCookies(cookies []any, baseURL string, domainSuffix string, requireRefreshCookie bool) (CookieSnapshot, error) {
	host, err := loginHostFromURL(baseURL)
	if err != nil {
		return CookieSnapshot{}, err
	}
	selected := selectCookies(cookies, host, domainSuffix)
	if len(selected) == 0 {
		return CookieSnapshot{}, loginErrorf("没有找到属于 WPS 云盘的登录 Cookie，请确认已经登录")
	}
	pairs := make([]string, 0, len(selected))
	byName := make(map[string]string, len(selected))
	names := make([]string, 0, len(selected))
	for _, cookie := range selected {
		pairs = append(pairs, cookieString(cookie, "name")+"="+cookieString(cookie, "value"))
		byName[strings.ToLower(cookieString(cookie, "name"))] = cookieString(cookie, "value")
		names = append(names, cookieString(cookie, "name"))
	}
	csrf := byName["csrf"]
	if csrf == "" {
		return CookieSnapshot{}, loginErrorf("登录 Cookie 中没有 csrf，请在 WPS 页面完成登录后重试")
	}
	if requireRefreshCookie && byName["rtk"] == "" {
		return CookieSnapshot{}, loginErrorf("登录 Cookie 中没有 rtk，无法启用自动续期；请使用此助手重新登录 WPS 后重试")
	}
	cookieHeader := strings.Join(pairs, "; ")
	if len(cookieHeader) > MaxCookieSnapshotBytes {
		return CookieSnapshot{}, loginErrorf("WPS Cookie 快照过大")
	}
	return CookieSnapshot{
		Credentials: Credentials{Cookie: cookieHeader, CSRFToken: csrf},
		Names:       names,
	}, nil
}

// loginHostFromURL mirrors _host_from_url: HTTPS, no userinfo, a kdocs.cn
// host (or subdomain), no explicit port beyond 443, case-folded and
// trailing-dot-free.
func loginHostFromURL(rawURL string) (string, error) {
	parts, err := url.Parse(rawURL)
	if err != nil {
		return "", loginErrorf("登录地址必须是不带账号信息的 HTTPS WPS 地址")
	}
	if parts.Port() != "" && parts.Port() != "443" {
		// url.Parse rejects syntactically invalid ports; an out-of-range
		// numeric port is the Python .port ValueError case.
		if _, err := parsePort(parts.Port()); err != nil {
			return "", loginErrorf("登录地址中的端口无效")
		}
		return "", loginErrorf("登录地址必须是不带账号信息的 HTTPS WPS 地址")
	}
	host := ""
	if parts.Hostname() != "" {
		host = strings.ToLower(strings.TrimRight(parts.Hostname(), "."))
	}
	if parts.Scheme != "https" || host == "" || parts.User != nil ||
		!(host == "kdocs.cn" || strings.HasSuffix(host, ".kdocs.cn")) {
		return "", loginErrorf("登录地址必须是不带账号信息的 HTTPS WPS 地址")
	}
	return host, nil
}

func parsePort(value string) (int, error) {
	port := 0
	for i := 0; i < len(value); i++ {
		digit := int(value[i] - '0')
		if digit < 0 || digit > 9 {
			return 0, fmt.Errorf("invalid port")
		}
		port = port*10 + digit
		if port > 65535 {
			return 0, fmt.Errorf("port out of range")
		}
	}
	return port, nil
}

// cookieString coerces a cookie field the way Python's str() wraps
// cookie.get(...): a missing field becomes "", other JSON types degrade to
// their printed form and then fail the safety checks.
func cookieString(cookie map[string]any, field string) string {
	value, present := cookie[field]
	if !present || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func domainWithoutDot(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "."))
}

func domainMatchesHost(domain string, host string) bool {
	normalized := domainWithoutDot(domain)
	return normalized != "" && (host == normalized || strings.HasSuffix(host, "."+normalized))
}

func domainIsAllowed(domain string, suffix string) bool {
	normalized := domainWithoutDot(domain)
	allowed := domainWithoutDot(suffix)
	return normalized != "" && allowed != "" &&
		(normalized == allowed || strings.HasSuffix(normalized, "."+allowed))
}

// cookieRank mirrors _cookie_rank: prefer exact-host cookies, then longer
// domains, then longer paths, so duplicate names resolve deterministically.
func cookieRank(cookie map[string]any, host string) [3]int {
	domain := domainWithoutDot(cookieString(cookie, "domain"))
	path := cookieString(cookie, "path")
	if path == "" {
		path = "/"
	}
	var exactHost int
	if domain == host {
		exactHost = 1
	}
	return [3]int{exactHost, len(domain), len(path)}
}

// rankBetter reports whether a beats b in Python's tuple ordering.
func rankBetter(a [3]int, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// safeCookiePart mirrors _safe_cookie_part: non-empty, bounded, no control
// characters, no ";" and — for names — no separator characters at all.
func safeCookiePart(value string, name bool) bool {
	if value == "" || len(value) > maxCookiePartBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if b := value[i]; b < 0x20 || b == 0x7F {
			return false
		}
	}
	if strings.Contains(value, ";") {
		return false
	}
	if name && strings.ContainsAny(value, "()<>@,;:\\\"/[]?={} \t") {
		return false
	}
	return true
}

// selectCookies mirrors _select_cookies: keep the best-ranked cookie per
// case-folded name, then order the survivors by name.
func selectCookies(cookies []any, host string, domainSuffix string) []map[string]any {
	selected := map[string]map[string]any{}
	for _, raw := range cookies {
		cookie, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		domain := cookieString(cookie, "domain")
		if !domainMatchesHost(domain, host) {
			continue
		}
		if !domainIsAllowed(domain, domainSuffix) {
			continue
		}
		name := cookieString(cookie, "name")
		value := cookieString(cookie, "value")
		if !safeCookiePart(name, true) || !safeCookiePart(value, false) {
			continue
		}
		key := strings.ToLower(name)
		previous, seen := selected[key]
		if !seen {
			selected[key] = cookie
			continue
		}
		if rankBetter(cookieRank(cookie, host), cookieRank(previous, host)) {
			selected[key] = cookie
		}
	}
	ordered := make([]map[string]any, 0, len(selected))
	for _, cookie := range selected {
		ordered = append(ordered, cookie)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return strings.ToLower(cookieString(ordered[i], "name")) < strings.ToLower(cookieString(ordered[j], "name"))
	})
	return ordered
}
