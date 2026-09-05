// Remote entry normalization ports client.py's _entry_from_item: one
// malformed field rejects the item with an operation-only error, while an
// unknown kind silently normalizes and never breaks a whole page.

package wps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

// pyBool mirrors the _bool helper: WPS query booleans are lowercase.
func pyBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// ListOptions mirrors the list_entries keyword arguments. A zero Count,
// Orderby, or Order falls back to the Python signature defaults (20,
// "mtime", "desc"); a non-empty GroupID overrides the configured group.
type ListOptions struct {
	GroupID string
	Offset  int
	Count   int
	Orderby string
	Order   string

	LinkGroup           *bool
	Include             *string
	WithLink            *bool
	ReviewPicThumbnail  *bool
	WithSharefolderType *bool
	NextFilter          *string
}

// ListEntries mirrors list_entries: the captured v5 endpoint shape with the
// exact query order, parsed into a redacted page. The 401 retry follows the
// Python default and stays enabled.
func (c *Client) ListEntries(parentID string, options ListOptions) (model.ListPage, error) {
	return c.listEntries(parentID, options, true)
}

func (c *Client) listEntries(parentID string, options ListOptions, retryOn401 bool) (model.ListPage, error) {
	groupID := options.GroupID
	if groupID == "" {
		resolved, err := c.GroupID()
		if err != nil {
			return model.ListPage{}, err
		}
		groupID = resolved
	}
	count := options.Count
	if count <= 0 {
		count = 20
	}
	orderby := options.Orderby
	if orderby == "" {
		orderby = "mtime"
	}
	order := options.Order
	if order == "" {
		order = "desc"
	}
	query := []QueryPair{
		{Key: "parentid", Value: parentID},
		{Key: "offset", Value: strconv.Itoa(options.Offset)},
		{Key: "count", Value: strconv.Itoa(count)},
		{Key: "orderby", Value: orderby},
		{Key: "order", Value: order},
	}
	if options.LinkGroup != nil {
		query = append(query, QueryPair{Key: "linkgroup", Value: pyBool(*options.LinkGroup)})
	}
	if options.Include != nil {
		query = append(query, QueryPair{Key: "include", Value: *options.Include})
	}
	if options.WithLink != nil {
		query = append(query, QueryPair{Key: "with_link", Value: pyBool(*options.WithLink)})
	}
	if options.ReviewPicThumbnail != nil {
		query = append(query, QueryPair{Key: "review_pic_thumbnail", Value: pyBool(*options.ReviewPicThumbnail)})
	}
	if options.WithSharefolderType != nil {
		query = append(query, QueryPair{Key: "with_sharefolder_type", Value: pyBool(*options.WithSharefolderType)})
	}
	if options.NextFilter != nil {
		query = append(query, QueryPair{Key: "next_filter", Value: *options.NextFilter})
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/groups/" + quotePathSegment(groupID) + "/files",
		Query:      query,
		RetryOn401: retryOn401,
	})
	if err != nil {
		return model.ListPage{}, err
	}
	return parseListPage(payload)
}

// parseListPage mirrors the list_entries response rules: files must be a
// list (missing counts as empty), items without an id are skipped, one
// malformed name fails the page, and result must be absent/null or "ok".
func parseListPage(payload map[string]any) (model.ListPage, error) {
	var items []any
	if raw, present := payload["files"]; present {
		list, isList := raw.([]any)
		if !isList {
			return model.ListPage{}, model.NewWpsAPIError("list files", 0, model.WpsCategoryUpstream)
		}
		items = list
	}
	entries := []model.RemoteEntry{}
	for _, itemValue := range items {
		item, isObject := itemValue.(map[string]any)
		if !isObject {
			continue
		}
		if rawID, present := item["id"]; !present || rawID == nil {
			continue
		}
		entry, err := entryFromItem(item)
		if err != nil {
			return model.ListPage{}, err
		}
		entries = append(entries, entry)
	}
	var nextOffset *int
	if raw, present := payload["next_offset"]; present {
		if number, isNumber := raw.(json.Number); isNumber {
			if parsed, err := number.Int64(); err == nil {
				nextOffset = model.Ptr(int(parsed))
			}
		}
	}
	var nextFilter *string
	if raw, present := payload["next_filter"]; present {
		if text, isString := raw.(string); isString {
			nextFilter = model.Ptr(text)
		}
	}
	var result *string
	if raw, present := payload["result"]; present {
		if text, isString := raw.(string); isString {
			result = model.Ptr(text)
		}
	}
	if result != nil && *result != "ok" {
		return model.ListPage{}, model.NewWpsAPIError("list files", 0, model.WpsCategoryUpstream)
	}
	return model.ListPage{
		Entries:    entries,
		NextOffset: nextOffset,
		NextFilter: nextFilter,
		Result:     result,
	}, nil
}

