package credentials

import (
	"net/http"
	"strings"
	"time"
)

// parseSetCookieUpdates turns Set-Cookie headers into name→value updates,
// where a nil value means the cookie expired and must be dropped. It also
// tracks whether a csrf cookie was seen, because its rotation is mirrored
// into the CSRF file. Parse errors skip the header, like Python's
// CookieError handling.
func parseSetCookieUpdates(headers []string) (updates map[string]*string, csrfSeen bool, csrfValue string) {
	updates = map[string]*string{}
	for _, header := range headers {
		cookie, err := http.ParseSetCookie(header)
		if err != nil {
			continue
		}
		if cookieExpired(cookie) {
			updates[cookie.Name] = nil
			if strings.EqualFold(cookie.Name, "csrf") {
				csrfSeen = true
				csrfValue = ""
			}
			continue
		}
		value := cookie.Value
		updates[cookie.Name] = &value
		if strings.EqualFold(cookie.Name, "csrf") {
			csrfSeen = true
			csrfValue = cookie.Value
		}
	}
	return updates, csrfSeen, csrfValue
}

// cookieExpired mirrors _cookie_expired: a parseable Max-Age decides first
// (an unparseable one is skipped, falling through to Expires), then an
// Expires timestamp at or before now.
func cookieExpired(cookie *http.Cookie) bool {
	if cookie.MaxAge < 0 {
		return true
	}
	if cookie.MaxAge > 0 {
		return false
	}
	if !cookie.Expires.IsZero() {
		return !cookie.Expires.After(time.Now())
	}
	return false
}

// cookieMap parses the stored cookie header into values and their first
// spelling order: pairs without "=", with an empty or whitespace-bearing
// name are skipped; duplicate names fold case-insensitively with the last
// value winning.
func cookieMap(cookieHeader string) (map[string]string, []string) {
	values := map[string]string{}
	var order []string
	for _, item := range strings.Split(cookieHeader, ";") {
		name, value, found := strings.Cut(item, "=")
		name = strings.TrimSpace(name)
		if !found || name == "" || strings.ContainsAny(name, " \t\r\n") {
			continue
		}
		existing := ""
		for _, key := range order {
			if strings.EqualFold(key, name) {
				existing = key
				break
			}
		}
		if existing == "" {
			order = append(order, name)
			existing = name
		}
		values[existing] = strings.TrimSpace(value)
	}
	return values, order
}

// joinCookie renders the merged header in stored order, skipping names that
// were removed.
func joinCookie(values map[string]string, order []string) string {
	var parts []string
	for _, name := range order {
		if value, ok := values[name]; ok {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

// CSRFFromCookie extracts the csrf item from a cookie header when the CSRF
// file holds no value.
func CSRFFromCookie(cookie string) string {
	for _, item := range strings.Split(cookie, ";") {
		name, value, found := strings.Cut(item, "=")
		if found && strings.EqualFold(strings.TrimSpace(name), "csrf") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
