// Package config loads adapter runtime configuration from the environment.
//
// Field set, defaults, types, and accept/reject rules mirror the Python
// reference (src/wps_adapter/__main__.py, client.py from_env, storage.py,
// server.py, settings.py, workspace.py) in the same evaluation order, so
// check-config agrees with the Python service on the same environment.
// Error text names the variable and the rule; it never echoes the value.
package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// Defaults shared with the Python service.
const (
	DefaultBind           = "127.0.0.1"
	DefaultPort           = 54321
	DefaultDAVPrefix      = "/dav"
	DefaultRESTPrefix     = "/api/v1"
	DefaultRootName       = "WPS Enterprise Drive"
	DefaultBaseURL        = "https://365.kdocs.cn"
	DefaultObjectSuffix   = ".ag.kdocs.cn"
	DefaultMaxConnections = 64
	DefaultRequestTimeout = 60.0

	DefaultWebSettingsFile = "/etc/wps-adapter/secrets/web-settings.json"

	kib int64 = 1024
	mib int64 = 1024 * 1024
	gib int64 = 1024 * 1024 * 1024
)

// fallbackWebSettingsFile is a var so tests can redirect the default web
// settings path; production always uses DefaultWebSettingsFile.
var fallbackWebSettingsFile = DefaultWebSettingsFile

// Config carries every runtime setting of the adapter.
type Config struct {
	// Workspace resolution.
	GroupID        string
	RootID         string
	WorkspaceFile  string
	WorkspaceState *workspace.State

	// WPS client connection.
	CookieFile          string
	CSRFTokenFile       string
	RefreshCommand      []string
	RefreshTimeout      float64
	BaseURL             string
	AccountBaseURL      string
	ObjectSuffix        string
	AutoRefresh         bool
	Referer             string
	Origin              string
	CID                 string
	Timeout             float64
	StatusProbeTTL      float64
	StatusFailureBackup float64

	// Upload/download client behaviour.
	UploadSpoolMemory  int64
	StreamChunkSize    int64
	MultipartThreshold int64
	MultipartPartSize  int64
	EnableRange        bool
	UploadSpoolDir     string
	UploadResumeDir    string
	UploadMinFreeBytes int64
	MaxUploadBytes     int64
	UploadRetries      int
	UploadRetryDelay   float64
	MaxJSONResponse    int64

	// Storage options.
	RootName         string
	ListCount        int
	MaxListEntries   int
	CacheTTL         float64
	MaxCachedFolders int
	MaxUploads       int
	MaxDownloads     int
	TransferWait     float64
	MaxCopyEntries   int
	MaxCopyDepth     int

	// HTTP application limits.
	MaxPropfindEntries int
	MaxPropfindDepth   int
	MaxControlBody     int64
	MaxResponseBody    int64
	MaxLocks           int

	// Adapter Basic Auth and networking.
	Username       string
	Password       string
	UsernameFile   string
	PasswordFile   string
	WebSettingsDir string
	DAVPrefix      string
	RESTPrefix     string
	Bind           string
	Port           int
	MaxConnections int
	RequestTimeout float64
}

