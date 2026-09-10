// The write surface ports client.py's confirmed WPS write endpoints. Every
// call resolves the CSRF token before touching the network, builds the body
// with the exact captured field order, and fails with operation-only errors
// that never echo the response or the URL.

package wps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
	return json.Number(pyJSONIDString(value))
}

// pyJSONIDString mirrors str(_json_id(value)): the same decimal
// normalization as pyJSONID, rendered back to the string form the query
// builder sends.
func pyJSONIDString(value string) string {
	if !isASCIIDecimal(value) {
		return value
	}
	trimmed := strings.TrimLeft(value, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

// pyInt renders one Go integer as Python's json.dumps would.
func pyInt(value int64) json.Number {
	return json.Number(strconv.FormatInt(value, 10))
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
		Path:       c.drivePath("/3rd/drive/api/v5/files/folder"),
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

// Rename mirrors rename: the confirmed v3 endpoint with the fname body. The
// argument and CSRF checks are plain errors, matching the Python ValueError
// surface before any request is built.
func (c *Client) Rename(fileID string, name string) (model.RemoteEntry, error) {
	if fileID == "" {
		return model.RemoteEntry{}, errors.New("file_id is required")
	}
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return model.RemoteEntry{}, errors.New("name must be one remote entry name")
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
		keys:   []string{"fname", "csrfmiddlewaretoken"},
		values: map[string]any{"fname": name, "csrfmiddlewaretoken": current.CSRFToken},
	}
	encoded, err := dumpPYValue(body)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path: c.drivePath("/3rd/drive/api/v3/groups/" + quotePathSegment(groupID) +
			"/files/" + quotePathSegment(fileID)),
		Method:     http.MethodPut,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return model.RemoteEntry{}, model.NewWpsAPIError("rename file", 0, model.WpsCategoryUpstream)
	}
	return entryFromItem(payload)
}

// Move mirrors move: the confirmed v5 batch task endpoint, waiting for the
// observed task to finish before returning. The destination_group_id and
// option keywords stay at their Python defaults on this surface (same
// group, empty option dict).
func (c *Client) Move(fileID string, sourceParentID string, destinationParentID string) error {
	if fileID == "" {
		return errors.New("file_id is required")
	}
	if sourceParentID == "" || destinationParentID == "" {
		return errors.New("source and destination parent IDs are required")
	}
	current, err := c.currentCredentials()
	if err != nil {
		return err
	}
	if current.CSRFToken == "" {
		return errors.New("csrf_token is required for write operation")
	}
	groupID, err := c.GroupID()
	if err != nil {
		return err
	}
	if c.personal() {
		body := &pyObject{
			keys: []string{"fileids", "target_groupid", "target_parentid"},
			values: map[string]any{
				"fileids":         []any{pyJSONID(fileID)},
				"target_groupid":  pyJSONID(groupID),
				"target_parentid": pyJSONID(destinationParentID),
			},
		}
		encoded, err := dumpPYValue(body)
		if err != nil {
			return err
		}
		payload, err := c.RequestJSON(JSONRequest{
			Path:       c.drivePath("/api/v3/groups/" + quotePathSegment(groupID) + "/files/batch/move"),
			Method:     http.MethodPost,
			Body:       encoded,
			RetryOn401: true,
		})
		if err != nil {
			return err
		}
		if result, present := payload["result"]; present && result != nil && result != "ok" {
			return model.NewWpsAPIError("move file", 0, model.WpsCategoryUpstream)
		}
		return nil
	}
	body := &pyObject{
		keys: []string{"groupid", "parentid", "dst_groupid", "dst_parentid", "fileids", "option", "csrfmiddlewaretoken"},
		values: map[string]any{
			"groupid":             pyJSONID(groupID),
			"parentid":            pyJSONID(sourceParentID),
			"dst_groupid":         pyJSONID(groupID),
			"dst_parentid":        pyJSONID(destinationParentID),
			"fileids":             []any{pyJSONID(fileID)},
			"option":              &pyObject{},
			"csrfmiddlewaretoken": current.CSRFToken,
		},
	}
	encoded, err := dumpPYValue(body)
	if err != nil {
		return err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       c.drivePath("/3rd/drive/api/v5/files/batch/task/move"),
		Method:     http.MethodPost,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return err
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return model.NewWpsAPIError("move file", 0, model.WpsCategoryUpstream)
	}
	taskUUID, isString := payload["taskuuid"].(string)
	if taskUUID == "" || !isString {
		return model.NewWpsAPIError("move file task", 0, model.WpsCategoryUpstream)
	}
	return c.WaitForTask(context.Background(), taskUUID, "move file",
		DefaultTaskPollInterval, DefaultTaskPollTimeout)
}

// Copy mirrors copy: the confirmed same-group v3 batch copy endpoint. The
// captured response carries exactly one copied file ID; strings and JSON
// integers both pass Python's isinstance gate, everything else is invalid.
func (c *Client) Copy(fileID string, targetParentID string) (string, error) {
	if fileID == "" || targetParentID == "" {
		return "", errors.New("file and target parent IDs are required")
	}
	current, err := c.currentCredentials()
	if err != nil {
		return "", err
	}
	if current.CSRFToken == "" {
		return "", errors.New("csrf_token is required for write operation")
	}
	groupID, err := c.GroupID()
	if err != nil {
		return "", err
	}
	body := &pyObject{
		keys: []string{"fileids", "groupid", "target_groupid", "target_parentid", "duplicated_name_model", "csrfmiddlewaretoken"},
		values: map[string]any{
			"fileids":               []any{pyJSONID(fileID)},
			"groupid":               pyJSONID(groupID),
			"target_groupid":        pyJSONID(groupID),
			"target_parentid":       pyJSONID(targetParentID),
			"duplicated_name_model": pyInt(1),
			"csrfmiddlewaretoken":   current.CSRFToken,
		},
	}
	encoded, err := dumpPYValue(body)
	if err != nil {
		return "", err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       c.drivePath("/3rd/drive/api/v3/groups/" + quotePathSegment(groupID) + "/files/batch/copy"),
		Method:     http.MethodPost,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return "", err
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return "", model.NewWpsAPIError("copy file", 0, model.WpsCategoryUpstream)
	}
	fileids, isList := payload["fileids"].([]any)
	if !isList || len(fileids) != 1 {
		return "", model.NewWpsAPIError("copy response missing file ID", 0, model.WpsCategoryUpstream)
	}
	copiedID, valid := pyCopiedFileID(fileids[0])
	if !valid {
		return "", model.NewWpsAPIError("copy response contains invalid file ID", 0, model.WpsCategoryUpstream)
	}
	return copiedID, nil
}

// pyCopiedFileID mirrors str(fileids[0]) behind Python's isinstance gate:
// strings pass through unchanged, JSON integer literals normalize through
// str(int(...)) ("−0" becomes "0"), and bool/float/null/containers are
// rejected exactly like the reference (bool first, then str|int).
func pyCopiedFileID(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		text := string(typed)
		if !pyJSONIntLiteral(text) {
			return "", false
		}
		if text == "-0" {
			return "0", true
		}
		return text, true
	}
	return "", false
}

