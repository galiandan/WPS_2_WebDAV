// Package app assembles the adapter services in a fixed order: config ->
// secure state -> credentials/workspace/settings -> HTTP clients -> global
// budget/cache -> storage -> handlers -> server.
// Nothing here dials WPS: construction builds transports and local state
// only, so check-config can run the same assembly offline.
package app

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/config"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/httpserver"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
	"github.com/galiandan/WPS_2_WebDAV/go/web"
)

// webContentSecurityPolicy keeps the embedded page self-contained: there are
// no inline scripts or styles and no third-party network resources.
const webContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self'; connect-src 'self'; object-src 'none'; " +
	"base-uri 'none'; frame-ancestors 'none'"

// healthPayload mirrors the Python /healthz body byte for byte.
type healthPayload struct {
	Status       string `json:"status"`
	Service      string `json:"service"`
	Version      string `json:"version"`
	NetworkCalls string `json:"network_calls"`
}

// Application owns the process-wide services. The zero value is unusable;
// build one with New.
type Application struct {
	Config  config.Config
	Version string

	// Assembled services, in construction order.
	Settings *workspace.WebSettings
	Sessions *auth.Store
	State    *workspace.WorkspaceState // nil without an auto/workspace setup
	Source   credentials.Source        // nil without any credential source
	Client   *wps.Client
	Budget   *budget.Budget
	Storage  *storage.MultiSpace
	Locks    *httpserver.DavLockStore

	rest *httpserver.RESTDispatcher
	dav  *httpserver.DAVDispatcher

	// clientConfig is the base client configuration and transportOptions
	// carries the shared opener/signed transport (real or injected); the
	// space factory derives per-mount clients from both.
	clientConfig     wps.Config
	transportOptions []wps.Option

	// Shared transports: one control-plane opener and one signed transport
	// reused by the base client and every child space. Closed when the assembly
	// or the process stops.
	opener          *http.Client
	signedTransport http.RoundTripper
}

// Option adjusts the assembly. Production wiring passes none; focused tests
// may inject fake WPS transports without changing configuration, secure files,
// storage routing, or the server lifecycle.
type Option func(*transports)

type transports struct {
	opener wps.Opener
	signed http.RoundTripper
}

// WithTransports replaces the control-plane opener and the signed-object
// transport for every client the assembly builds.
func WithTransports(opener wps.Opener, signed http.RoundTripper) Option {
	return func(t *transports) {
		if opener != nil {
			t.opener = opener
		}
		if signed != nil {
			t.signed = signed
		}
	}
}

// options converts the injected transports into client options.
func (t transports) options() []wps.Option {
	var out []wps.Option
	if t.opener != nil {
		out = append(out, wps.WithOpener(t.opener))
	}
	if t.signed != nil {
		out = append(out, wps.WithSignedTransport(t.signed))
	}
	return out
}

