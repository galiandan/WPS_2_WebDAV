// Package workspace resolves configured or login-selected group and root
// IDs from the workspace state file.
//
// B201 scope: load-time validation and group/root resolution with the same
// accept/reject semantics as the Python reference
// (src/wps_adapter/workspace.py). Hot reload and multi-space routing land
// with their own migration tasks.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AutoValue keeps a setting on automatic resolution from the state file.
const AutoValue = "auto"

// MaxSpaces mirrors MAX_WORKSPACE_SPACES.
const MaxSpaces = 128

// MaxFileBytes mirrors MAX_WORKSPACE_FILE_BYTES.
const MaxFileBytes = 16 * 1024

// DefaultFile mirrors DEFAULT_WORKSPACE_FILE.
const DefaultFile = "/etc/wps-adapter/secrets/wps-workspace.json"

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
}

// State is the resolved workspace view.
type State struct {
	GroupID string
	RootID  string
	Spaces  []Mount
}

// LoadFromFile mirrors WorkspaceState.from_file: validate the configured
// identifiers, read the state file if present, and resolve group/root.
func LoadFromFile(filePath string, configuredGroupID string, configuredRootID string) (*State, error) {
	if filePath == "" {
		return nil, configErrorf("workspace file path is invalid")
	}
	if err := validatePath(filePath); err != nil {
		return nil, err
	}
	configuredGroupID, err := validateConfigured(configuredGroupID, "WPS_GROUP_ID", true)
	if err != nil {
		return nil, err
	}
	if configuredRootID == "" {
		configuredRootID = "0"
	}
	configuredRootID, err = validateConfigured(configuredRootID, "WPS_ROOT_ID", false)
	if err != nil {
		return nil, err
	}

	groupID := ""
	if configuredGroupID != "" && configuredGroupID != AutoValue {
		groupID = configuredGroupID
	}
	rootID := configuredRootID
	if configuredRootID == AutoValue {
		rootID = "0"
	}

	payload, err := readFile(filePath)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		rawGroup, _ := payload["group_id"].(string)
		rawRoot, hasRoot := payload["root_id"]
		fileGroup := ""
		if rawGroup != "" {
			if err := ValidateIdentifier(rawGroup, "workspace.group_id"); err != nil {
				return nil, err
			}
			fileGroup = rawGroup
		}
		if !hasRoot {
			rawRoot = "0"
		}
		rootText, ok := rawRoot.(string)
		if !ok {
			return nil, configErrorf("workspace.root_id is invalid")
		}
		if err := ValidateIdentifier(rootText, "workspace.root_id"); err != nil {
			return nil, err
		}
		if configuredGroupID == "" || configuredGroupID == AutoValue {
			groupID = fileGroup
		}
		if configuredRootID == AutoValue {
			rootID = rootText
		}
		spaces, err := parseSpaces(payload["spaces"])
		if err != nil {
			return nil, err
		}
		return &State{GroupID: groupID, RootID: rootID, Spaces: spaces}, nil
	}
	return &State{GroupID: groupID, RootID: rootID}, nil
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

func parseSpaces(raw any) ([]Mount, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 || len(items) > MaxSpaces {
		return nil, configErrorf("workspace.spaces is invalid")
	}
	seenGroups := make(map[string]bool, len(items))
	seenNames := make(map[string]bool, len(items))
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
			nameText, ok = name.(string)
			if !ok {
				return nil, configErrorf("space.name is invalid")
			}
		}
		if nameText == "" || strings.ContainsAny(nameText, "/\\") {
			return nil, configErrorf("space.name is invalid")
		}
		if len(nameText) > 4096 {
			return nil, configErrorf("space.name is too long")
		}
		if seenGroups[group] {
			return nil, configErrorf("workspace spaces contain duplicate groups")
		}
		seenGroups[group] = true
		if seenNames[nameText] {
			return nil, configErrorf("WPS space names must be unique")
		}
		seenNames[nameText] = true
		spaces = append(spaces, Mount{GroupID: group, RootID: rootText, Name: nameText})
	}
	return spaces, nil
}

// validatePath mirrors the workspace file path checks: absolute path, no
// control characters, no symlinked parent, private directory and file.
func validatePath(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return configErrorf("workspace file path is invalid")
	}
	parent := filepath.Dir(path)
	realParent, err := filepath.EvalSymlinks(parent)
	if err == nil && realParent != parent {
		return configErrorf("workspace file path must not use symlinks")
	}
	if err != nil && !os.IsNotExist(err) {
		return configErrorf("workspace file directory is unavailable")
	}
	metadata, err := os.Stat(parent)
	if err != nil {
		if os.IsNotExist(err) {
			// Fresh installs create the directory before the first login.
			return nil
		}
		return configErrorf("workspace file directory is unavailable")
	}
	if !metadata.IsDir() || metadata.Mode().Perm()&0o077 != 0 || !OwnedByService(metadata) {
		return configErrorf("workspace file directory must be private")
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return configErrorf("workspace file is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return configErrorf("workspace file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 || !OwnedByService(info) {
		return configErrorf("workspace file permissions are too broad")
	}
	return nil
}

func readFile(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, configErrorf("read workspace file failed")
	}
	if len(raw) > MaxFileBytes {
		return nil, configErrorf("workspace file is too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, configErrorf("workspace file is not valid JSON")
	}
	payload, ok := decoded.(map[string]any)
	if !ok {
		return nil, configErrorf("workspace file must contain a JSON object")
	}
	return payload, nil
}