// pyJSONIntLiteral reports whether the JSON number is an integer literal
// (-?(0|[1-9][0-9]*)). Python's json module yields int for exactly these;
// floats and exponent forms never pass the isinstance(int) gate.
func pyJSONIntLiteral(text string) bool {
	if text == "" {
		return false
	}
	index := 0
	if text[0] == '-' {
		index = 1
	}
	if index >= len(text) {
		return false
	}
	if text[index] == '0' {
		return index+1 == len(text)
	}
	for position := index; position < len(text); position++ {
		if text[position] < '0' || text[position] > '9' {
			return false
		}
	}
	return true
}

// Delete mirrors delete: the confirmed v5 batch task endpoint, waiting for
// the observed task to finish before returning.
func (c *Client) Delete(fileID string) error {
	if fileID == "" {
		return errors.New("file_id is required")
	}
	current, err := c.currentCredentials()
	if err != nil {
		return err
	}
	if current.CSRFToken == "" {
		return errors.New("csrf_token is required for write operation")
	}
	groupID, err := c.GroupID()
	if err != nil {
		return err
	}
	if c.personal() {
		body := &pyObject{
			keys: []string{"fileids", "groupid", "csrfmiddlewaretoken"},
			values: map[string]any{
				"fileids":             []any{pyJSONID(fileID)},
				"groupid":             pyJSONID(groupID),
				"csrfmiddlewaretoken": current.CSRFToken,
			},
		}
		encoded, err := dumpPYValue(body)
		if err != nil {
			return err
		}
		payload, err := c.RequestJSON(JSONRequest{
			Path:       c.drivePath("/api/v3/groups/" + quotePathSegment(groupID) + "/files/batch/delete"),
			Method:     http.MethodPost,
			Body:       encoded,
			RetryOn401: true,
		})
		if err != nil {
			return err
		}
		if result, present := payload["result"]; present && result != nil && result != "ok" {
			return model.NewWpsAPIError("delete file", 0, model.WpsCategoryUpstream)
		}
		return nil
	}
	body := &pyObject{
		keys: []string{"fileids", "groupid", "csrfmiddlewaretoken"},
		values: map[string]any{
			"fileids":             []any{pyJSONID(fileID)},
			"groupid":             pyJSONID(groupID),
			"csrfmiddlewaretoken": current.CSRFToken,
		},
	}
	encoded, err := dumpPYValue(body)
	if err != nil {
		return err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       c.drivePath("/3rd/drive/api/v5/files/batch/task/delete"),
		Method:     http.MethodPost,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return err
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return model.NewWpsAPIError("delete file", 0, model.WpsCategoryUpstream)
	}
	taskUUID, isString := payload["taskuuid"].(string)
	if taskUUID == "" || !isString {
		return model.NewWpsAPIError("delete file task", 0, model.WpsCategoryUpstream)
	}
	return c.WaitForTask(context.Background(), taskUUID, "delete file",
		DefaultTaskPollInterval, DefaultTaskPollTimeout)
}