// New assembles every service from the loaded configuration. On failure the
// resources created so far are released before the error returns.
func New(cfg config.Config, version string, options ...Option) (*Application, error) {
	application := &Application{Config: cfg, Version: version}
	groupAuto := cfg.GroupID == "" || cfg.GroupID == workspace.AutoValue
	rootAuto := cfg.RootID == workspace.AutoValue
	fail := func(err error) (*Application, error) {
		application.closeTransports()
		return nil, err
	}
	var injected transports
	for _, option := range options {
		option(&injected)
	}
	application.transportOptions = injected.options()

	// --- credentials/workspace/settings ---
	// config.Load keeps a validated snapshot for check-config; the running
	// services share the hot-reloading workspace state built here under the
	// same conditions Python's from_env constructs it (auto group/root or
	// an existing file).
	if groupAuto || rootAuto || fileExists(cfg.WorkspaceFile) {
		state, err := workspace.NewWorkspaceState(cfg.WorkspaceFile, cfg.GroupID, cfg.RootID)
		if err != nil {
			return fail(err)
		}
		application.State = state
	}
	application.Source = newCredentialSource(cfg)
	settings, err := workspace.NewWebSettings(cfg.WebSettingsDir, cfg.RootName)
	if err != nil {
		return fail(err)
	}
	application.Settings = settings
	if cfg.AuthEnabled() {
		adapterAuth := adapterAuthConfig(cfg)
		application.Sessions = auth.NewStore(adapterAuth.Credentials)
	}
	rootName, err := settings.Name()
	if err != nil {
		return fail(err)
	}

	// --- global budget (upload/download slots, connection cap, spool) ---
	// The budget precedes the clients: the client's spool reservations
	// coordinate through it (D-03), so uploads can never bypass the
	// process-wide limits.
	maxConnections := cfg.MaxConnections
	if maxConnections <= 0 {
		maxConnections = config.DefaultMaxConnections
	}
	transferBudget, err := budget.New(budget.Config{
		MaxUploads:          cfg.MaxUploads,
		MaxDownloads:        cfg.MaxDownloads,
		MaxConnections:      maxConnections,
		TransferWaitTimeout: cfg.TransferWait,
		UploadSpoolMemory:   cfg.UploadSpoolMemory,
		UploadSpoolDir:      cfg.UploadSpoolDir,
		UploadMinFreeBytes:  cfg.UploadMinFreeBytes,
	})
	if err != nil {
		return fail(err)
	}
	application.Budget = transferBudget

	// --- HTTP clients (shared opener, one client per space) ---
	application.clientConfig = wpsConfig(cfg, application.Source, application.State)
	application.clientConfig.SpoolLimiter = transferBudget
	clientOptions := append([]wps.Option(nil), application.transportOptions...)
	if injected.opener == nil {
		application.opener = wps.NewControlHTTPClient(cfg.Timeout)
		clientOptions = append(clientOptions, wps.WithOpener(application.opener))
	}
	if injected.signed == nil {
		application.signedTransport = wps.NewSignedTransport(cfg.Timeout)
		clientOptions = append(clientOptions, wps.WithSignedTransport(application.signedTransport))
	}
	// Keep the exact same options for the base client and every mounted space.
	// Without these additions NewClient would silently create a private
	// transport for each client, defeating the process-wide connection pool.
	application.transportOptions = clientOptions
	client, err := wps.NewClient(application.clientConfig, clientOptions...)
	if err != nil {
		return fail(err)
	}
	application.Client = client

	// --- storage: one client per mounted space, shared resources ---
	// Python resolves the startup root from the workspace when one exists,
	// else from WPS_ROOT_ID (already defaulted by config.Load).
	singleRootID := cfg.RootID
	if application.State != nil {
		singleRootID, err = application.State.RootID()
		if err != nil {
			return fail(err)
		}
	}
	storageRootID := singleRootID
	multi, err := storage.NewMultiSpace(transferBudget, storage.MultiSpaceConfig{
		RootName:        rootName,
		SingleRootID:    storageRootID,
		MountsSource:    application.mountsSource(),
		StaticGroupID:   staticGroupID(cfg),
		SingleSelection: application.singleSelection(),
		SpaceFactory:    application.spaceFactory(),
		Space: storage.StorageConfig{
			ListCount:           cfg.ListCount,
			MaxListEntries:      cfg.MaxListEntries,
			CacheTTLSeconds:     cfg.CacheTTL,
			MaxCachedFolders:    cfg.MaxCachedFolders,
			TransferWaitTimeout: cfg.TransferWait,
			MaxCopyEntries:      cfg.MaxCopyEntries,
			MaxCopyDepth:        cfg.MaxCopyDepth,
		},
	})
	if err != nil {
		return fail(err)
	}
	application.Storage = multi

	// --- handlers ---
	locks, err := httpserver.NewDavLockStore(httpserver.DefaultLockMaxTimeout, cfg.MaxLocks)
	if err != nil {
		return fail(err)
	}
	application.Locks = locks

	limits := httpserver.ControlLimits{
		MaxControlBody:  cfg.MaxControlBody,
		MaxResponseBody: cfg.MaxResponseBody,
	}
	downloadLimits := httpserver.DownloadLimits{StreamChunkSize: cfg.StreamChunkSize}
	session, err := httpserver.NewSessionImporter(limits, cfg.BaseURL,
		selectLoginCookies, credentialReplacer(cfg),
		application.workspaceImporter(), application.roots())
	if err != nil {
		return fail(err)
	}
	rootNames, err := httpserver.NewRootNameController(settings, multi)
	if err != nil {
		return fail(err)
	}
	downloads := downloadStorage{storage: multi}
	rest, err := httpserver.NewRESTDispatcher(limits, rootNames, session,
		multi, httpserver.NewStatusController(multi, client), downloadLimits,
		downloads, multi, multi, locks, cfg.MaxUploadBytes)
	if err != nil {
		return fail(err)
	}
	if application.Sessions != nil {
		rest.SetWebAuth(application.Sessions)
	}
	rest.SetStorageLocations(application.storageLocations())
	dav, err := httpserver.NewDAVDispatcher(multi, limits,
		httpserver.DAVLimits{
			MaxPropfindEntries: cfg.MaxPropfindEntries,
			MaxPropfindDepth:   cfg.MaxPropfindDepth,
		},
		downloadLimits, downloads, multi, multi, locks, cfg.MaxUploadBytes, cfg.DAVPrefix)
	if err != nil {
		return fail(err)
	}
	application.rest = rest
	application.dav = dav
	return application, nil
}

