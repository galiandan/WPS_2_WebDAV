package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
)

// WebAuthController exposes the one-account browser login flow. The account
// credentials are the same credentials used by WebDAV Basic Auth.
type WebAuthController struct {
	store *auth.Store
}

// SetWebAuth enables the browser login/logout endpoints on a dispatcher.
func (d *RESTDispatcher) SetWebAuth(store *auth.Store) {
	d.webAuth = &WebAuthController{store: store}
}

type authUserPayload struct {
	Username string `json:"username"`
}

type authMePayload struct {
	Authenticated bool             `json:"authenticated"`
	User          *authUserPayload `json:"user,omitempty"`
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
		payload := authMePayload{Authenticated: ok}
		if ok {
			payload.User = publicAuthUser(user)
		}
		return sendJSON(w, r, http.StatusOK, payload, d.limits, nil)
	case "auth/security":
		if r.Method != http.MethodGet {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		if _, ok := d.requireUser(w, r); !ok {
			return nil
		}
		return sendJSON(w, r, http.StatusOK, map[string]any{
			"status": "ok", "totp_enabled": d.webAuth.store.TwoFactorEnabled(),
			"passkeys": d.webAuth.store.Passkeys(),
		}, d.limits, nil)
	case "auth/login":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.login(w, r)
	case "auth/2fa/verify":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.verifyTwoFactor(w, r)
	case "auth/passkey/options":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.passkeyLoginOptions(w, r)
	case "auth/passkey/verify":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.verifyPasskeyLogin(w, r)
	case "auth/passkey/register/options":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.passkeyRegistrationOptions(w, r)
	case "auth/passkey/register":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.registerPasskey(w, r)
	case "auth/passkey/delete":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.deletePasskey(w, r)
	case "auth/totp/setup":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.totpSetup(w, r)
	case "auth/totp/enable":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.totpEnable(w, r)
	case "auth/totp/disable":
		if r.Method != http.MethodPost {
			return sendAuthError(w, r, http.StatusMethodNotAllowed, "auth_method_not_allowed", "method not allowed")
		}
		return d.totpDisable(w, r)
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
	login, err := d.webAuth.store.LoginWithFactors(username, password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		return sendAuthError(w, r, http.StatusUnauthorized, "auth_invalid_credentials", "用户名或密码错误")
	}
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_login_failed", "登录失败，请稍后重试")
	}
	if login.TwoFactorRequired {
		return sendJSON(w, r, http.StatusOK, map[string]any{"status": "two_factor_required", "challenge": login.Challenge, "methods": []string{"totp"}}, d.limits, nil)
	}
	http.SetCookie(w, sessionCookie(r, login.Token))
	return sendJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "user": publicAuthUser(login.User)}, d.limits, nil)
}

func (d *RESTDispatcher) verifyTwoFactor(w http.ResponseWriter, r *http.Request) error {
	payload, err := readAuthJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	challenge, challengeOK := payload["challenge"].(string)
	code, codeOK := payload["code"].(string)
	if !challengeOK || !codeOK || len(payload) != 2 || strings.TrimSpace(challenge) == "" || strings.TrimSpace(code) == "" {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "请输入验证码")
	}
	token, user, err := d.webAuth.store.VerifyTwoFactor(challenge, code)
	if errors.Is(err, auth.ErrFactorChallengeExpired) {
		return sendAuthError(w, r, http.StatusUnauthorized, "auth_challenge_expired", "登录验证码已过期，请重新输入密码")
	}
	if errors.Is(err, auth.ErrInvalidTwoFactor) {
		return sendAuthError(w, r, http.StatusUnauthorized, "auth_invalid_factor", "验证码错误或已过期")
	}
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_login_failed", "登录失败，请稍后重试")
	}
	http.SetCookie(w, sessionCookie(r, token))
	return sendJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "user": publicAuthUser(user)}, d.limits, nil)
}