// Load reads the environment with the Python reference's evaluation order:
// client env first (including workspace state), then the web root name and
// web settings path, client-side validation, storage options, application
// limits, and finally adapter networking fields.
func Load() (Config, error) {
	cfg := Config{}

	// --- WpsClientConfig.from_env, in kwarg order ---
	refreshText := strings.TrimSpace(os.Getenv("WPS_CREDENTIAL_REFRESH_COMMAND"))
	if refreshText != "" {
		parts, err := splitCommand(refreshText)
		if err != nil {
			return Config{}, fmt.Errorf("WPS_CREDENTIAL_REFRESH_COMMAND must be a valid command line")
		}
		cfg.RefreshCommand = parts
	}
	cfg.GroupID = os.Getenv("WPS_GROUP_ID")
	cfg.RootID = os.Getenv("WPS_ROOT_ID")
	if cfg.RootID == "" {
		cfg.RootID = "0"
	}
	cfg.WorkspaceFile = os.Getenv("WPS_WORKSPACE_FILE")
	if cfg.WorkspaceFile == "" {
		cfg.WorkspaceFile = workspace.DefaultFile
	}
	// The workspace file is loaded and validated when the configuration does
	// not pin both IDs explicitly or when the file is present.
	groupAuto := cfg.GroupID == "" || cfg.GroupID == workspace.AutoValue
	rootAuto := cfg.RootID == workspace.AutoValue
	if groupAuto || rootAuto || fileExists(cfg.WorkspaceFile) {
		state, err := workspace.LoadFromFile(cfg.WorkspaceFile, cfg.GroupID, cfg.RootID)
		if err != nil {
			return Config{}, err
		}
		cfg.WorkspaceState = state
	}
	cfg.CookieFile = os.Getenv("WPS_COOKIE_FILE")
	cfg.CSRFTokenFile = os.Getenv("WPS_CSRF_TOKEN_FILE")

	refreshTimeout, err := envFloat("WPS_CREDENTIAL_REFRESH_TIMEOUT", 30)
	if err != nil {
		return Config{}, err
	}
	cfg.RefreshTimeout = refreshTimeout

	// Python reads these with os.environ.get(name, default): an explicitly
	// empty value is kept and fails validation later, like the reference.
	if _, present := os.LookupEnv("WPS_BASE_URL"); present {
		cfg.BaseURL = os.Getenv("WPS_BASE_URL")
	} else {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.AccountBaseURL = os.Getenv("WPS_ACCOUNT_BASE_URL")
	if _, present := os.LookupEnv("WPS_OBJECT_STORAGE_HOST_SUFFIX"); present {
		cfg.ObjectSuffix = os.Getenv("WPS_OBJECT_STORAGE_HOST_SUFFIX")
	} else {
		cfg.ObjectSuffix = DefaultObjectSuffix
	}

	autoRefresh, err := envBool("WPS_AUTO_REFRESH", true)
	if err != nil {
		return Config{}, err
	}
	cfg.AutoRefresh = autoRefresh

	cfg.Referer = os.Getenv("WPS_REFERER")
	cfg.Origin = os.Getenv("WPS_ORIGIN")
	cfg.CID = os.Getenv("WPS_CID")

	timeout, err := envFloat("WPS_TIMEOUT", 30)
	if err != nil {
		return Config{}, err
	}
	cfg.Timeout = timeout
	probeTTL, err := envFloat("WPS_STATUS_PROBE_TTL", 30)
	if err != nil {
		return Config{}, err
	}
	cfg.StatusProbeTTL = probeTTL
	backoff, err := envFloat("WPS_STATUS_FAILURE_BACKOFF", 5)
	if err != nil {
		return Config{}, err
	}
	cfg.StatusFailureBackup = backoff

	spoolMemory, err := envInt64("WPS_UPLOAD_SPOOL_MEMORY", 8*mib)
	if err != nil {
		return Config{}, err
	}
	cfg.UploadSpoolMemory = spoolMemory
	chunkSize, err := envInt64("WPS_STREAM_CHUNK_SIZE", 1*mib)
	if err != nil {
		return Config{}, err
	}
	cfg.StreamChunkSize = chunkSize
	threshold, err := envInt64("WPS_MULTIPART_THRESHOLD", 50*mib)
	if err != nil {
		return Config{}, err
	}
	cfg.MultipartThreshold = threshold
	partSize, err := envInt64("WPS_MULTIPART_PART_SIZE", 10*mib)
	if err != nil {
		return Config{}, err
	}
	cfg.MultipartPartSize = partSize

	enableRange, err := envBool("WPS_ENABLE_RANGE", true)
	if err != nil {
		return Config{}, err
	}
	cfg.EnableRange = enableRange

	cfg.UploadSpoolDir = os.Getenv("WPS_UPLOAD_SPOOL_DIR")
	cfg.UploadResumeDir = os.Getenv("WPS_UPLOAD_RESUME_DIR")

	minFree, err := envInt64("WPS_UPLOAD_MIN_FREE_BYTES", 512*mib)
	if err != nil {
		return Config{}, err
	}
	cfg.UploadMinFreeBytes = minFree
	maxUpload, err := envInt64("WPS_MAX_UPLOAD_BYTES", 1*gib)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxUploadBytes = maxUpload
	retries, err := envInt("WPS_UPLOAD_RETRIES", 2)
	if err != nil {
		return Config{}, err
	}
	cfg.UploadRetries = retries
	retryDelay, err := envFloat("WPS_UPLOAD_RETRY_DELAY", 0.5)
	if err != nil {
		return Config{}, err
	}
	cfg.UploadRetryDelay = retryDelay
	maxJSON, err := envInt64("WPS_MAX_JSON_RESPONSE_BYTES", 8*mib)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxJSONResponse = maxJSON

	// --- WebSettings fallback root name and settings path ---
	cfg.RootName = os.Getenv("WPS_ROOT_NAME")
	if cfg.RootName == "" {
		cfg.RootName = DefaultRootName
	}
	if err := validateRootName(cfg.RootName); err != nil {
		return Config{}, err
	}
	cfg.WebSettingsDir = fallbackWebSettingsFile
	if err := validateWebSettingsPath(cfg.WebSettingsDir); err != nil {
		return Config{}, err
	}

	// --- WpsDriveClient.__init__ validation ---
	if err := cfg.validateClient(); err != nil {
		return Config{}, err
	}

	// --- storage options (__main__ storage_options) ---
	listCount, err := envInt("WPS_LIST_COUNT", 20)
	if err != nil {
		return Config{}, err
	}
	cfg.ListCount = listCount
	maxListEntries, err := envInt("WPS_MAX_LIST_ENTRIES", 10000)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxListEntries = maxListEntries
	cacheTTL, err := envFloat("WPS_CACHE_TTL", 2.0)
	if err != nil {
		return Config{}, err
	}
	cfg.CacheTTL = cacheTTL
	maxCachedFolders, err := envInt("WPS_MAX_CACHED_FOLDERS", 1024)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxCachedFolders = maxCachedFolders
	maxUploads, err := envInt("WPS_MAX_UPLOADS", 2)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxUploads = maxUploads
	maxDownloads, err := envInt("WPS_MAX_DOWNLOADS", 4)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxDownloads = maxDownloads
	transferWait, err := envFloat("WPS_TRANSFER_WAIT_TIMEOUT", 30.0)
	if err != nil {
		return Config{}, err
	}
	cfg.TransferWait = transferWait
	copyEntries, err := envInt("WPS_MAX_COPY_ENTRIES", 10000)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxCopyEntries = copyEntries
	copyDepth, err := envInt("WPS_MAX_COPY_DEPTH", 64)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxCopyDepth = copyDepth
	// Python only builds a WpsStorage (where these rules live) when a group
	// is resolved or workspace spaces exist.
	if err := cfg.validateStorage(); err != nil {
		return Config{}, err
	}

	// --- AdapterApplication limits ---
	propfindEntries, err := envInt("WPS_MAX_PROPFIND_ENTRIES", 10000)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxPropfindEntries = propfindEntries
	propfindDepth, err := envInt("WPS_MAX_PROPFIND_DEPTH", 64)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxPropfindDepth = propfindDepth
	controlBody, err := envInt64("WPS_MAX_CONTROL_BODY", 1*mib)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxControlBody = controlBody
	responseBody, err := envInt64("WPS_MAX_RESPONSE_BODY_BYTES", 16*mib)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxResponseBody = responseBody
	maxLocks, err := envInt("WPS_MAX_LOCKS", 4096)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxLocks = maxLocks
	if err := cfg.validateApplicationLimits(); err != nil {
		return Config{}, err
	}

	// --- Basic Auth, prefixes, and networking ---
	cfg.Username = os.Getenv("ADAPTER_USERNAME")
	cfg.Password = os.Getenv("ADAPTER_PASSWORD")
	cfg.UsernameFile = os.Getenv("ADAPTER_USERNAME_FILE")
	cfg.PasswordFile = os.Getenv("ADAPTER_PASSWORD_FILE")
	cfg.DAVPrefix = normalisePrefix(os.Getenv("ADAPTER_DAV_PREFIX"), DefaultDAVPrefix)
	cfg.RESTPrefix = normalisePrefix(os.Getenv("ADAPTER_REST_PREFIX"), DefaultRESTPrefix)
	cfg.Bind = os.Getenv("ADAPTER_BIND")
	if cfg.Bind == "" {
		cfg.Bind = DefaultBind
	}
	// Python parses ADAPTER_PORT while building the CLI parser, so a broken
	// value fails every command.
	port, err := envInt("ADAPTER_PORT", DefaultPort)
	if err != nil {
		return Config{}, err
	}
	cfg.Port = port
	// ADAPTER_MAX_CONNECTIONS and ADAPTER_REQUEST_TIMEOUT are parsed on the
	// serve path only; ParseServerRuntime and ValidateRuntime enforce them
	// there.
	cfg.MaxConnections = DefaultMaxConnections
	cfg.RequestTimeout = DefaultRequestTimeout

	return cfg, nil
}

// validateClient mirrors WpsDriveClient.__init__ checks.
func (c Config) validateClient() error {
	if c.GroupID == "" && c.WorkspaceState == nil {
		return fmt.Errorf("WPS_GROUP_ID or workspace state is required")
	}
	if c.MaxJSONResponse <= 0 {
		return fmt.Errorf("WPS_MAX_JSON_RESPONSE_BYTES must be positive")
	}
	if err := validateWPSURL(c.BaseURL, "WPS_BASE_URL"); err != nil {
		return err
	}
	if err := validateObjectSuffix(c.ObjectSuffix); err != nil {
		return err
	}
	if c.StatusProbeTTL < 0 {
		return fmt.Errorf("WPS_STATUS_PROBE_TTL must not be negative")
	}
	if c.StatusFailureBackup < 0 {
		return fmt.Errorf("WPS_STATUS_FAILURE_BACKOFF must not be negative")
	}
	return nil
}

// storageValidated mirrors the Python condition under which a WpsStorage is
// actually constructed: workspace spaces always build one, otherwise a
// resolved group id is required.
func (c Config) storageValidated() bool {
	if c.WorkspaceState != nil && len(c.WorkspaceState.Spaces) > 0 {
		return true
	}
	return c.ResolvedGroupID() != ""
}

// validateStorage mirrors WpsStorage.__init__ checks.
func (c Config) validateStorage() error {
	if !c.storageValidated() {
		return nil
	}
	if c.resolvedRootID() == "" {
		return fmt.Errorf("WPS_ROOT_ID is required")
	}
	if c.ListCount <= 0 {
		return fmt.Errorf("WPS_LIST_COUNT must be positive")
	}
	if c.MaxListEntries <= 0 {
		return fmt.Errorf("WPS_MAX_LIST_ENTRIES must be positive")
	}
	if c.ListCount > c.MaxListEntries {
		return fmt.Errorf("WPS_LIST_COUNT must not exceed WPS_MAX_LIST_ENTRIES")
	}
	if c.CacheTTL < 0 {
		return fmt.Errorf("WPS_CACHE_TTL must not be negative")
	}
	if c.MaxCachedFolders <= 0 {
		return fmt.Errorf("WPS_MAX_CACHED_FOLDERS must be positive")
	}
	if c.MaxUploads <= 0 {
		return fmt.Errorf("WPS_MAX_UPLOADS must be positive")
	}
	if c.MaxDownloads <= 0 {
		return fmt.Errorf("WPS_MAX_DOWNLOADS must be positive")
	}
	if c.TransferWait <= 0 {
		return fmt.Errorf("WPS_TRANSFER_WAIT_TIMEOUT must be positive")
	}
	if c.MaxCopyEntries <= 0 {
		return fmt.Errorf("WPS_MAX_COPY_ENTRIES must be positive")
	}
	if c.MaxCopyDepth <= 0 {
		return fmt.Errorf("WPS_MAX_COPY_DEPTH must be positive")
	}
	return nil
}

// validateApplicationLimits mirrors AdapterApplication.__post_init__ and
// DavLockStore checks; the lock store is constructed first in Python.
func (c Config) validateApplicationLimits() error {
	if c.MaxLocks <= 0 {
		return fmt.Errorf("WPS_MAX_LOCKS must be positive")
	}
	if c.MaxPropfindEntries <= 0 {
		return fmt.Errorf("WPS_MAX_PROPFIND_ENTRIES must be positive")
	}
	if c.MaxPropfindDepth <= 0 {
		return fmt.Errorf("WPS_MAX_PROPFIND_DEPTH must be positive")
	}
	if c.MaxControlBody <= 0 {
		return fmt.Errorf("WPS_MAX_CONTROL_BODY must be positive")
	}
	if c.MaxResponseBody <= 0 {
		return fmt.Errorf("WPS_MAX_RESPONSE_BODY_BYTES must be positive")
	}
	return nil
}

// ValidateRuntime mirrors the serve-path checks in create_server. It is not
// part of Load because check-config does not construct a server.
func (c Config) ValidateRuntime() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("ADAPTER_PORT must be between 1 and 65535")
	}
	if c.MaxConnections <= 0 {
		return fmt.Errorf("ADAPTER_MAX_CONNECTIONS must be positive")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("ADAPTER_REQUEST_TIMEOUT must be positive")
	}
	return nil
}