// cursorKey identifies one pagination cursor, the Python tuple
// (next_offset, next_filter).
type cursorKey struct {
	offset int
	filter *string
}

// IterOptions mirrors the iter_entries keyword arguments. Count is required
// exactly like the Python argument check; MaxEntries nil means no entry
// ceiling (the page limit still applies).
type IterOptions struct {
	Count      int
	MaxEntries *int
	Orderby    string
	Order      string

	LinkGroup           *bool
	Include             *string
	WithLink            *bool
	ReviewPicThumbnail  *bool
	WithSharefolderType *bool
}

// IterEntries mirrors iter_entries: fetch every page while deduplicating
// WPS's natural page-boundary overlaps by entry id, stopping on a repeated
// or non-advancing cursor, and mapping exhausted pagination to the
// insufficient-storage error instead of a truncated success.
func (c *Client) IterEntries(parentID string, options IterOptions) ([]model.RemoteEntry, error) {
	if options.Count <= 0 {
		return nil, errors.New("count must be positive")
	}
	if options.MaxEntries != nil && *options.MaxEntries <= 0 {
		return nil, errors.New("max_entries must be positive")
	}
	pageLimit := 10000
	if options.MaxEntries != nil {
		pageLimit = *options.MaxEntries + 1
	}
	entries := []model.RemoteEntry{}
	seenEntryIDs := map[string]struct{}{}
	offset := 0
	var pageFilter *string
	seenCursors := map[cursorKey]struct{}{}
	pageCount := 0
	filtersEqual := func(a *string, b *string) bool {
		if a == nil || b == nil {
			return a == b
		}
		return *a == *b
	}
	for {
		pageCount++
		if pageCount > pageLimit {
			return nil, model.NewStorageError(
				model.KindInsufficientStorage,
				"remote folder pagination exceeds the configured limit",
			)
		}
		page, err := c.listEntries(parentID, ListOptions{
			Offset:              offset,
			Count:               options.Count,
			Orderby:             options.Orderby,
			Order:               options.Order,
			LinkGroup:           options.LinkGroup,
			Include:             options.Include,
			WithLink:            options.WithLink,
			ReviewPicThumbnail:  options.ReviewPicThumbnail,
			WithSharefolderType: options.WithSharefolderType,
			NextFilter:          pageFilter,
		}, true)
		if err != nil {
			return nil, err
		}
		fresh := []model.RemoteEntry{}
		for _, entry := range page.Entries {
			if _, seen := seenEntryIDs[entry.ID]; seen {
				continue
			}
			fresh = append(fresh, entry)
		}
		if options.MaxEntries != nil && len(entries)+len(fresh) > *options.MaxEntries {
			return nil, model.NewStorageError(
				model.KindInsufficientStorage,
				"remote folder exceeds the configured entry limit",
			)
		}
		for _, entry := range fresh {
			seenEntryIDs[entry.ID] = struct{}{}
			entries = append(entries, entry)
		}
		if page.NextOffset == nil || *page.NextOffset < 0 {
			return entries, nil
		}
		nextCursor := cursorKey{offset: *page.NextOffset, filter: page.NextFilter}
		if _, seen := seenCursors[nextCursor]; seen {
			return entries, nil
		}
		if *page.NextOffset < offset ||
			(*page.NextOffset == offset && filtersEqual(page.NextFilter, pageFilter)) {
			return entries, nil
		}
		seenCursors[nextCursor] = struct{}{}
		offset = *page.NextOffset
		pageFilter = page.NextFilter
	}
}
