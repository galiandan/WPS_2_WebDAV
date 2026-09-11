// Package workspace resolves configured or login-selected group and root
// IDs from the workspace state file.
//
// WorkspaceState owns configured and login-selected group/root IDs —
// old {group_id,root_id} and new spaces schemas, hot reload on mtime
// change, atomic persist with Python-compatible ensure_ascii JSON, and the
// pending-login state when the file is absent.
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
)

// AutoValue keeps a setting on automatic resolution from the state file.
const AutoValue = "auto"

const (
	ModeAuto     = "auto"
	ModeBusiness = "business"
	ModePersonal = "personal"
)

// MaxSpaces mirrors MAX_WORKSPACE_SPACES.
const MaxSpaces = 128

// MaxFileBytes mirrors MAX_WORKSPACE_FILE_BYTES.
const MaxFileBytes = 16 * 1024

// DefaultFile mirrors DEFAULT_WORKSPACE_FILE.
const DefaultFile = "/opt/wps-adapter/config/secrets/wps-workspace.json"

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)

// ConfigError marks an invalid workspace configuration; the text never
// includes file contents or configured values.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

func configErrorf(format string, args ...any) error {
	return &ConfigError{Msg: fmt.Sprintf(format, args...)}
}

// ValidateIdentifier applies the shared WPS identifier rule.
func ValidateIdentifier(value string, fieldName string) error {
	if !identifierPattern.MatchString(value) {
		return configErrorf("%s is invalid", fieldName)
	}
	return nil
}

// Mount describes one WPS space below the virtual root.
type Mount struct {
	GroupID string
	RootID  string
	Name    string
	// Path is the human-readable folder path selected inside the WPS space.
	// The ID remains the routing source of truth; Path is persisted so the
	// web UI can explain the current mapping after a restart.
	Path string
}

// NewMount validates and builds one mount, mirroring WorkspaceMount's
// construction checks. Control characters in names are rejected per the
// owner decision on D-07.
func NewMount(groupID string, rootID string, name string) (Mount, error) {
	return NewMountWithPath(groupID, rootID, name, "/")
}

// NewMountWithPath validates and builds a mounted WPS space with its
// optional human-readable folder path.
func NewMountWithPath(groupID string, rootID string, name string, path string) (Mount, error) {
	if err := ValidateIdentifier(groupID, "space.group_id"); err != nil {
		return Mount{}, err
	}
	if err := ValidateIdentifier(rootID, "space.root_id"); err != nil {
		return Mount{}, err
	}
	if err := validateMountName(name); err != nil {
		return Mount{}, err
	}
	if path == "" {
		path = "/"
	}
	if err := ValidateSelectionPath(path, "space.path"); err != nil {
		return Mount{}, err
	}
	return Mount{GroupID: groupID, RootID: rootID, Name: name, Path: path}, nil
}

// ValidateSelectionPath validates a persisted WPS folder path. It is a
// display and selection path, not a local filesystem path, but rejecting
// traversal and controls keeps it safe for the web API and logs.
func ValidateSelectionPath(path string, fieldName string) error {
	if path == "" || !strings.HasPrefix(path, "/") || strings.Contains(path, "\\") || strings.Contains(path, "\x00") {
		return configErrorf("%s is invalid", fieldName)
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7F {
			return configErrorf("%s is invalid", fieldName)
		}
	}
	if path != "/" {
		for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
			if part == "" || part == "." || part == ".." {
				return configErrorf("%s is invalid", fieldName)
			}
		}
	}
	if len([]rune(path)) > 4096 {
		return configErrorf("%s is too long", fieldName)
	}
	return nil
}

func validateMountName(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return configErrorf("space.name is invalid")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7F {
			return configErrorf("space.name is invalid")
		}
	}
	if len(name) > 4096 {
		return configErrorf("space.name is too long")
	}
	return nil
}

// State is a resolved workspace snapshot.
type State struct {
	GroupID  string
	RootID   string
	RootPath string
	Mode     string
	Spaces   []Mount
}