func (d *RESTDispatcher) totpSetup(w http.ResponseWriter, r *http.Request) error {
	user, ok := d.requireUser(w, r)
	if !ok {
		return nil
	}
	setup, err := d.webAuth.store.BeginTOTPSetup(user.Username)
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_factor_failed", "无法生成验证器密钥")
	}
	return sendJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "secret": setup.Secret, "otpauth_uri": setup.OTPAuthURI}, d.limits, nil)
}

func (d *RESTDispatcher) totpEnable(w http.ResponseWriter, r *http.Request) error {
	if _, ok := d.requireUser(w, r); !ok {
		return nil
	}
	payload, err := readAuthJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	secret, secretOK := payload["secret"].(string)
	code, codeOK := payload["code"].(string)
	if !secretOK || !codeOK || len(payload) != 2 {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "请输入验证码")
	}
	recovery, err := d.webAuth.store.EnableTOTP(secret, code)
	if errors.Is(err, auth.ErrInvalidTwoFactor) {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_factor", "验证码错误")
	}
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_factor_failed", "无法启用两步验证")
	}
	return sendJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "recovery_codes": recovery}, d.limits, nil)
}

func (d *RESTDispatcher) totpDisable(w http.ResponseWriter, r *http.Request) error {
	if _, ok := d.requireUser(w, r); !ok {
		return nil
	}
	payload, err := readAuthJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	code, codeOK := payload["code"].(string)
	if !codeOK || len(payload) != 1 {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "请输入验证码")
	}
	if err := d.webAuth.store.DisableTOTP(code); errors.Is(err, auth.ErrInvalidTwoFactor) {
		return sendAuthError(w, r, http.StatusUnauthorized, "auth_invalid_factor", "验证码错误")
	} else if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_factor_failed", "无法关闭两步验证")
	}
	return sendJSON(w, r, http.StatusOK, map[string]string{"status": "ok"}, d.limits, nil)
}

func (d *RESTDispatcher) passkeyLoginOptions(w http.ResponseWriter, r *http.Request) error {
	_, rpID, _ := passkeyRequestContext(r)
	options, err := d.webAuth.store.BeginPasskeyLogin(rpID)
	if errors.Is(err, auth.ErrPasskeyNotConfigured) {
		return sendAuthError(w, r, http.StatusNotFound, "passkey_not_configured", "尚未注册 Passkey")
	}
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_factor_failed", "无法创建 Passkey 登录请求")
	}
	return sendJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "challenge": options.Challenge, "publicKey": options}, d.limits, nil)
}

func (d *RESTDispatcher) verifyPasskeyLogin(w http.ResponseWriter, r *http.Request) error {
	payload, err := readAuthJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	challenge, ok := payload["challenge"].(string)
	if !ok || strings.TrimSpace(challenge) == "" {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "登录请求已失效")
	}
	credential, err := credentialFromPayload(payload["credential"])
	if err != nil {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "Passkey 数据无效")
	}
	origin, rpID, err := passkeyRequestContext(r)
	if err != nil {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_origin", "当前地址不支持 Passkey")
	}
	token, user, err := d.webAuth.store.VerifyPasskeyLogin(challenge, credential, rpID, origin)
	if errors.Is(err, auth.ErrFactorChallengeExpired) || errors.Is(err, auth.ErrInvalidPasskey) {
		return sendAuthError(w, r, http.StatusUnauthorized, "auth_invalid_passkey", "Passkey 验证失败，请重试")
	}
	if err != nil {
		return sendAuthError(w, r, http.StatusInternalServerError, "auth_login_failed", "登录失败，请稍后重试")
	}
	http.SetCookie(w, sessionCookie(r, token))
	return sendJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "user": publicAuthUser(user)}, d.limits, nil)
}

