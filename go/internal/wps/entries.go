// Remote entry normalization ports client.py's _entry_from_item: one
// malformed field rejects the item with an operation-only error, while an
// unknown kind silently normalizes and never breaks a whole page.

package wps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// pyStr mirrors Python str() for the JSON value classes WPS sends: numbers
// keep their raw token and booleans capitalize like Python's repr.
func pyStr(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		if typed {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// pyTruthy mirrors Python truthiness for the JSON value classes.
func pyTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return true
		}
		return parsed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	}
	return true
}

// entryFromItem mirrors _entry_from_item. The id must exist, the name must
// be a safe bounded string, and size/etag degrade to null on malformed
// values instead of failing the entry.
func entryFromItem(item map[string]any) (model.RemoteEntry, error) {
	const operation = "normalize file metadata"
	rawID, present := item["id"]
	if !present || rawID == nil {
		return model.RemoteEntry{}, model.NewWpsAPIError(operation, 0, model.WpsCategoryUpstream)
	}
	name, isString := item["fname"].(string)
	if !isString || name == "" || name == "." || name == ".." ||
		strings.Contains(name, "/") ||
		strings.Contains(name, "\\") ||
		strings.Contains(name, "\x00") ||
		hasControlChars(name) ||
		len(name) > MaxRemoteNameBytes {
		return model.RemoteEntry{}, model.NewWpsAPIError(operation, 0, model.WpsCategoryUpstream)
	}
	kind := model.KindUnknown
	if rawKind, present := item["ftype"]; present {
		if text, isString := rawKind.(string); isString {
			switch text {
			case "file":
				kind = model.KindFile
			case "folder":
				kind = model.KindFolder
			}
		}
	}
	var size *int64
	if rawSize, present := item["fsize"]; present {
		if number, isNumber := rawSize.(json.Number); isNumber {
			if parsed, err := number.Int64(); err == nil && parsed >= 0 {
				size = model.Ptr(parsed)
			}
		}
	}
	var modifiedAt *string
	if rawModified, present := item["mtime"]; present && rawModified != nil {
		modifiedAt = model.Ptr(pyStr(rawModified))
	}
	var etag *string
	if rawEtag, present := item["fsha"]; present {
		if text, isString := rawEtag.(string); isString &&
			len(text) <= MaxRemoteEtagBytes &&
			!hasControlChars(text) {
			etag = model.Ptr(text)
		}
	}
	var parentID *string
	if rawParent, present := item["parentid"]; present && rawParent != nil {
		parentID = model.Ptr(pyStr(rawParent))
	}
	var linkID *string
	if rawLink, present := item["link_id"]; present && pyTruthy(rawLink) {
		linkID = model.Ptr(pyStr(rawLink))
	}
	return model.RemoteEntry{
		ID:         pyStr(rawID),
		Name:       name,
		Kind:       kind,
		ParentID:   parentID,
		Size:       size,
		ModifiedAt: modifiedAt,
		Etag:       etag,
		LinkID:     linkID,
		Raw:        item,
	}, nil
}