// WorkspaceState resolves the live workspace view, reloading the state file
// when its mtime changes. It mirrors Python's WorkspaceState.
type WorkspaceState struct {
	filePath          string
	configuredGroupID string
	configuredRootID  string

	mu       sync.Mutex
	groupID  string
	rootID   string
	rootPath string
	mode     string
	spaces   []Mount
	mtimeNs  *int64
}

// NewWorkspaceState mirrors WorkspaceState.__post_init__: validate the file
// path, the configured identifiers, then read the file once.
func NewWorkspaceState(filePath string, configuredGroupID string, configuredRootID string) (*WorkspaceState, error) {
	if filePath != "" {
		if err := securefile.ValidateStatePath(filePath); err != nil {
			return nil, translateReadErr(err)
		}
	}
	state := &WorkspaceState{
		filePath:          filePath,
		configuredGroupID: configuredGroupID,
		configuredRootID:  configuredRootID,
		mode:              ModeAuto,
	}
	var err error
	if state.configuredGroupID, err = validateConfigured(state.configuredGroupID, "WPS_GROUP_ID", true); err != nil {
		return nil, err
	}
	if state.configuredRootID == "" {
		state.configuredRootID = "0"
	}
	if state.configuredRootID, err = validateConfigured(state.configuredRootID, "WPS_ROOT_ID", false); err != nil {
		return nil, err
	}
	state.groupID = ""
	if state.configuredGroupID != "" && state.configuredGroupID != AutoValue {
		state.groupID = state.configuredGroupID
	}
	state.rootID = state.configuredRootID
	if state.configuredRootID == AutoValue {
		state.rootID = "0"
	}
	if err := state.refreshLocked(true); err != nil {
		return nil, err
	}
	return state, nil
}

// LoadFromFile builds one resolved snapshot, keeping the B201 config entry
// point stable.
func LoadFromFile(filePath string, configuredGroupID string, configuredRootID string) (*State, error) {
	if filePath == "" {
		return nil, configErrorf("workspace file path is invalid")
	}
	state, err := NewWorkspaceState(filePath, configuredGroupID, configuredRootID)
	if err != nil {
		return nil, err
	}
	groupID, err := state.GroupID()
	if err != nil {
		return nil, err
	}
	rootID, err := state.RootID()
	if err != nil {
		return nil, err
	}
	spaces, err := state.Spaces()
	if err != nil {
		return nil, err
	}
	mode, err := state.Mode()
	if err != nil {
		return nil, err
	}
	return &State{GroupID: groupID, RootID: rootID, Mode: mode, Spaces: spaces}, nil
}

func validateConfigured(value string, fieldName string, allowEmpty bool) (string, error) {
	if allowEmpty && value == "" {
		return value, nil
	}
	if value == AutoValue {
		return value, nil
	}
	if err := ValidateIdentifier(value, fieldName); err != nil {
		return "", err
	}
	return value, nil
}

// GroupID returns the resolved group, hot-reloading the file first.
func (s *WorkspaceState) GroupID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return "", err
	}
	return s.groupID, nil
}

// RootID returns the resolved root, hot-reloading the file first.
func (s *WorkspaceState) RootID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return "", err
	}
	return s.rootID, nil
}

// RootPath returns the selected folder path for the single-space view.
func (s *WorkspaceState) RootPath() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return "", err
	}
	if s.rootPath == "" {
		return "/", nil
	}
	return s.rootPath, nil
}

// Mode returns the detected WPS account family. Old workspace files resolve
// to auto and the application keeps its historical business default.
func (s *WorkspaceState) Mode() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return "", err
	}
	if s.mode == "" {
		return ModeAuto, nil
	}
	return s.mode, nil
}

// Spaces returns the named spaces written by the current login flow.
func (s *WorkspaceState) Spaces() ([]Mount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return nil, err
	}
	return s.spaces, nil
}

// Configured reports whether a group is resolved.
func (s *WorkspaceState) Configured() (bool, error) {
	groupID, err := s.GroupID()
	if err != nil {
		return false, err
	}
	return groupID != "", nil
}

