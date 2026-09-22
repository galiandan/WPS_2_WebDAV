package httpserver

import "strings"

// PublicShareRoute is deliberately literal. An auth exemption must describe
// exactly the same routes as dispatch, never a broad /api/share prefix.
func PublicShareRoute(method, rawPath string) (id, action string, ok bool) {
	parts := strings.Split(rawPath, "/")
	if len(parts) == 3 && parts[0] == "" && parts[1] == "share" && validPublicShareID(parts[2]) && (method == "GET" || method == "HEAD") {
		return parts[2], "page", true
	}
	if len(parts) != 5 || parts[0] != "" || parts[1] != "api" || parts[2] != "share" || !validPublicShareID(parts[3]) {
		return "", "", false
	}
	action = parts[4]
	if method == "POST" && action == "unlock" {
		return parts[3], action, true
	}
	if method == "GET" {
		switch action {
		case "info", "entries", "download", "preview", "thumbnail":
			return parts[3], action, true
		}
	}
	return "", "", false
}
func validPublicShareID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