// ParseServerRuntime reads the serve-only variables; Python evaluates them
// only when create_server is called.
func ParseServerRuntime() (maxConnections int, requestTimeout float64, err error) {
	maxConnections, err = envInt("ADAPTER_MAX_CONNECTIONS", DefaultMaxConnections)
	if err != nil {
		return 0, 0, err
	}
	requestTimeout, err = envFloat("ADAPTER_REQUEST_TIMEOUT", DefaultRequestTimeout)
	if err != nil {
		return 0, 0, err
	}
	return maxConnections, requestTimeout, nil
}

// AuthEnabled mirrors BasicAuth.enabled: any of the four settings turns
// authentication on.
func (c Config) AuthEnabled() bool {
	return c.Username != "" || c.Password != "" ||
		c.UsernameFile != "" || c.PasswordFile != ""
}

// CheckPublicBind refuses a non-local bind on a server without Basic Auth.
func (c Config) CheckPublicBind() error {
	local := map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}
	if !local[c.Bind] && !c.AuthEnabled() {
		return fmt.Errorf("refusing a non-local bind without ADAPTER_USERNAME/PASSWORD or secret files")
	}
	return nil
}

// ResolvedGroupID returns the effective group id for the check-config
// summary: the workspace value when loaded, else the configured value when
// it is neither empty nor "auto".
func (c Config) ResolvedGroupID() string {
	if c.WorkspaceState != nil {
		return c.WorkspaceState.GroupID
	}
	if c.GroupID == "" || c.GroupID == workspace.AutoValue {
		return ""
	}
	return c.GroupID
}