// fileExists mirrors os.path.exists for the workspace loading condition.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// wpsConfig maps the environment configuration onto the client config.
func wpsConfig(cfg config.Config, source credentials.Source, state *workspace.WorkspaceState) wps.Config {
	return wps.Config{
		GroupID:                 cfg.GroupID,
		Workspace:               state,
		CredentialSource:        source,
		BaseURL:                 cfg.BaseURL,
		AccountBaseURL:          cfg.AccountBaseURL,
		ObjectStorageHostSuffix: cfg.ObjectSuffix,
		AutoRefresh:             cfg.AutoRefresh,
		Referer:                 cfg.Referer,
		Origin:                  cfg.Origin,
		CID:                     cfg.CID,
		Timeout:                 cfg.Timeout,
		StatusProbeTTL:          cfg.StatusProbeTTL,
		StatusFailureBackoff:    cfg.StatusFailureBackup,
		UploadSpoolMemory:       cfg.UploadSpoolMemory,
		StreamChunkSize:         cfg.StreamChunkSize,
		MultipartThreshold:      cfg.MultipartThreshold,
		MultipartPartSize:       cfg.MultipartPartSize,
		EnableRange:             cfg.EnableRange,
		UploadSpoolDir:          cfg.UploadSpoolDir,
		UploadResumeDir:         cfg.UploadResumeDir,
		UploadMinFreeBytes:      cfg.UploadMinFreeBytes,
		MaxUploadBytes:          cfg.MaxUploadBytes,
		UploadRetries:           cfg.UploadRetries,
		UploadRetryDelay:        cfg.UploadRetryDelay,
		MaxJSONResponseBytes:    cfg.MaxJSONResponse,
	}
}

// staticGroupID mirrors Python's no-workspace group check: "" and "auto"
// never count as a configured group.
func staticGroupID(cfg config.Config) string {
	if cfg.GroupID == "" || cfg.GroupID == workspace.AutoValue {
		return ""
	}
	return cfg.GroupID
}

// mountsSource feeds the hot multi-space routing from the workspace state.
// nil without a state, mirroring Python's missing workspace attribute.
func (a *Application) mountsSource() func() ([]storage.Mount, string, error) {
	if a.State == nil {
		return nil
	}
	state := a.State
	return func() ([]storage.Mount, string, error) {
		spaces, err := state.Spaces()
		if err != nil {
			return nil, "", err
		}
		groupID, err := state.GroupID()
		if err != nil {
			return nil, "", err
		}
		mounts := make([]storage.Mount, len(spaces))
		for index, mount := range spaces {
			mounts[index] = storage.Mount{Name: mount.Name, GroupID: mount.GroupID, RootID: mount.RootID}
		}
		return mounts, groupID, nil
	}
}

