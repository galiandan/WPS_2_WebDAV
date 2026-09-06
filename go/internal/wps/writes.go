// The write surface ports client.py's confirmed WPS write endpoints. Every
// call resolves the CSRF token before touching the network, builds the body
// with the exact captured field order, and fails with operation-only errors
// that never echo the response or the URL.

package wps

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// pyJSONID mirrors _json_id: an all-decimal id travels as a JSON number,
// everything else stays a string. Python int() also drops leading zeros
// ("007" becomes 7), so the number is normalized the same way. The only
// divergence is intentionally narrow: Python's isdecimal() also accepts
// non-ASCII decimal digits, which cannot occur in WPS identifiers.
func pyJSONID(value string) any {
	if !isASCIIDecimal(value) {
		return value
	}
	trimmed := strings.TrimLeft(value, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	return json.Number(trimmed)
}

func isASCIIDecimal(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// CreateFolder mirrors create_folder: the captured v5 folder endpoint with
// the exact JSON body. The name and CSRF checks are plain errors, matching
// the Python ValueError surface before any request is built.
func (c *Client) CreateFolder(parentID string, name string) (model.RemoteEntry, error) {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return model.RemoteEntry{}, errors.New("name must be one remote folder name")
	}
	current, err := c.currentCredentials()
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if current.CSRFToken == "" {
		return model.RemoteEntry{}, errors.New("csrf_token is required for write operation")
	}
	groupID, err := c.GroupID()
	if err != nil {
		return model.RemoteEntry{}, err
	}
	body := &pyObject{
		keys: []string{"groupid", "parentid", "name", "owner", "parsed", "csrfmiddlewaretoken"},
		values: map[string]any{
			"groupid":             pyJSONID(groupID),
			"parentid":            pyJSONID(parentID),
			"name":                name,
			"owner":               true,
			"parsed":              true,
			"csrfmiddlewaretoken": current.CSRFToken,
		},
	}
	encoded, err := dumpPYValue(body)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/files/folder",
		Method:     http.MethodPost,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return model.RemoteEntry{}, model.NewWpsAPIError("create folder", 0, model.WpsCategoryUpstream)
	}
	return entryFromItem(payload)
}
