package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
)

// Defaults and limits mirroring settings.py.
const (
	DefaultWebSettingsFile  = "/etc/wps-adapter/secrets/web-settings.json"
	DefaultRootName         = "WPS Enterprise Drive"
	MaxWebSettingsFileBytes = int64(16 * 1024)
	MaxRootNameChars        = 256
	MaxRootNameBytes        = 1024
)

// SettingsError marks invalid web settings values (Python WebSettingsError).
type SettingsError struct{ Msg string }

func (e *SettingsError) Error() string { return e.Msg }

// SettingsFileError marks unsafe or unavailable web settings files
// (Python WebSettingsFileError).
type SettingsFileError struct{ Msg string }

func (e *SettingsFileError) Error() string { return e.Msg }

func settingsErrorf(format string, args ...any) error {
	return &SettingsError{Msg: fmt.Sprintf(format, args...)}
}

func settingsFileErrorf(format string, args ...any) error {
	return &SettingsFileError{Msg: fmt.Sprintf(format, args...)}
}

// ValidateRootName validates and normalizes a user-visible adapter root
// name; value stays untyped because it arrives from JSON payloads.
func ValidateRootName(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", settingsErrorf("root name must be a string")
	}
	name := strings.TrimSpace(text)
	if name == "" {
		return "", settingsErrorf("root name must not be empty")
	}
	if len([]rune(name)) > MaxRootNameChars || len(name) > MaxRootNameBytes {
		return "", settingsErrorf("root name is too long")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7F {
			return "", settingsErrorf("root name contains a control character")
		}
	}
	return name, nil
}

// WebSettings keeps the display name in a small private file beside the
// adapter secrets, hot-reloading on mtime change like Python's WebSettings.
type WebSettings struct {
	filePath     string
	fallbackName string

	mu      sync.Mutex
	name    string
	mtimeNs *int64
}

// NewWebSettings mirrors __post_init__: validate the fallback name, then the
// path, then read the file once. An empty filePath keeps settings in memory
// only.
func NewWebSettings(filePath string, fallbackName string) (*WebSettings, error) {
	var err error
	if fallbackName, err = ValidateRootName(fallbackName); err != nil {
		return nil, err
	}
	if filePath != "" {
		if err := securefile.ValidateStatePath(filePath); err != nil {
			return nil, translateSettingsReadErr(err)
		}
	}
	settings := &WebSettings{
		filePath:     filePath,
		fallbackName: fallbackName,
		name:         fallbackName,
	}
	if err := settings.refreshLocked(true); err != nil {
		return nil, err
	}
	return settings, nil
}

// Name returns the current display name, hot-reloading the file first.
func (s *WebSettings) Name() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(false); err != nil {
		return "", err
	}
	return s.name, nil
}

// SetName validates and persists a new display name; on a write failure the
// previous name stays active.
func (s *WebSettings) SetName(value any) (string, error) {
	name, err := ValidateRootName(value)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.filePath != "" {
		mtimeNs, err := securefile.WriteAtomic(s.filePath, buildSettingsPayload(name))
		if err != nil {
			return "", translateSettingsWriteErr(err)
		}
		s.mtimeNs = &mtimeNs
	}
	s.name = name
	return name, nil
}

func (s *WebSettings) refreshLocked(force bool) error {
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
		// statMtime stays nil while the file is missing.
	default:
		return settingsFileErrorf("stat web settings file failed")
	}
	if !force && mtimeEqual(statMtime, s.mtimeNs) {
		return nil
	}
	payload, readMtime, err := securefile.ReadJSONState(s.filePath, MaxWebSettingsFileBytes)
	if err != nil {
		return translateSettingsReadErr(err)
	}
	if payload == nil {
		s.name = s.fallbackName
	} else if err := s.applyName(payload); err != nil {
		return err
	}
	s.mtimeNs = readMtime
	return nil
}

func (s *WebSettings) applyName(payload map[string]any) error {
	name, err := ValidateRootName(payload["name"])
	if err != nil {
		return err
	}
	s.name = name
	return nil
}

func buildSettingsPayload(name string) string {
	var b strings.Builder
	b.WriteString(`{"name":"`)
	b.WriteString(pyEscape(name))
	b.WriteString(`"}`)
	return b.String()
}

// translateSettingsReadErr maps securefile codes to the Python settings
// wording, splitting value problems (SettingsError) from file problems
// (SettingsFileError) exactly like the reference exception types.
func translateSettingsReadErr(err error) error {
	switch securefile.CodeOf(err) {
	case securefile.CodeInvalidPath:
		return settingsErrorf("web settings file path is invalid")
	case securefile.CodeParentSymlink:
		return settingsErrorf("web settings file path must not use symlinks")
	case securefile.CodeParentUnavailable:
		return settingsFileErrorf("stat web settings directory failed")
	case securefile.CodeParentUnsafe:
		return settingsErrorf("web settings directory must be private")
	case securefile.CodeStatFailed:
		return settingsFileErrorf("stat web settings file failed")
	case securefile.CodeNotRegular:
		return settingsErrorf("web settings file must be a regular file")
	case securefile.CodeFileUnsafe:
		return settingsErrorf("web settings file permissions are too broad")
	case securefile.CodeOpenFailed, securefile.CodeReadFailed:
		return settingsFileErrorf("read web settings file failed")
	case securefile.CodePostOpenUnsafe:
		return settingsErrorf("web settings file is unsafe")
	case securefile.CodeTooLarge:
		return settingsErrorf("web settings file is too large")
	case securefile.CodeNotUTF8:
		return settingsErrorf("web settings file is not valid UTF-8")
	case securefile.CodeNotJSON:
		return settingsErrorf("web settings file is not valid JSON")
	case securefile.CodeNotObject:
		return settingsErrorf("web settings file must contain a JSON object")
	default:
		return settingsFileErrorf("read web settings file failed")
	}
}

func translateSettingsWriteErr(err error) error {
	switch code := securefile.CodeOf(err); code {
	case securefile.CodeInvalidPath:
		return settingsErrorf("web settings file path is invalid")
	case securefile.CodeParentSymlink:
		return settingsErrorf("web settings file path must not use symlinks")
	case securefile.CodeParentUnavailable:
		return settingsFileErrorf("stat web settings directory failed")
	case securefile.CodeParentUnsafe:
		return settingsErrorf("web settings directory must be private")
	default:
		return settingsFileErrorf("write web settings file failed")
	}
}