// singleSelection feeds the no-mounts fallback storage the live workspace
// selection; the root only follows it when WPS_ROOT_ID was "auto".
func (a *Application) singleSelection() func() (string, string, bool, error) {
	if a.State == nil {
		return nil
	}
	state := a.State
	return func() (string, string, bool, error) {
		groupID, err := state.GroupID()
		if err != nil {
			return "", "", false, err
		}
		rootID, err := state.RootID()
		if err != nil {
			return "", "", false, err
		}
		return groupID, rootID, state.ConfiguredRootID() == workspace.AutoValue, nil
	}
}

// spaceFactory builds one client per mounted space over the shared opener,
// shared signed transport, and shared credential source. The empty group id
// reuses the base client, mirroring Python's reuse of the full client for
// the single-space fallback.
func (a *Application) spaceFactory() storage.SpaceFactory {
	return func(groupID string) (storage.SpaceClients, error) {
		client := a.Client
		if groupID != "" {
			child := a.clientConfig
			child.GroupID = groupID
			child.Workspace = nil
			var err error
			client, err = wps.NewClient(child, a.transportOptions...)
			if err != nil {
				return storage.SpaceClients{}, err
			}
		}
		return storage.SpaceClients{
			Lister:     client,
			Writer:     storage.NewWriter(client),
			Downloader: storage.NewDownloader(client),
		}, nil
	}
}

// workspaceImporter applies session-imported workspace selections. nil
// without a workspace state (D-06): the route refuses workspace fields.
func (a *Application) workspaceImporter() httpserver.WorkspaceImporter {
	if a.State == nil {
		return nil
	}
	state := a.State
	return importerFunc{state: state}
}

type importerFunc struct{ state *workspace.WorkspaceState }

func (f importerFunc) Update(groupID, rootID string, spaces []workspace.Mount) (string, error) {
	if err := f.state.Update(groupID, rootID, spaces); err != nil {
		return "", err
	}
	return f.state.RootID()
}

// UpdateWithPaths preserves the optional folder labels carried by the login
// helper. The existing Update method remains the narrow session-import
// contract used by focused test doubles.
func (f importerFunc) UpdateWithPaths(groupID, rootID, rootPath string, spaces []workspace.Mount) (string, error) {
	if err := f.state.UpdateWithPaths(groupID, rootID, rootPath, spaces); err != nil {
		return "", err
	}
	return f.state.RootID()
}

// storageLocations wires the browser storage picker to the live workspace
// state and the WPS-backed storage facade. A fixed, hand-configured adapter
// has no safe runtime persistence surface, so it leaves the feature disabled.
func (a *Application) storageLocations() httpserver.StorageLocationController {
	if a.State == nil || a.Storage == nil {
		return nil
	}
	return &storageLocationController{state: a.State, storage: a.Storage}
}

type storageLocationController struct {
	state   *workspace.WorkspaceState
	storage *storage.MultiSpace
}

func (c *storageLocationController) Locations() (string, []httpserver.StorageLocation, error) {
	spaces, err := c.state.Spaces()
	if err != nil {
		return "", nil, err
	}
	if len(spaces) == 0 {
		root, err := c.storage.Root()
		if err != nil {
			return "", nil, err
		}
		rootPath, err := c.state.RootPath()
		if err != nil {
			return "", nil, err
		}
		return "single", []httpserver.StorageLocation{{Name: root.Name, Path: "/", RootPath: rootPath}}, nil
	}
	locations := make([]httpserver.StorageLocation, 0, len(spaces))
	for _, mount := range spaces {
		path, err := storage.JoinRemotePath([]string{mount.Name}, false)
		if err != nil {
			return "", nil, err
		}
		rootPath := mount.Path
		if rootPath == "" {
			rootPath = "/"
		}
		locations = append(locations, httpserver.StorageLocation{
			Name: mount.Name, Path: path, RootPath: rootPath,
		})
	}
	return "spaces", locations, nil
}

