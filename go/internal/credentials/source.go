// Package credentials provides the credential snapshot type and the
// file-backed sources consulted before every WPS control request. Values
// never appear in errors, logs, or formatted output.
package credentials

import (
	"net/http"
	"strings"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
)

// Credentials is a point-in-time credential snapshot. The String and
// GoString hooks are redacted so accidental diagnostic output cannot reveal
// the browser session.
type Credentials struct {
	Cookie    string
	CSRFToken string
}

func (c Credentials) String() string { return "credentials(redacted)" }

func (c Credentials) GoString() string { return "credentials(redacted)" }

// Source mirrors the Python CredentialSource protocol.
type Source interface {
	Get() (Credentials, error)
	Refresh() (bool, error)
	StoreSetCookieHeaders(headers http.Header) (bool, error)
	ReplaceCredentials(credentials Credentials) (bool, error)
}

// ValidateValues applies the value-level checks client.py runs before
// credentials become outbound HTTP headers.
func ValidateValues(credentials Credentials) error {
	if err := securefile.CheckCredentialValues(credentials.Cookie, credentials.CSRFToken); err != nil {
		if securefile.CodeOf(err) == securefile.CodeTooLarge {
			return model.NewWpsAPIError("credential value is too large", 0, model.WpsCategoryUpstream)
		}
		return model.NewWpsAPIError("credential value contains a control character", 0, model.WpsCategoryUpstream)
	}
	return nil
}

// FileCredentialSource reads session values from local files on every
// request. refresh can invoke a locally configured helper or detect files
// replaced by the operator while the service runs.
type FileCredentialSource struct {
	CookiePath     string
	CSRFTokenPath  string
	RefreshCommand []string
	RefreshTimeout float64

	refreshLock sync.Mutex
	last        Credentials
	lastSet     bool
}

// NewFileCredentialSource builds the source; paths may be empty to disable
// that half.
func NewFileCredentialSource(cookiePath string, csrfTokenPath string, refreshCommand []string, refreshTimeout float64) *FileCredentialSource {
	return &FileCredentialSource{
		CookiePath:     cookiePath,
		CSRFTokenPath:  csrfTokenPath,
		RefreshCommand: refreshCommand,
		RefreshTimeout: refreshTimeout,
	}
}

// read mirrors FileCredentialSource._read: an unset path is an empty value,
// anything else must be a safe credential file. Failures collapse to the
// Python WpsApiError("read credential file") without path or content.
func (s *FileCredentialSource) read(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	value, err := securefile.ReadSecret(path)
	if err != nil {
		return "", model.NewWpsAPIError("read credential file", 0, model.WpsCategoryUpstream)
	}
	return value, nil
}

func (s *FileCredentialSource) snapshot() (Credentials, error) {
	cookie, err := s.read(s.CookiePath)
	if err != nil {
		return Credentials{}, err
	}
	csrfToken, err := s.read(s.CSRFTokenPath)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Cookie: cookie, CSRFToken: csrfToken}, nil
}

// Get returns a freshly read snapshot; every WPS control request consults
// the current files.
func (s *FileCredentialSource) Get() (Credentials, error) {
	credentials, err := s.snapshot()
	if err != nil {
		return Credentials{}, err
	}
	s.refreshLock.Lock()
	s.last = credentials
	s.lastSet = true
	s.refreshLock.Unlock()
	return credentials, nil
}

// writeAtomic applies the credential write discipline. Python surfaces the
// parent problems as WpsApiError("write credential file") and the tighten
// step as WpsApiError("protect credential directory"); later OS failures
// stay in the same category here.
func (s *FileCredentialSource) writeAtomic(path string, value string) error {
	if err := securefile.WriteCredentialAtomic(path, value); err != nil {
		if securefile.CodeOf(err) == securefile.CodeChmodDir {
			return model.NewWpsAPIError("protect credential directory", 0, model.WpsCategoryUpstream)
		}
		return model.NewWpsAPIError("write credential file", 0, model.WpsCategoryUpstream)
	}
	return nil
}

