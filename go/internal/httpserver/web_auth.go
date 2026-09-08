package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
)

// WebAuthController exposes the local browser account flow. The account
// store is optional at the dispatcher level so the older REST test fixtures
// can continue to exercise file operations without setting up users.
type WebAuthController struct {
	store               *auth.Store
	registrationEnabled bool
}

// SetWebAuth enables the browser login/register endpoints on a dispatcher.
func (d *RESTDispatcher) SetWebAuth(store *auth.Store, registrationEnabled bool) {
	d.webAuth = &WebAuthController{store: store, registrationEnabled: registrationEnabled}
}

type authUserPayload struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	CreatedAt int64  `json:"created_at"`
}

type authMePayload struct {
	Authenticated       bool             `json:"authenticated"`
	RegistrationEnabled bool             `json:"registration_enabled"`
	User                *authUserPayload `json:"user,omitempty"`
}

type authConfigPayload struct {
	RegistrationEnabled bool `json:"registration_enabled"`
	UserCount           int  `json:"user_count"`
}

func (d *RESTDispatcher) serveWebAuth(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if d.webAuth == nil || d.webAuth.store == nil {
		return sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "web authentication is unavailable"}, d.limits, nil)
	}
	switch route.Suffix {
	case "auth/me":
		if r.Method != http.MethodGet {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		user, ok := d.webAuth.currentUser(r)
		payload := authMePayload{Authenticated: ok, RegistrationEnabled: d.webAuth.registrationEnabled}
		if ok {
			payload.User = publicAuthUser(user)
		}
		return sendJSON(w, r, http.StatusOK, payload, d.limits, nil)
	case "auth/config":
		if r.Method != http.MethodGet {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return sendJSON(w, r, http.StatusOK, authConfigPayload{
			RegistrationEnabled: d.webAuth.registrationEnabled,
			UserCount:           d.webAuth.store.UserCount(),
		}, d.limits, nil)
	case "auth/login":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.login(w, r)
	case "auth/register":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.register(w, r)
	case "auth/logout":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		d.webAuth.store.Logout(sessionToken(r))
		http.SetCookie(w, expiredSessionCookie(r))
		return sendJSON(w, r, http.StatusOK, map[string]string{"status": "ok"}, d.limits, nil)
	default:
		return sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "unknown REST route"}, d.limits, nil)
	}
}

func (d *RESTDispatcher) login(w http.ResponseWriter, r *http.Request) error {
	payload, err := readAuthJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	username, password, ok := accountFields(payload)
	if !ok {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "请输入用户名和密码")
	}
	token, user, err := d.webAuth.store.Login(username, password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		return sendAuthError(w, r, http.StatusUnauthorized, "auth_invalid_credentials", "用户名或密码错误")
	}
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_login_failed", "登录失败，请稍后重试")
	}
	http.SetCookie(w, sessionCookie(r, token))
	return sendJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "user": publicAuthUser(user)}, d.limits, nil)
}

func (d *RESTDispatcher) register(w http.ResponseWriter, r *http.Request) error {
	if !d.webAuth.registrationEnabled {
		return sendAuthError(w, r, http.StatusForbidden, "auth_registration_disabled", "注册功能已关闭")
	}
	payload, err := readAuthJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	username, password, ok := accountFields(payload)
	if !ok {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "请输入用户名和密码")
	}
	user, err := d.webAuth.store.Register(username, password)
	if errors.Is(err, auth.ErrUsernameTaken) {
		return sendAuthError(w, r, http.StatusConflict, "auth_username_taken", "用户名已存在")
	}
	if errors.Is(err, auth.ErrInvalidInput) {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "用户名需为 3-64 位，密码至少 8 位")
	}
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_register_failed", "注册失败，请稍后重试")
	}
	// Registration is immediately useful: the new account is signed in so a
	// first-time visitor never has to repeat the same credentials.
	token, _, err := d.webAuth.store.Login(username, password)
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_login_failed", "注册成功，但自动登录失败")
	}
	http.SetCookie(w, sessionCookie(r, token))
	return sendJSON(w, r, http.StatusCreated, map[string]any{"status": "ok", "user": publicAuthUser(user)}, d.limits, nil)
}

func accountFields(payload map[string]any) (string, string, bool) {
	username, usernameOK := payload["username"].(string)
	password, passwordOK := payload["password"].(string)
	return strings.TrimSpace(username), password, usernameOK && passwordOK && len(payload) == 2
}

// readAuthJSONBody accepts the parsed ContentLength that net/http keeps on a
// real browser request after removing Content-Length from Header. The legacy
// REST contract remains strict; only the browser account endpoints need this
// transport-level compatibility.
func readAuthJSONBody(w http.ResponseWriter, r *http.Request, limits ControlLimits) (map[string]any, error) {
	if r.ContentLength > 0 && len(r.Header.Values("Content-Length")) == 0 {
		if r.ContentLength > limits.MaxControlBody {
			return nil, errRequestBodyTooLarge()
		}
		body := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, body); err != nil {
			return nil, errBadRequestClose("request body is shorter than Content-Length")
		}
		var decoded any
		if err := json.Unmarshal(body, &decoded); err != nil {
			return nil, errBadRequest("request body must be valid JSON")
		}
		payload, ok := decoded.(map[string]any)
		if !ok {
			return nil, errBadRequest("request body must be a JSON object")
		}
		return payload, nil
	}
	// Keep malformed and explicitly-headered requests on the established
	// bounded parser, including its exact 411/400 responses.
	return readJSONBody(w, r, limits)
}

func publicAuthUser(user auth.User) *authUserPayload {
	return &authUserPayload{ID: user.ID, Username: user.Username, CreatedAt: user.CreatedAt}
}

func (a *WebAuthController) currentUser(r *http.Request) (auth.User, bool) {
	return a.store.Current(sessionToken(r))
}

func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func sessionCookie(r *http.Request, token string) *http.Cookie {
	return &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(auth.SessionMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	}
}

func expiredSessionCookie(r *http.Request) *http.Cookie {
	cookie := sessionCookie(r, "")
	cookie.MaxAge = -1
	cookie.Expires = time.Unix(1, 0)
	return cookie
}

func sendAuthError(w http.ResponseWriter, r *http.Request, status int, code, message string) error {
	return sendJSON(w, r, status, map[string]string{"error": message, "code": code}, DefaultControlLimits(), nil)
}