func (c *storageLocationController) Browse(path string) ([]model.RemoteEntry, error) {
	return c.storage.ListLocation(path)
}

func (c *storageLocationController) Select(path string) error {
	parts, err := storage.SplitRemotePath(path)
	if err != nil {
		return err
	}
	groupID, err := c.state.GroupID()
	if err != nil {
		return err
	}
	if groupID == "" {
		return model.NewStorageError(model.KindEntryNotFound, "WPS workspace is not configured")
	}
	spaces, err := c.state.Spaces()
	if err != nil {
		return err
	}
	if len(spaces) == 0 {
		if c.state.ConfiguredRootID() != workspace.AutoValue {
			return model.NewStorageError(model.KindUnsupportedOperation, "固定 WPS_ROOT_ID 不支持网页切换存储位置")
		}
		rootID := "0"
		rootPath := "/"
		if len(parts) > 0 {
			entry, err := c.storage.ResolveLocation(path)
			if err != nil {
				return err
			}
			if entry.Kind != model.KindFolder {
				return model.NewStorageError(model.KindNotFolder, "the selected storage location is not a folder")
			}
			rootID = entry.ID
			rootPath = path
		}
		return c.state.UpdateWithPaths(groupID, rootID, rootPath, nil)
	}
	if len(parts) == 0 {
		return model.NewStorageError(model.KindBadRequest, "请选择一个 WPS 空间和文件夹")
	}
	mountIndex := -1
	for index, mount := range spaces {
		if mount.Name == parts[0] {
			mountIndex = index
			break
		}
	}
	if mountIndex < 0 {
		return model.NewStorageError(model.KindEntryNotFound, "WPS space not found: "+parts[0])
	}
	rootID := "0"
	rootPath := "/"
	if len(parts) > 1 {
		entry, err := c.storage.ResolveLocation(path)
		if err != nil {
			return err
		}
		if entry.Kind != model.KindFolder {
			return model.NewStorageError(model.KindNotFolder, "the selected storage location is not a folder")
		}
		rootID = entry.ID
		rootPath, err = storage.JoinRemotePath(parts[1:], false)
		if err != nil {
			return err
		}
	}
	updated := append([]workspace.Mount(nil), spaces...)
	updated[mountIndex].RootID = rootID
	updated[mountIndex].Path = rootPath
	return c.state.UpdateWithPaths(groupID, "0", "/", updated)
}

// roots exposes the storage surface that follows an imported workspace.
// It must be nil exactly when the workspace importer is nil: Python never
// calls set_root_id without a workspace update.
func (a *Application) roots() httpserver.RootIDSetter {
	if a.State == nil {
		return nil
	}
	return a.Storage
}

// credentialReplacer adapts the file source to the import route. Without a
// file source every import is refused with the fixed upstream error, like
// Python's missing replace_credentials attribute.
func credentialReplacer(cfg config.Config) httpserver.CredentialReplacer {
	if cfg.CookieFile == "" && cfg.CSRFTokenFile == "" && len(cfg.RefreshCommand) == 0 {
		return refusingReplacer{}
	}
	source := credentials.NewFileCredentialSource(cfg.CookieFile, cfg.CSRFTokenFile, cfg.RefreshCommand, cfg.RefreshTimeout)
	return replacerFunc(func(cookieHeader, csrfToken string) (bool, error) {
		return source.ReplaceCredentials(credentials.Credentials{Cookie: cookieHeader, CSRFToken: csrfToken})
	})
}

type replacerFunc func(cookieHeader, csrfToken string) (bool, error)