// StoreSetCookieHeaders persists WPS session-cookie rotation without
// exposing cookie values, keeping the CSRF file in sync when a csrf cookie
// rotates.
func (s *FileCredentialSource) StoreSetCookieHeaders(headers http.Header) (bool, error) {
	if s.CookiePath == "" {
		return false, nil
	}
	setCookieHeaders := headers.Values("Set-Cookie")
	if len(setCookieHeaders) == 0 {
		return false, nil
	}

	updates, csrfSeen, csrfValue := parseSetCookieUpdates(setCookieHeaders)
	if len(updates) == 0 {
		return false, nil
	}

	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()
	current, err := s.snapshot()
	if err != nil {
		return false, err
	}
	values, order := cookieMap(current.Cookie)
	positions := make(map[string]string, len(order))
	for _, name := range order {
		positions[strings.ToLower(name)] = name
	}
	for name, value := range updates {
		storedName := positions[strings.ToLower(name)]
		if storedName == "" {
			storedName = name
			positions[strings.ToLower(name)] = storedName
			order = append(order, storedName)
		}
		if value == nil {
			delete(values, storedName)
			kept := order[:0]
			for _, item := range order {
				if item != storedName {
					kept = append(kept, item)
				}
			}
			order = kept
			delete(positions, strings.ToLower(name))
		} else {
			values[storedName] = *value
		}
	}
	newCookie := joinCookie(values, order)
	cookieChanged := newCookie != current.Cookie
	csrfChanged := csrfSeen && s.CSRFTokenPath != "" && csrfValue != current.CSRFToken
	if !cookieChanged && !csrfChanged {
		return false, nil
	}
	if cookieChanged {
		if err := s.writeAtomic(s.CookiePath, newCookie); err != nil {
			return false, err
		}
	}
	if csrfChanged && s.CSRFTokenPath != "" {
		if err := s.writeAtomic(s.CSRFTokenPath, csrfValue); err != nil {
			return false, err
		}
	}
	fresh, err := s.snapshot()
	if err != nil {
		return false, err
	}
	s.last = fresh
	s.lastSet = true
	return true, nil
}

// ReplaceCredentials swaps the pair after a local interactive login. A
// failure after the first write rolls both halves back to the previous
// snapshot before returning the error.
func (s *FileCredentialSource) ReplaceCredentials(credentials Credentials) (bool, error) {
	if s.CookiePath == "" || s.CSRFTokenPath == "" {
		return false, nil
	}
	if credentials.Cookie == "" || credentials.CSRFToken == "" {
		return false, nil
	}
	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()
	current, err := s.snapshot()
	if err != nil {
		return false, err
	}
	if current == credentials {
		s.last = current
		s.lastSet = true
		return true, nil
	}
	if err := securefile.WriteCredentialPair(s.CookiePath, credentials.Cookie, s.CSRFTokenPath, credentials.CSRFToken); err != nil {
		return false, pairWriteErr(err)
	}
	s.last = credentials
	s.lastSet = true
	return true, nil
}

// pairWriteErr maps a failed pair write to the Python WpsApiError wording.
func pairWriteErr(err error) error {
	if securefile.CodeOf(err) == securefile.CodeChmodDir {
		return model.NewWpsAPIError("protect credential directory", 0, model.WpsCategoryUpstream)
	}
	return model.NewWpsAPIError("write credential file", 0, model.WpsCategoryUpstream)
}

// StaticCredentialSource is a small adapter useful for embedding and tests.
type StaticCredentialSource struct {
	Credentials Credentials
}

func (s *StaticCredentialSource) Get() (Credentials, error) {
	return s.Credentials, nil
}

func (s *StaticCredentialSource) Refresh() (bool, error) {
	return false, nil
}

func (s *StaticCredentialSource) StoreSetCookieHeaders(_ http.Header) (bool, error) {
	return false, nil
}

func (s *StaticCredentialSource) ReplaceCredentials(_ Credentials) (bool, error) {
	return false, nil
}
