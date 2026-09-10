package httpserver

import (
	"net/http"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// maxImportBodyBytes mirrors _do_rest_session_import's explicit body cap:
// 512 KiB regardless of the general control-body limit.
const maxImportBodyBytes = 512 * 1024

// maxImportCookies mirrors the cookie array bound.
const maxImportCookies = 256

// CookieSelector validates and selects browser cookies into a credential
// snapshot. The app assembly injects credentials.CredentialsFromCookies —
// the dependency rules keep httpserver away from the credentials package,
// while the server-side route still re-validates every field itself.
type CookieSelector func(cookies []any, baseURL string) (cookieHeader, csrfToken string, names []string, err error)

// CredentialReplacer swaps the stored credential pair. A false result or an
// error both fail the import with a fixed redacted upstream error.
type CredentialReplacer interface {
	ReplaceCredentials(cookieHeader, csrfToken string) (bool, error)
}

// RootIDSetter is the storage surface that follows an imported workspace
// selection (Python's storage.set_root_id call).
type RootIDSetter interface {
	SetRootID(rootID string) error
}

// WorkspaceImporter applies an imported workspace selection and returns the
// fresh root id. A nil importer refuses workspace fields entirely (D-06:
// only auto-configured or file-backed setups carry a workspace state).
type WorkspaceImporter interface {
	Update(groupID, rootID string, spaces []workspace.Mount) (freshRoot string, err error)
}

// WorkspacePathImporter is implemented by the current application state so
// login can persist the human-readable folder selected by the helper. The
// small optional interface keeps focused test importers and older adapters
// usable for root-only payloads.
type WorkspacePathImporter interface {
	UpdateWithPaths(groupID, rootID, rootPath string, spaces []workspace.Mount) (freshRoot string, err error)
}

type WorkspaceModeImporter interface {
	UpdateWithPathsMode(groupID, rootID, rootPath string, spaces []workspace.Mount, mode string) (freshRoot string, err error)
}

// SessionImporter implements POST /api/v1/session/import. All input is
// validated into a plan before the first file write: cookies are selected
// and checked, the workspace payload is fully validated, and only then does
// the credential pair get replaced followed by the workspace update — the
// Python order, with every failure answered by fixed redacted errors.
type SessionImporter struct {
	limits    ControlLimits
	baseURL   string
	cookies   CookieSelector
	sources   CredentialReplacer
	workspace WorkspaceImporter
	roots     RootIDSetter
}

// NewSessionImporter wires the import route. The workspace surfaces may
// both be nil (non-auto configuration), but must be provided together.
func NewSessionImporter(limits ControlLimits, baseURL string, cookies CookieSelector, sources CredentialReplacer, workspaceImport WorkspaceImporter, roots RootIDSetter) (*SessionImporter, error) {
	if cookies == nil {
		return nil, errChainConfig("a cookie selector is required")
	}
	if sources == nil {
		return nil, errChainConfig("a credential replacer is required")
	}
	if (workspaceImport == nil) != (roots == nil) {
		return nil, errChainConfig("workspace import needs both the state and the storage root")
	}
	if baseURL == "" {
		baseURL = "https://365.kdocs.cn"
	}
	if limits.MaxControlBody <= 0 || limits.MaxResponseBody <= 0 {
		limits = DefaultControlLimits()
	}
	return &SessionImporter{
		limits:    limits,
		baseURL:   baseURL,
		cookies:   cookies,
		sources:   sources,
		workspace: workspaceImport,
		roots:     roots,
	}, nil
}

// sessionImportPayload keeps the Python response key order (status,
// cookie_count, workspace).
type sessionImportPayload struct {
	Status      string `json:"status"`
	CookieCount int    `json:"cookie_count"`
	Workspace   string `json:"workspace,omitempty"`
}

// Import mirrors _do_rest_session_import.
func (s *SessionImporter) Import(w http.ResponseWriter, r *http.Request) error {
	payload, err := readJSONBodyLimit(w, r, maxImportBodyBytes)
	if err != nil || payload == nil {
		return err
	}
	rawCookies, present := payload["cookies"]
	cookieList, ok := rawCookies.([]any)
	if !present || !ok || len(cookieList) == 0 {
		return errBadRequest("JSON field 'cookies' must be a non-empty array")
	}
	if len(cookieList) > maxImportCookies {
		return errBadRequest("too many cookies")
	}

	// The workspace plan is validated completely before any write (D-06).
	var mounts []workspace.Mount
	workspaceRequested := false
	var groupID, rootID, rootPath, mode string
	if rawWorkspace, present := payload["workspace"]; present && rawWorkspace != nil {
		workspaceRequested = true
		workspaceMap, ok := rawWorkspace.(map[string]any)
		if !ok {
			return errBadRequest("JSON field 'workspace' must be an object")
		}
		if s.workspace == nil {
			return errBadRequest("workspace import requires WPS_GROUP_ID=auto or WPS_ROOT_ID=auto")
		}
		if groupID, err = identifierField(workspaceMap, "group_id", "workspace.group_id", ""); err != nil {
			return err
		}
		if rootID, err = identifierField(workspaceMap, "root_id", "workspace.root_id", "0"); err != nil {
			return err
		}
		rootPath = "/"
		mode = workspace.ModeAuto
		if rawMode, present := workspaceMap["mode"]; present {
			var ok bool
			mode, ok = rawMode.(string)
			if !ok || (mode != workspace.ModeAuto && mode != workspace.ModeBusiness && mode != workspace.ModePersonal) {
				return errBadRequest("workspace.mode is invalid")
			}
		}
		if rawPath, present := workspaceMap["root_path"]; present {
			var ok bool
			rootPath, ok = rawPath.(string)
			if !ok {
				return errBadRequest("workspace.root_path is invalid")
			}
			if err := workspace.ValidateSelectionPath(rootPath, "workspace.root_path"); err != nil {
				return errBadRequest(err.Error())
			}
		}
		rawSpaces, present := workspaceMap["spaces"]
		if present && rawSpaces != nil {
			spaces, ok := rawSpaces.([]any)
			if !ok || len(spaces) == 0 || len(spaces) > workspace.MaxSpaces {
				return errBadRequest("JSON field 'workspace.spaces' is invalid")
			}
			seenNames := map[string]struct{}{}
			seenGroups := map[string]struct{}{}
			for _, item := range spaces {
				spaceMap, ok := item.(map[string]any)
				if !ok {
					return errBadRequest("workspace space must be an object")
				}
				mount, err := buildImportMount(spaceMap)
				if err != nil {
					return err
				}
				if _, duplicate := seenNames[mount.Name]; duplicate {
					return errBadRequest("workspace spaces contain duplicate names")
				}
				if _, duplicate := seenGroups[mount.GroupID]; duplicate {
					return errBadRequest("workspace spaces contain duplicate groups")
				}
				seenNames[mount.Name] = struct{}{}
				seenGroups[mount.GroupID] = struct{}{}
				mounts = append(mounts, mount)
			}
		}
	}

	cookieHeader, csrfToken, names, err := s.cookies(cookieList, s.baseURL)
	if err != nil {
		// LoginError and friends carry user-facing wording; Python's
		// RuntimeError falls through to the fixed 500, and so does this.
		return err
	}
	replaced, err := s.sources.ReplaceCredentials(cookieHeader, csrfToken)
	if err != nil || !replaced {
		return model.NewWpsAPIError("store imported credentials", 0, model.WpsCategoryUpstream)
	}
	responsePayload := sessionImportPayload{Status: "ok", CookieCount: len(names)}
	if workspaceRequested {
		var freshRoot string
		if importer, ok := s.workspace.(WorkspaceModeImporter); ok {
			freshRoot, err = importer.UpdateWithPathsMode(groupID, rootID, rootPath, mounts, mode)
		} else if importer, ok := s.workspace.(WorkspacePathImporter); ok {
			freshRoot, err = importer.UpdateWithPaths(groupID, rootID, rootPath, mounts)
		} else {
			if rootPath != "/" {
				return model.NewWpsAPIError("store imported workspace", 0, model.WpsCategoryUpstream)
			}
			freshRoot, err = s.workspace.Update(groupID, rootID, mounts)
		}
		if err != nil {
			return model.NewWpsAPIError("store imported workspace", 0, model.WpsCategoryUpstream)
		}
		if err := s.roots.SetRootID(freshRoot); err != nil {
			return err
		}
		responsePayload.Workspace = "updated"
	}
	return sendJSON(w, r, http.StatusOK, responsePayload, s.limits, nil)
}

// identifierField mirrors validate_workspace_identifier over an untyped JSON
// value with an optional default for a missing key. The error text matches
// Python's WorkspaceConfigError surfaced through ValueError.
func identifierField(payload map[string]any, key string, fieldName string, missing string) (string, error) {
	value, present := payload[key]
	if !present {
		if missing == "" {
			return "", errBadRequest(fieldName + " is invalid")
		}
		value = missing
	}
	text, ok := value.(string)
	if !ok {
		return "", errBadRequest(fieldName + " is invalid")
	}
	if err := workspace.ValidateIdentifier(text, fieldName); err != nil {
		return "", errBadRequest(err.Error())
	}
	return text, nil
}

// buildImportMount mirrors the WorkspaceMount construction inside
// _do_rest_session_import: group_id required, root_id defaulting to "0",
// name defaulting to the group id.
func buildImportMount(spaceMap map[string]any) (workspace.Mount, error) {
	groupID, err := identifierField(spaceMap, "group_id", "space.group_id", "")
	if err != nil {
		return workspace.Mount{}, err
	}
	rootID, err := identifierField(spaceMap, "root_id", "space.root_id", "0")
	if err != nil {
		return workspace.Mount{}, err
	}
	nameValue, present := spaceMap["name"]
	var name string
	if !present {
		name = groupID
	} else if text, ok := nameValue.(string); ok {
		name = text
	} else {
		// Python str()-coerces; the mount name check rejects anything that
		// is not a usable name, so keep the coercion minimal.
		return workspace.Mount{}, errBadRequest("space.name is invalid")
	}
	path := "/"
	if rawPath, present := spaceMap["path"]; present {
		var ok bool
		path, ok = rawPath.(string)
		if !ok {
			return workspace.Mount{}, errBadRequest("space.path is invalid")
		}
		if err := workspace.ValidateSelectionPath(path, "space.path"); err != nil {
			return workspace.Mount{}, errBadRequest(err.Error())
		}
	}
	mount, err := workspace.NewMountWithPath(groupID, rootID, name, path)
	if err != nil {
		return workspace.Mount{}, errBadRequest(err.Error())
	}
	return mount, nil
}
