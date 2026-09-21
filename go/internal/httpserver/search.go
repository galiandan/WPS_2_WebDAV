package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/mimetypes"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

// SearchLimits bound the one process-local metadata index. Searches never
// download file contents. A refresh replaces the previous snapshot and follows
// only the folders exposed by the normal browser storage view.
type SearchLimits struct {
	MaxEntries  int
	MaxFolders  int
	MaxDepth    int
	MaxBytes    int
	MaxDuration time.Duration
	Interval    time.Duration
}

func defaultSearchLimits() SearchLimits {
	return SearchLimits{MaxEntries: 50000, MaxFolders: 5000, MaxDepth: 64,
		MaxBytes: 32 << 20, MaxDuration: 10 * time.Minute, Interval: 100 * time.Millisecond}
}

// SearchIndexStatus describes coverage separately from pagination. A partial
// scan can have zero matches without proving that the requested file is absent.
type SearchIndexStatus struct {
	State          string `json:"state"`
	Path           string `json:"path"`
	Generation     uint64 `json:"generation"`
	Entries        int    `json:"entries"`
	ScannedFolders int    `json:"scanned_folders"`
	SkippedFolders int    `json:"skipped_folders"`
	Complete       bool   `json:"complete"`
	Reason         string `json:"reason,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

type SearchResult struct {
	Path  string            `json:"path"`
	Entry model.PublicEntry `json:"entry"`
}

type searchResponse struct {
	Query        string            `json:"query"`
	Path         string            `json:"path"`
	Type         string            `json:"type"`
	Match        string            `json:"match"`
	Results      []SearchResult    `json:"results"`
	Total        int               `json:"total"`
	Offset       int               `json:"offset"`
	Limit        int               `json:"limit"`
	HasMore      bool              `json:"has_more"`
	ScopeCovered bool              `json:"scope_covered"`
	Index        SearchIndexStatus `json:"index"`
}

type searchQuery struct {
	text, path, kind, match string
	offset, limit           int
}

// SearchIndex holds one bounded in-memory snapshot and one worker at most.
// Legacy listing methods have their own request deadlines but no context
// parameter; cancellation stops traversal immediately after that one in-flight
// listing returns. We deliberately do not spawn an unbounded goroutine for each
// listing just to make cancellation appear instantaneous.
type SearchIndex struct {
	read           RESTReadStorage
	limits         SearchLimits
	mu             sync.Mutex
	status         SearchIndexStatus
	items          []SearchResult
	active         bool
	cancel         context.CancelFunc
	identitySource func() ([32]byte, error)
	identity       [32]byte
	identitySet    bool
}

func newSearchIndex(read RESTReadStorage, limits SearchLimits) *SearchIndex {
	return &SearchIndex{read: read, limits: limits, status: SearchIndexStatus{State: "idle", Path: "/"}}
}

var errSearchBusy = errors.New("a search index refresh is already running")

func (s *SearchIndex) start(root string) (SearchIndexStatus, error) {
	if err := s.refreshIdentity(); err != nil {
		return SearchIndexStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return s.status, errSearchBusy
	}
	s.invalidateMetadataCache()
	ctx, cancel := context.WithTimeout(context.Background(), s.limits.MaxDuration)
	s.cancel, s.active, s.items = cancel, true, nil
	s.status = SearchIndexStatus{State: "indexing", Path: root,
		Generation: s.status.Generation + 1, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	go s.build(ctx, s.status.Generation, root)
	return s.status, nil
}

func (s *SearchIndex) stop() SearchIndexStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		s.cancel()
		s.status.State, s.status.Complete = "cancelling", false
	}
	return s.status
}

// invalidate also discards old account/root metadata. The generation check
// prevents an in-flight listing from repopulating the discarded snapshot.
func (s *SearchIndex) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateLocked()
}

func (s *SearchIndex) invalidateLocked() {
	state := "idle"
	if s.active {
		s.cancel()
		state = "cancelling"
	}
	s.items = nil
	s.status = SearchIndexStatus{State: state, Path: "/", Generation: s.status.Generation + 1}
}

func (s *SearchIndex) refreshIdentity() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.identitySource == nil {
		return nil
	}
	identity, err := s.identitySource()
	if err != nil {
		s.invalidateLocked()
		s.invalidateMetadataCache()
		return model.NewStorageError(model.KindUnsupportedOperation, "search storage identity is unavailable")
	}
	if s.identitySet && identity != s.identity {
		s.invalidateLocked()
		s.invalidateMetadataCache()
	}
	s.identity, s.identitySet = identity, true
	return nil
}

func (s *SearchIndex) invalidateMetadataCache() {
	if cache, ok := s.read.(interface{ InvalidateMetadataCache() }); ok {
		cache.InvalidateMetadataCache()
	}
}

func (s *SearchIndex) build(ctx context.Context, generation uint64, root string) {
	state, reason := "ready", ""
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		contextErr := ctx.Err()
		s.cancel()
		s.active = false
		if s.status.Generation != generation {
			if s.status.State == "cancelling" {
				s.status.State = "idle"
			}
			return
		}
		if contextErr != nil {
			state, reason = "cancelled", "cancelled"
			if errors.Is(contextErr, context.DeadlineExceeded) {
				state, reason = "partial", "time_limit"
			}
		}
		s.status.State, s.status.Reason = state, reason
		s.status.Complete = state == "ready"
		s.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}()

	if s.refreshIdentity() != nil || ctx.Err() != nil {
		return
	}
	rootEntry, err := s.read.Metadata(root)
	if err != nil || rootEntry.Kind != model.KindFolder {
		state, reason = "failed", "root_unavailable"
		return
	}
	type folder struct {
		path, namespace string
		entry           model.RemoteEntry
		depth           int
	}
	queue := []folder{{path: root, namespace: root, entry: rootEntry}}
	visited := map[string]bool{root + "\x00" + rootEntry.ID: true}
	metadataBytes := 0
	for len(queue) > 0 {
		if s.refreshIdentity() != nil || ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		count := s.status.ScannedFolders
		s.mu.Unlock()
		if count >= s.limits.MaxFolders {
			state, reason = "partial", "folder_limit"
			return
		}
		if count > 0 && s.limits.Interval > 0 {
			timer := time.NewTimer(s.limits.Interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		current := queue[0]
		queue[0] = folder{}
		queue = queue[1:]
		var children []model.RemoteEntry
		if current.path == "/" {
			children, err = s.read.ListPath(current.path)
		} else {
			children, err = s.read.ListChildren(current.path, current.entry)
		}
		if s.refreshIdentity() != nil || ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		if s.status.Generation != generation {
			s.mu.Unlock()
			return
		}
		s.status.ScannedFolders++
		if err != nil {
			s.status.SkippedFolders++
		}
		s.mu.Unlock()
		if err != nil {
			// Other spaces may still be readable. Never echo upstream error
			// text, which can include request details or signed URLs.
			state, reason = "partial", "listing_failed"
			continue
		}
		// Keep stable ordering within each folder without mutating cached
		// storage slices, which may be shared with other requests.
		children = append([]model.RemoteEntry(nil), children...)
		sort.SliceStable(children, func(i, j int) bool { return children[i].Name < children[j].Name })
		for n, entry := range children {
			if ctx.Err() != nil {
				return
			}
			if entry.Kind != model.KindFolder && entry.Kind != model.KindFile {
				state, reason = "partial", "invalid_entry"
				continue
			}
			if (n > 0 && children[n-1].Name == entry.Name) || (n+1 < len(children) && children[n+1].Name == entry.Name) {
				state, reason = "partial", "ambiguous_path"
				continue
			}
			childPath, pathErr := storage.JoinRemotePath([]string{entry.Name}, false)
			if pathErr != nil || !utf8.ValidString(entry.Name) {
				state, reason = "partial", "invalid_entry"
				continue
			}
			childPath = strings.TrimSuffix(current.path, "/") + childPath
			item := SearchResult{Path: childPath, Entry: entry.Public()}
			encoded, encodeErr := json.Marshal(item)
			if encodeErr != nil {
				state, reason = "partial", "invalid_entry"
				continue
			}
			if metadataBytes+len(encoded) > s.limits.MaxBytes {
				state, reason = "partial", "metadata_limit"
				return
			}
			s.mu.Lock()
			if s.status.Generation != generation {
				s.mu.Unlock()
				return
			}
			if len(s.items) >= s.limits.MaxEntries {
				s.mu.Unlock()
				state, reason = "partial", "entry_limit"
				return
			}
			s.items = append(s.items, item)
			s.status.Entries = len(s.items)
			s.mu.Unlock()
			metadataBytes += len(encoded)
			if entry.Kind != model.KindFolder {
				continue
			}
			if current.depth+1 >= s.limits.MaxDepth {
				state, reason = "partial", "depth_limit"
				continue
			}
			namespace := current.namespace
			if strings.HasPrefix(entry.ID, "space:") {
				namespace = entry.ID
			}
			identity := namespace + "\x00" + entry.ID
			if entry.ID == "" || visited[identity] {
				state, reason = "partial", "repeated_folder"
				continue
			}
			visited[identity] = true
			queue = append(queue, folder{path: childPath, namespace: namespace, entry: entry, depth: current.depth + 1})
		}
	}
}

func searchContainsPath(root, candidate string) bool {
	return root == "/" || candidate == root || strings.HasPrefix(candidate, root+"/")
}

func (s *SearchIndex) query(query searchQuery) searchResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	response := searchResponse{Query: query.text, Path: query.path, Type: query.kind, Match: query.match,
		Results: make([]SearchResult, 0), Offset: query.offset, Limit: query.limit, Index: s.status}
	response.ScopeCovered = s.status.State != "idle" && searchContainsPath(s.status.Path, query.path)
	if !response.ScopeCovered {
		return response
	}
	needle := strings.ToLower(query.text)
	for _, item := range s.items {
		if !searchContainsPath(query.path, item.Path) || !searchMatchesType(item.Entry, query.kind) {
			continue
		}
		haystack := item.Entry.Name
		if query.match == "path" {
			haystack = item.Path
		}
		if !strings.Contains(strings.ToLower(haystack), needle) {
			continue
		}
		if response.Total >= query.offset && len(response.Results) < query.limit {
			response.Results = append(response.Results, item)
		}
		response.Total++
	}
	response.HasMore = response.Total > response.Offset+len(response.Results)
	return response
}

func searchMatchesType(entry model.PublicEntry, kind string) bool {
	switch kind {
	case "all":
		return true
	case "folder":
		return entry.Kind == model.KindFolder
	case "file":
		return entry.Kind == model.KindFile
	}
	if entry.Kind != model.KindFile {
		return false
	}
	typ := mimetypes.GuessMimeType(entry.Name)
	if kind == "document" {
		if strings.HasPrefix(typ, "text/") || typ == "application/pdf" {
			return true
		}
		switch strings.ToLower(path.Ext(entry.Name)) {
		case ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".wps", ".et", ".dps", ".odt", ".ods", ".odp", ".rtf", ".md", ".json", ".yaml", ".yml":
			return true
		}
		return false
	}
	return strings.HasPrefix(typ, kind+"/")
}

func parseSearchQuery(query url.Values) (searchQuery, error) {
	parsed := searchQuery{path: "/", kind: "all", match: "name", limit: 100}
	for _, name := range []string{"q", "path", "type", "match", "offset", "limit"} {
		if values, ok := query[name]; ok && len(values) != 1 {
			return parsed, errBadRequest("search query parameters must occur once")
		}
	}
	var err error
	parsed.path, err = queryPath(query)
	if err != nil {
		return parsed, err
	}
	parts, err := storage.SplitRemotePath(parsed.path)
	if err != nil {
		return parsed, err
	}
	parsed.path, _ = storage.JoinRemotePath(parts, false)
	parsed.text = strings.TrimSpace(query.Get("q"))
	if !utf8.ValidString(parsed.text) || len(parsed.text) > 256 || strings.ContainsAny(parsed.text, "\x00\r\n") {
		return parsed, errBadRequest("search query must be valid text of at most 256 bytes")
	}
	if value := query.Get("type"); value != "" {
		parsed.kind = value
	}
	switch parsed.kind {
	case "all", "file", "folder", "image", "video", "audio", "document":
	default:
		return parsed, errBadRequest("unsupported search type")
	}
	if value := query.Get("match"); value != "" {
		parsed.match = value
	}
	if parsed.match != "name" && parsed.match != "path" {
		return parsed, errBadRequest("search match must be name or path")
	}
	for name, destination := range map[string]*int{"offset": &parsed.offset, "limit": &parsed.limit} {
		if value, present := query[name]; present {
			*destination, err = strconv.Atoi(value[0])
			if err != nil || *destination < 0 || *destination > 50000 {
				return parsed, errBadRequest("invalid search pagination")
			}
		}
	}
	if parsed.limit < 1 || parsed.limit > 200 {
		return parsed, errBadRequest("search limit must be between 1 and 200")
	}
	return parsed, nil
}

func (d *RESTDispatcher) searchIndex() *SearchIndex {
	d.searchOnce.Do(func() { d.search = newSearchIndex(d.read, defaultSearchLimits()) })
	return d.search
}

func (d *RESTDispatcher) invalidateSearch() { d.searchIndex().invalidate() }

// InvalidateSearch connects mutations through other protocol dispatchers to
// the same process-local filename snapshot.
func (d *RESTDispatcher) InvalidateSearch() { d.invalidateSearch() }

// SetSearchIdentity installs a local-only fingerprint source before serving
// requests. It catches credential/workspace replacement by the desktop helper
// in addition to normal REST mutations. Fingerprints never leave this process.
func (d *RESTDispatcher) SetSearchIdentity(source func() ([32]byte, error)) {
	index := d.searchIndex()
	index.mu.Lock()
	defer index.mu.Unlock()
	index.identitySource = source
}

// CancelSearch stops accepting more listing work during server shutdown.
func (d *RESTDispatcher) CancelSearch() { d.searchIndex().stop() }

func (d *RESTDispatcher) doSearch(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	query, err := parseSearchQuery(route.Query)
	if err != nil {
		return err
	}
	if err := d.searchIndex().refreshIdentity(); err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, d.searchIndex().query(query), d.limits, nil)
}

func (d *RESTDispatcher) doSearchRefresh(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	query, err := parseSearchQuery(route.Query)
	if err != nil {
		return err
	}
	status, err := d.searchIndex().start(query.path)
	code := http.StatusAccepted
	if errors.Is(err, errSearchBusy) {
		code = http.StatusConflict
	} else if err != nil {
		return err
	}
	return sendJSON(w, r, code, map[string]any{"index": status}, d.limits, nil)
}

func (d *RESTDispatcher) doSearchCancel(w http.ResponseWriter, r *http.Request) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, map[string]any{"index": d.searchIndex().stop()}, d.limits, nil)
}