// ConfiguredRootID returns the raw configured root identifier ("auto" or a
// fixed id), mirroring Python's workspace.configured_root_id attribute. It
// is fixed at construction and never reloaded.
func (s *WorkspaceState) ConfiguredRootID() string {
	return s.configuredRootID
}

// Update validates and persists a new selection, then adopts it in memory
// for every field still on auto. Like Python, configured (non-auto) values
// are never overwritten.
func (s *WorkspaceState) Update(groupID string, rootID string, spaces []Mount) error {
	return s.UpdateWithPaths(groupID, rootID, "", spaces)
}

// UpdateWithPaths persists a login or web selection, including the
// human-readable path for the selected root and each mounted space.
func (s *WorkspaceState) UpdateWithPaths(groupID string, rootID string, rootPath string, spaces []Mount) error {
	return s.UpdateWithPathsMode(groupID, rootID, rootPath, spaces, "")
}

// UpdateWithPathsMode persists an account family supplied by the login
// helper. An empty mode preserves the current value for web-only changes.
func (s *WorkspaceState) UpdateWithPathsMode(groupID string, rootID string, rootPath string, spaces []Mount, mode string) error {
	if err := ValidateIdentifier(groupID, "workspace.group_id"); err != nil {
		return err
	}
	if err := ValidateIdentifier(rootID, "workspace.root_id"); err != nil {
		return err
	}
	if rootPath == "" {
		rootPath = "/"
	}
	if err := ValidateSelectionPath(rootPath, "workspace.root_path"); err != nil {
		return err
	}
	if mode != "" && mode != ModeAuto && mode != ModeBusiness && mode != ModePersonal {
		return configErrorf("workspace.mode is invalid")
	}
	if len(spaces) > MaxSpaces {
		return configErrorf("too many workspace spaces")
	}
	normalized := make([]Mount, 0, len(spaces))
	seenGroups := make(map[string]struct{}, len(spaces))
	seenNames := make(map[string]struct{}, len(spaces))
	for _, mount := range spaces {
		validated, err := NewMountWithPath(mount.GroupID, mount.RootID, mount.Name, mount.Path)
		if err != nil {
			return err
		}
		if _, duplicate := seenGroups[validated.GroupID]; duplicate {
			return configErrorf("workspace spaces contain duplicate groups")
		}
		if _, duplicate := seenNames[validated.Name]; duplicate {
			return configErrorf("workspace spaces contain duplicate names")
		}
		seenGroups[validated.GroupID] = struct{}{}
		seenNames[validated.Name] = struct{}{}
		normalized = append(normalized, validated)
	}
	if len(normalized) == 0 {
		normalized = nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if mode == "" {
		mode = s.mode
	}
	if mode == "" {
		mode = ModeAuto
	}
	if s.filePath != "" {
		if err := s.persistLocked(groupID, rootID, rootPath, normalized, mode); err != nil {
			return err
		}
	}
	if s.configuredGroupID == "" || s.configuredGroupID == AutoValue {
		s.groupID = groupID
	}
	if s.configuredRootID == AutoValue {
		s.rootID = rootID
	}
	s.rootPath = rootPath
	s.mode = mode
	s.spaces = normalized
	return nil
}

func (s *WorkspaceState) persistLocked(groupID string, rootID string, rootPath string, spaces []Mount, mode string) error {
	mtimeNs, err := securefile.WriteAtomic(s.filePath, buildStatePayloadWithMode(groupID, rootID, rootPath, spaces, mode))
	if err != nil {
		return translateWriteErr(err)
	}
	s.mtimeNs = &mtimeNs
	return nil
}

// refreshLocked reloads the file when its mtime changed; parse failures
// leave the previously applied state and cached mtime untouched.
func (s *WorkspaceState) refreshLocked(force bool) error {
	if s.filePath == "" {
		return nil
	}
	var statMtime *int64
	info, err := os.Lstat(s.filePath)
	switch {
	case err == nil:
		ns := info.ModTime().UnixNano()
		statMtime = &ns
	case errors.Is(err, fs.ErrNotExist):
		// statMtime stays nil: the file is gone, mirroring the Python
		// FileNotFoundError branch.
	default:
		return configErrorf("stat workspace file failed")
	}
	if !force && mtimeEqual(statMtime, s.mtimeNs) {
		return nil
	}
	payload, readMtime, err := securefile.ReadJSONState(s.filePath, MaxFileBytes)
	if err != nil {
		return translateReadErr(err)
	}
	groupID, rootID, rootPath, mode, spaces, err := applyPayloadWithMode(s.configuredGroupID, s.configuredRootID, s.groupID, s.rootID, s.rootPath, s.mode, payload)
	if err != nil {
		return err
	}
	s.groupID = groupID
	s.rootID = rootID
	s.rootPath = rootPath
	s.mode = mode
	s.spaces = spaces
	s.mtimeNs = readMtime
	return nil
}

// applyPayload mirrors _apply_file_payload_locked: file values are adopted
// only for configured auto/empty fields, and everything is validated before
// any value is applied.
func applyPayload(configuredGroupID string, configuredRootID string, currentGroupID string, currentRootID string, currentRootPath string, payload map[string]any) (string, string, string, []Mount, error) {
	groupID, rootID, rootPath, _, spaces, err := applyPayloadWithMode(configuredGroupID, configuredRootID, currentGroupID, currentRootID, currentRootPath, ModeAuto, payload)
	return groupID, rootID, rootPath, spaces, err
}

func applyPayloadWithMode(configuredGroupID string, configuredRootID string, currentGroupID string, currentRootID string, currentRootPath string, currentMode string, payload map[string]any) (string, string, string, string, []Mount, error) {
	fileGroup := ""
	fileRoot := "0"
	fileRootPath := "/"
	fileMode := ModeAuto
	var spaces []Mount
	if payload != nil {
		rawGroup, hasGroup := payload["group_id"]
		if hasGroup {
			groupText, ok := rawGroup.(string)
			if !ok {
				return "", "", "", "", nil, configErrorf("workspace.group_id is invalid")
			}
			if groupText != "" {
				if err := ValidateIdentifier(groupText, "workspace.group_id"); err != nil {
					return "", "", "", "", nil, err
				}
				fileGroup = groupText
			}
		}
		rawRoot, hasRoot := payload["root_id"]
		if !hasRoot {
			rawRoot = "0"
		}
		rootText, ok := rawRoot.(string)
		if !ok {
			return "", "", "", "", nil, configErrorf("workspace.root_id is invalid")
		}
		if err := ValidateIdentifier(rootText, "workspace.root_id"); err != nil {
			return "", "", "", "", nil, err
		}
		fileRoot = rootText
		if rawPath, present := payload["root_path"]; present {
			rootPath, ok := rawPath.(string)
			if !ok {
				return "", "", "", "", nil, configErrorf("workspace.root_path is invalid")
			}
			if err := ValidateSelectionPath(rootPath, "workspace.root_path"); err != nil {
				return "", "", "", "", nil, err
			}
			fileRootPath = rootPath
		}

		var err error
		if spaces, err = parseSpaces(payload["spaces"]); err != nil {
			return "", "", "", "", nil, err
		}
		if rawMode, present := payload["mode"]; present {
			value, ok := rawMode.(string)
			if !ok || (value != ModeAuto && value != ModeBusiness && value != ModePersonal) {
				return "", "", "", "", nil, configErrorf("workspace.mode is invalid")
			}
			fileMode = value
		}
	}

	groupID := currentGroupID
	if configuredGroupID == "" || configuredGroupID == AutoValue {
		groupID = fileGroup
	}
	rootID := currentRootID
	if configuredRootID == AutoValue {
		rootID = fileRoot
	}
	rootPath := currentRootPath
	if rootPath == "" {
		rootPath = "/"
	}
	if configuredRootID == AutoValue {
		rootPath = fileRootPath
	}
	mode := currentMode
	if mode == "" || mode == ModeAuto {
		mode = fileMode
	}
	return groupID, rootID, rootPath, mode, spaces, nil
}

func parseSpaces(raw any) ([]Mount, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 || len(items) > MaxSpaces {
		return nil, configErrorf("workspace.spaces is invalid")
	}
	seenGroups := make(map[string]bool, len(items))
	spaces := make([]Mount, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, configErrorf("workspace space is invalid")
		}
		group, _ := fields["group_id"].(string)
		if err := ValidateIdentifier(group, "space.group_id"); err != nil {
			return nil, err
		}
		root, hasRoot := fields["root_id"]
		if !hasRoot {
			root = "0"
		}
		rootText, ok := root.(string)
		if !ok {
			return nil, configErrorf("space.root_id is invalid")
		}
		if err := ValidateIdentifier(rootText, "space.root_id"); err != nil {
			return nil, err
		}
		name, hasName := fields["name"]
		nameText := group
		if hasName {
			var ok bool
			nameText, ok = name.(string)
			if !ok {
				return nil, configErrorf("space.name is invalid")
			}
		}
		if err := validateMountName(nameText); err != nil {
			return nil, err
		}
		pathText := "/"
		if rawPath, present := fields["path"]; present {
			var ok bool
			pathText, ok = rawPath.(string)
			if !ok {
				return nil, configErrorf("space.path is invalid")
			}
		}
		if err := ValidateSelectionPath(pathText, "space.path"); err != nil {
			return nil, err
		}
		if seenGroups[group] {
			return nil, configErrorf("workspace spaces contain duplicate groups")
		}
		seenGroups[group] = true
		spaces = append(spaces, Mount{GroupID: group, RootID: rootText, Name: nameText, Path: pathText})
	}
	return spaces, nil
}