func (d *RESTDispatcher) passkeyRegistrationOptions(w http.ResponseWriter, r *http.Request) error {
	user, ok := d.requireUser(w, r)
	if !ok {
		return nil
	}
	_, rpID, err := passkeyRequestContext(r)
	if err != nil {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_origin", "当前地址不支持 Passkey")
	}
	options, err := d.webAuth.store.BeginPasskeyRegistration(sessionToken(r), user.Username, rpID)
	if err != nil {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_factor_failed", "无法创建 Passkey 注册请求")
	}
	return sendJSON(w, r, http.StatusOK, map[string]any{"status": "ok", "challenge": options.Challenge, "publicKey": options}, d.limits, nil)
}

func (d *RESTDispatcher) registerPasskey(w http.ResponseWriter, r *http.Request) error {
	if _, ok := d.requireUser(w, r); !ok {
		return nil
	}
	payload, err := readAuthJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	challenge, challengeOK := payload["challenge"].(string)
	name, _ := payload["name"].(string)
	credential, err := credentialFromPayload(payload["credential"])
	if !challengeOK || err != nil {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "Passkey 数据无效")
	}
	origin, rpID, err := passkeyRequestContext(r)
	if err != nil {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_origin", "当前地址不支持 Passkey")
	}
	if err := d.webAuth.store.RegisterPasskey(sessionToken(r), challenge, credential, name, rpID, origin); errors.Is(err, auth.ErrInvalidPasskey) || errors.Is(err, auth.ErrFactorChallengeExpired) {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_passkey", "Passkey 注册失败，请重试")
	} else if err != nil {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_factor_failed", "Passkey 注册失败")
	}
	return sendJSON(w, r, http.StatusOK, map[string]string{"status": "ok"}, d.limits, nil)
}

func (d *RESTDispatcher) deletePasskey(w http.ResponseWriter, r *http.Request) error {
	if _, ok := d.requireUser(w, r); !ok {
		return nil
	}
	payload, err := readAuthJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	id, ok := payload["id"].(string)
	if !ok || id == "" || len(payload) != 1 {
		return sendAuthError(w, r, http.StatusBadRequest, "auth_invalid_input", "Passkey 数据无效")
	}
	if err := d.webAuth.store.DeletePasskey(id); err != nil {
		return sendAuthError(w, r, http.StatusNotFound, "passkey_not_found", "Passkey 不存在")
	}
	return sendJSON(w, r, http.StatusOK, map[string]string{"status": "ok"}, d.limits, nil)
}

func (d *RESTDispatcher) requireUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	user, ok := d.webAuth.currentUser(r)
	if !ok {
		_ = sendAuthError(w, r, http.StatusUnauthorized, "auth_required", "请先登录")
	}
	return user, ok
}

func credentialFromPayload(value any) (auth.PasskeyCredential, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > maxAuthCredentialBytes {
		return auth.PasskeyCredential{}, errors.New("invalid credential")
	}
	var credential auth.PasskeyCredential
	if err := json.Unmarshal(raw, &credential); err != nil || credential.ID == "" || credential.Response.ClientDataJSON == "" {
		return auth.PasskeyCredential{}, errors.New("invalid credential")
	}
	return credential, nil
}

const maxAuthCredentialBytes = 128 * 1024

func passkeyRequestContext(r *http.Request) (origin, rpID string, err error) {
	host := r.Host
	if host == "" {
		return "", "", errors.New("missing host")
	}
	if originHeader := strings.TrimSpace(r.Header.Get("Origin")); originHeader != "" && originHeader != "null" {
		parsed, parseErr := url.Parse(originHeader)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host != host || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", errors.New("invalid origin")
		}
		origin = parsed.Scheme + "://" + parsed.Host
	} else {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		origin = scheme + "://" + host
	}
	rpID = host
	if parsedHost, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		rpID = parsedHost
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		rpID = strings.Trim(host, "[]")
	}
	if rpID == "" {
		return "", "", errors.New("invalid relying party")
	}
	return origin, rpID, nil
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
	return &authUserPayload{Username: user.Username}
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