func (f replacerFunc) ReplaceCredentials(cookieHeader, csrfToken string) (bool, error) {
	return f(cookieHeader, csrfToken)
}

type refusingReplacer struct{}

func (refusingReplacer) ReplaceCredentials(string, string) (bool, error) { return false, nil }

// selectLoginCookies adapts the credential package's cookie selection to
// the import route's shape.
func selectLoginCookies(cookies []any, baseURL string) (string, string, []string, error) {
	snapshot, err := credentials.CredentialsFromCookies(cookies, baseURL)
	if err != nil {
		return "", "", nil, err
	}
	return snapshot.Credentials.Cookie, snapshot.Credentials.CSRFToken, snapshot.Names, nil
}

// fallbackCredentialSource mirrors client._credentials: the file snapshot
// wins, the inline WPS_COOKIE / WPS_CSRF_TOKEN values fill empty fields.
// Refresh, cookie storage, and replacement only exist with a file source.
type fallbackCredentialSource struct {
	file      *credentials.FileCredentialSource
	cookie    string
	csrfToken string
}

func (s *fallbackCredentialSource) Get() (credentials.Credentials, error) {
	var current credentials.Credentials
	if s.file != nil {
		snapshot, err := s.file.Get()
		if err != nil {
			return credentials.Credentials{}, err
		}
		current = snapshot
	}
	if current.Cookie == "" && s.cookie != "" {
		current.Cookie = s.cookie
	}
	if current.CSRFToken == "" && s.csrfToken != "" {
		current.CSRFToken = s.csrfToken
	}
	return current, nil
}

func (s *fallbackCredentialSource) Refresh() (bool, error) {
	if s.file != nil {
		return s.file.Refresh()
	}
	return false, nil
}

func (s *fallbackCredentialSource) StoreSetCookieHeaders(headers http.Header) (bool, error) {
	if s.file != nil {
		return s.file.StoreSetCookieHeaders(headers)
	}
	return false, nil
}

func (s *fallbackCredentialSource) ReplaceCredentials(value credentials.Credentials) (bool, error) {
	if s.file != nil {
		return s.file.ReplaceCredentials(value)
	}
	return false, nil
}

// newCredentialSource builds the credential surface: a file source when any
// file or refresh command is configured, with the inline environment values
// as the fill-in layer Python applies inside client._credentials.
func newCredentialSource(cfg config.Config) credentials.Source {
	var file *credentials.FileCredentialSource
	if cfg.CookieFile != "" || cfg.CSRFTokenFile != "" || len(cfg.RefreshCommand) > 0 {
		file = credentials.NewFileCredentialSource(cfg.CookieFile, cfg.CSRFTokenFile, cfg.RefreshCommand, cfg.RefreshTimeout)
	}
	if file == nil && cfg.InlineCookie == "" && cfg.InlineCSRFToken == "" {
		return nil
	}
	return &fallbackCredentialSource{file: file, cookie: cfg.InlineCookie, csrfToken: cfg.InlineCSRFToken}
}

func adapterAuthConfig(cfg config.Config) httpserver.BasicAuthConfig {
	return httpserver.BasicAuthConfig{
		Username:     cfg.Username,
		Password:     cfg.Password,
		UsernameFile: cfg.UsernameFile,
		PasswordFile: cfg.PasswordFile,
		ReadSecret:   securefile.ReadSecret,
	}
}