func mtimeEqual(a *int64, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// translateReadErr maps securefile codes to the fixed workspace messages of
// the Python reference.
func translateReadErr(err error) error {
	switch securefile.CodeOf(err) {
	case securefile.CodeInvalidPath:
		return configErrorf("workspace file path is invalid")
	case securefile.CodeParentSymlink:
		return configErrorf("workspace file path must not use symlinks")
	case securefile.CodeParentUnavailable:
		return configErrorf("workspace file directory is unavailable")
	case securefile.CodeParentUnsafe:
		return configErrorf("workspace file directory must be private")
	case securefile.CodeStatFailed:
		return configErrorf("workspace file is unavailable")
	case securefile.CodeNotRegular:
		return configErrorf("workspace file must be a regular file")
	case securefile.CodeFileUnsafe:
		return configErrorf("workspace file permissions are too broad")
	case securefile.CodeOpenFailed, securefile.CodeReadFailed:
		return configErrorf("read workspace file failed")
	case securefile.CodePostOpenUnsafe:
		return configErrorf("workspace file is unsafe")
	case securefile.CodeTooLarge:
		return configErrorf("workspace file is too large")
	case securefile.CodeNotUTF8:
		return configErrorf("workspace file is not valid UTF-8")
	case securefile.CodeNotJSON:
		return configErrorf("workspace file is not valid JSON")
	case securefile.CodeNotObject:
		return configErrorf("workspace file must contain a JSON object")
	default:
		return configErrorf("read workspace file failed")
	}
}

// translateWriteErr maps write-stage codes; the Python persist path
// validates the parent with specific messages and wraps every later OSError
// as "write workspace file failed".
func translateWriteErr(err error) error {
	switch securefile.CodeOf(err) {
	case securefile.CodeInvalidPath:
		return configErrorf("workspace file path is invalid")
	case securefile.CodeParentSymlink:
		return configErrorf("workspace file path must not use symlinks")
	case securefile.CodeParentUnavailable:
		return configErrorf("workspace file directory is unavailable")
	case securefile.CodeParentUnsafe:
		return configErrorf("workspace file directory must be private")
	default:
		return configErrorf("write workspace file failed")
	}
}