func (c Config) resolvedRootID() string {
	if c.WorkspaceState != nil {
		return c.WorkspaceState.RootID
	}
	return c.RootID
}

// --- parsing helpers; all error text names the variable and the rule ---

func envBool(name string, fallback bool) (bool, error) {
	value, present := os.LookupEnv(name)
	if !present {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s must be a boolean", name)
}

func envInt(name string, fallback int) (int, error) {
	value, present := os.LookupEnv(name)
	if !present {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("%s is out of range", name)
		}
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func envInt64(name string, fallback int64) (int64, error) {
	value, present := os.LookupEnv(name)
	if !present {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("%s is out of range", name)
		}
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

// envFloat mirrors Python float(): surrounding whitespace is ignored and an
// overflowing literal becomes an infinity instead of an error. NaN passes
// through exactly as the reference does (NaN comparisons are false).
func envFloat(name string, fallback float64) (float64, error) {
	value, present := os.LookupEnv(name)
	if !present {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) && math.IsInf(parsed, 0) {
			return parsed, nil
		}
		return 0, fmt.Errorf("%s must be a number", name)
	}
	return parsed, nil
}

func normalisePrefix(value, fallback string) string {
	if value == "" {
		value = fallback
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	value = strings.TrimRight(value, "/")
	if value == "" {
		return "/"
	}
	return value
}

// fileExists mirrors os.path.exists: any file system entry counts.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// splitCommand mirrors shlex.split for the refresh command: whitespace
// separated words with single- and double-quoted parts.
func splitCommand(text string) ([]string, error) {
	var words []string
	var current strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(text); i++ {
		char := text[i]
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
			} else {
				current.WriteByte(char)
			}
		case char == '\'' || char == '"':
			quote = char
			inWord = true
		case char == ' ' || char == '\t' || char == '\n' || char == '\r':
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteByte(char)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quote")
	}
	if inWord {
		words = append(words, current.String())
	}
	return words, nil
}

// validateRootName mirrors settings.validate_root_name.
func validateRootName(value string) error {
	name := strings.TrimSpace(value)
	if name == "" {
		return fmt.Errorf("WPS_ROOT_NAME must not be empty")
	}
	if len([]rune(name)) > 256 || len(name) > 1024 {
		return fmt.Errorf("WPS_ROOT_NAME is too long")
	}
	for _, char := range name {
		if char < 0x20 || char == 0x7F {
			return fmt.Errorf("WPS_ROOT_NAME contains a control character")
		}
	}
	return nil
}

// validateWebSettingsPath mirrors settings._validate_path for the default
// web settings file the Python entrypoint constructs at startup.
func validateWebSettingsPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return fmt.Errorf("web settings file path is invalid")
	}
	parent := filepath.Dir(path)
	if realParent, err := filepath.EvalSymlinks(parent); err == nil && realParent != parent {
		return fmt.Errorf("web settings file path must not use symlinks")
	}
	metadata, err := os.Stat(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat web settings directory failed")
	}
	if !metadata.IsDir() || metadata.Mode().Perm()&0o077 != 0 || !workspace.OwnedByService(metadata) {
		return fmt.Errorf("web settings directory must be private")
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat web settings file failed")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("web settings file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 || !workspace.OwnedByService(info) {
		return fmt.Errorf("web settings file permissions are too broad")
	}
	return nil
}

// validateWPSURL enforces the HTTPS WPS host rules for base URLs.
func validateWPSURL(raw string, name string) error {
	parts, err := urlsplit(raw)
	if err != nil ||
		parts.scheme != "https" ||
		parts.hostname == "" ||
		parts.username != "" ||
		parts.password != "" ||
		parts.query != "" ||
		parts.fragment != "" ||
		(parts.path != "" && parts.path != "/") ||
		!isWPSHost(parts.hostname) {
		return fmt.Errorf("%s must be an HTTPS WPS host without a path or credentials", name)
	}
	return nil
}

func validateObjectSuffix(raw string) error {
	suffix := strings.ToLower(strings.Trim(strings.TrimSpace(raw), "."))
	if suffix == "" || (suffix != "kdocs.cn" && !strings.HasSuffix(suffix, ".kdocs.cn")) {
		return fmt.Errorf("WPS_OBJECT_STORAGE_HOST_SUFFIX must be within kdocs.cn")
	}
	return nil
}

func isWPSHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSuffix(host, "."))
	return normalized == "kdocs.cn" || strings.HasSuffix(normalized, ".kdocs.cn")
}

// urlParts is the subset of Python's urlsplit result the config rules need.
type urlParts struct {
	scheme, hostname, username, password, path, query, fragment string
}

func urlsplit(raw string) (urlParts, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return urlParts{}, err
	}
	parts := urlParts{
		scheme:   parsed.Scheme,
		hostname: parsed.Hostname(),
		path:     parsed.Path,
		query:    parsed.RawQuery,
		fragment: parsed.Fragment,
	}
	if parsed.User != nil {
		parts.username = parsed.User.Username()
		parts.password, _ = parsed.User.Password()
	}
	return parts, nil
}