// Handler builds the full middleware chain around the router. The health
// and web handlers never touch storage; REST and DAV errors map through
// the domain status table inside the router dispatch.
func (a *Application) Handler() (http.Handler, error) {
	router, err := httpserver.NewRouter(httpserver.RouterConfig{
		DAVPrefix:  a.Config.DAVPrefix,
		RESTPrefix: a.Config.RESTPrefix,
		Handlers: httpserver.Handlers{
			Health:   a.serveHealth,
			WebApp:   a.serveWebApp,
			WebAsset: a.serveWebAsset,
			REST:     a.rest.ServeREST,
			DAV:      a.dav.ServeDAV,
		},
	})
	if err != nil {
		return nil, err
	}
	chainConfig := httpserver.ChainConfig{
		Router: router,
		Health: a.serveHealth,
		Auth:   adapterAuthConfig(a.Config),
		Log: func(requestID, method, path string) {
			// Python's log_message prints method and sanitized path only.
			log.Printf("%s %s", method, path)
		},
		PanicLog: func(recovered any, stack []byte) {
			log.Printf("request failed: %v\n%s", recovered, stack)
		},
	}
	if a.Sessions != nil {
		chainConfig.WebAuth = &httpserver.WebAuthConfig{
			Store:      a.Sessions,
			RESTPrefix: a.Config.RESTPrefix,
		}
	}
	return httpserver.NewChain(chainConfig)
}

// DAVPrefix returns the normalized WebDAV prefix for the startup summary.
func (a *Application) DAVPrefix() string {
	return httpserver.NormalizePrefix(a.Config.DAVPrefix)
}

// RESTPrefix returns the normalized REST prefix.
func (a *Application) RESTPrefix() string {
	return httpserver.NormalizePrefix(a.Config.RESTPrefix)
}

// Close releases the shared transports. Assembly failures and process
// shutdown both call it; individual in-flight requests drain before that.
func (a *Application) Close() {
	a.closeTransports()
}

// downloadStorage adapts the storage facade to the HTTP download surface.
// The two DownloadStream interfaces are structurally identical; the wrapper
// keeps the httpserver contract self-contained for its test fakes.
type downloadStorage struct {
	storage *storage.MultiSpace
}

func (d downloadStorage) Metadata(path string) (model.RemoteEntry, error) {
	return d.storage.Metadata(path)
}

func (d downloadStorage) OpenPath(ctx context.Context, path string, offset int64, length *int64) (httpserver.DownloadStream, error) {
	stream, err := d.storage.OpenPath(ctx, path, offset, length)
	if err != nil {
		return nil, err
	}
	return downloadStream{stream}, nil
}

type downloadStream struct {
	storage.DownloadStream
}

func (a *Application) closeTransports() {
	if a.opener != nil {
		a.opener.CloseIdleConnections()
	}
	if transport, ok := a.signedTransport.(*http.Transport); ok && transport != nil {
		transport.CloseIdleConnections()
	}
}

// serveHealth mirrors _handle_health through _send_bytes: JSON with an
// explicit Content-Length and no-store. Only GET arrives here (the router
// sends every other method on /healthz to the unknown-route 404).
func (a *Application) serveHealth(w http.ResponseWriter, r *http.Request) {
	body, err := json.Marshal(healthPayload{
		Status:       "ok",
		Service:      "wps-enterprise-adapter",
		Version:      a.Version,
		NetworkCalls: "on-demand",
	})
	if err != nil {
		http.Error(w, "internal server error\n", http.StatusInternalServerError)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// serveWebApp mirrors _handle_web_app: the fixed page bytes with the CSP
// that F4 froze. The root name arrives via the settings API, never through
// template substitution.
func (a *Application) serveWebApp(w http.ResponseWriter, r *http.Request) {
	body := web.Page()
	if body == nil {
		// Unreachable with //go:embed, kept as the structural mirror of
		// Python's OSError branch.
		httpserver.SendPlainError(w, r, http.StatusNotFound, "web page is unavailable")
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("Cache-Control", web.CacheControl)
	header.Set("Content-Security-Policy", webContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// serveWebAsset mirrors _handle_web_asset: whitelisted assets only, with
// the fixed MIME types, nosniff, and no-store. Unknown names close the
// connection like Python's 404.
func (a *Application) serveWebAsset(w http.ResponseWriter, r *http.Request, name string) {
	body, contentType, ok := web.Asset(name)
	if !ok {
		httpserver.SendPlainError(w, r, http.StatusNotFound, "unknown web asset")
		return
	}
	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("Cache-Control", web.CacheControl)
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
