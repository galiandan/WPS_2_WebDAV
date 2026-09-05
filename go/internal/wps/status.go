// The status layer ports client.py's check_status: a cached, redacted
// login-and-workspace status built from the read-only islogin preflight and
// a count=1 root listing. Per decision D-02 the whole status path never
// triggers the refresh-token grant, and the returned value carries only the
// fixed redacted fields — never cookies, IDs, or upstream bodies.

package wps

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// statusCacheTTLValue and friends live on Config (StatusProbeTTL,
// StatusFailureBackoff); the probe wait deadline mirrors
// max(timeout, 1.0) + 1.0 seconds.

func statusValue(status string, wps string, workspaceState string, accountType string, checkedAt int) model.WpsStatus {
	return model.WpsStatus{
		Status:        status,
		Wps:           wps,
		Workspace:     workspaceState,
		AccountType:   accountType,
		LastCheckedAt: &checkedAt,
	}
}

func notConfiguredStatus(checkedAt int) model.WpsStatus {
	return statusValue("not_configured", "not_configured", "not_configured", "unknown", checkedAt)
}

func invalidResponseCredentialsStatus(checkedAt int) model.WpsStatus {
	return statusValue("invalid_response", "unknown", "unknown", "unknown", checkedAt)
}

func upstreamUnavailableStatus(checkedAt int, retryAfter int) model.WpsStatus {
	return statusValue("upstream_unavailable", "unknown", "unknown", "unknown", checkedAt).
		WithRetryAfter(retryAfter)
}

// statusCredentialsAreMissing mirrors _status_credentials_are_missing: a
// file-backed source with any unreadable path counts as not configured.
func (c *Client) statusCredentialsAreMissing() bool {
	if source, ok := c.config.CredentialSource.(*credentials.FileCredentialSource); ok {
		for _, path := range []string{source.CookiePath, source.CSRFTokenPath} {
			if path == "" {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				return true
			}
		}
		return false
	}
	return c.config.CredentialSource == nil
}

// statusTruth mirrors _status_truth: (value, false) models Python's None.
func statusTruth(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return false, false
		}
		return parsed != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on", "ok", "success", "logged_in":
			return true, true
		case "0", "false", "no", "off", "logout", "logged_out":
			return false, true
		}
		return false, false
	}
	return false, false
}

// statusAccountType mirrors _status_account_type: a coarse business/personal
// classification from boolean markers or company identifiers only.
func statusAccountType(payload map[string]any) string {
	for _, key := range []string{"is_company_account", "is_business_account"} {
		raw, present := payload[key]
		if !present {
			continue
		}
		marker, known := statusTruth(raw)
		if !known {
			continue
		}
		if marker {
			return "business"
		}
		return "personal"
	}
	for _, key := range []string{"companyid", "current_companyid", "company_id"} {
		value, present := payload[key]
		if !present {
			continue
		}
		switch typed := value.(type) {
		case nil:
			continue
		case bool:
			if !typed {
				continue
			}
		case json.Number:
			if parsed, err := typed.Float64(); err == nil && parsed == 0 {
				continue
			}
		case string:
			trimmed := strings.TrimSpace(typed)
			if trimmed == "" || trimmed == "0" {
				continue
			}
		}
		return "business"
	}
	return "unknown"
}

// loginPreflight mirrors _login_preflight: the account islogin probe with
// the 401 retry disabled. A JSON object counts as logged in unless the
// deployment provides a decidable marker.
func (c *Client) loginPreflight() (string, error) {
	accountURL, err := c.accountBaseURL()
	if err != nil {
		return "", err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       "/api/v3/islogin",
		BaseURL:    accountURL,
		RetryOn401: false,
	})
	if err != nil {
		return "", err
	}
	if raw, present := payload["islogin"]; present {
		loggedIn, known := statusTruth(raw)
		if !known {
			return "", model.NewWpsAPIError("parse WPS login status", 0, model.WpsCategoryInvalidResponse)
		}
		if !loggedIn {
			return "", model.NewWpsAPIError("WPS login preflight", 401, model.WpsCategorySessionExpired)
		}
	}
	return statusAccountType(payload), nil
}

// CheckLogin mirrors check_login: the coarse account type only.
func (c *Client) CheckLogin() (string, error) {
	return c.loginPreflight()
}

// GroupID mirrors the group_id property: the configured value wins unless
// it is empty or "auto", in which case the workspace state resolves it.
func (c *Client) GroupID() (string, error) {
	configured := c.config.GroupID
	if configured == "" || configured == workspace.AutoValue {
		if c.config.Workspace == nil {
			configured = ""
		} else {
			value, err := c.config.Workspace.GroupID()
			if err != nil {
				return "", err
			}
			configured = value
		}
	}
	if configured == "" {
		return "", model.NewWpsAPIError("WPS workspace is not configured", 503, model.WpsCategoryUpstream)
	}
	return configured, nil
}

// quotePathSegment mirrors quote(value, safe=""): only the unreserved set
// survives, everything else becomes uppercase percent-escapes.
func quotePathSegment(value string) string {
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case char >= 'A' && char <= 'Z',
			char >= 'a' && char <= 'z',
			char >= '0' && char <= '9',
			char == '_', char == '.', char == '-', char == '~':
			builder.WriteByte(char)
		default:
			const hexDigits = "0123456789ABCDEF"
			builder.WriteByte('%')
			builder.WriteByte(hexDigits[char>>4])
			builder.WriteByte(hexDigits[char&0x0F])
		}
	}
	return builder.String()
}

// probeList issues the count=1 root listing used by the preflight. D-02:
// the request runs with the 401 retry disabled so the status path can never
// trigger a refresh grant. Response validation here mirrors the parts of
// list_entries the status path can observe; full page parsing arrives with
// the list migration.
func (c *Client) probeList(rootID string) error {
	groupID, err := c.GroupID()
	if err != nil {
		return err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path: "/3rd/drive/api/v5/groups/" + quotePathSegment(groupID) + "/files",
		Query: []QueryPair{
			{Key: "parentid", Value: rootID},
			{Key: "offset", Value: "0"},
			{Key: "count", Value: "1"},
			{Key: "orderby", Value: "mtime"},
			{Key: "order", Value: "desc"},
		},
		RetryOn401: false,
	})
	if err != nil {
		return err
	}
	if raw, present := payload["files"]; present {
		if _, isList := raw.([]any); !isList {
			return model.NewWpsAPIError("list files", 0, model.WpsCategoryUpstream)
		}
	}
	if raw, present := payload["result"]; present {
		if resultText, isString := raw.(string); isString && resultText != "ok" {
			return model.NewWpsAPIError("list files", 0, model.WpsCategoryUpstream)
		}
	}
	return nil
}

// statusFromError mirrors _status_from_error.
func statusFromError(
	err error,
	workspacePhase bool,
	accountType string,
	checkedAt int,
) model.WpsStatus {
	apiErr, ok := model.AsWpsAPIError(err)
	if !ok {
		return upstreamUnavailableStatus(checkedAt, 0)
	}
	if apiErr.Status == 401 || apiErr.Category == model.WpsCategorySessionExpired {
		return statusValue("session_expired", "session_expired", "unknown", accountType, checkedAt)
	}
	if workspacePhase && (apiErr.Status == 403 || apiErr.Status == 404) {
		return statusValue("permission_denied", "connected", "permission_denied", accountType, checkedAt)
	}
	if apiErr.Category == model.WpsCategoryInvalidResponse {
		return statusValue("invalid_response", "unknown", "unknown", accountType, checkedAt)
	}
	return upstreamUnavailableStatus(checkedAt, 0)
}

// probeStatus mirrors _probe_status: the login preflight, then a single
// read-only root listing that proves the selected group/root is readable.
func (c *Client) probeStatus(rootID string, checkedAt int) model.WpsStatus {
	accountType, err := c.loginPreflight()
	if err != nil {
		return statusFromError(err, false, "unknown", checkedAt)
	}
	if err := c.probeList(rootID); err != nil {
		return statusFromError(err, true, accountType, checkedAt)
	}
	return statusValue("connected", "connected", "ready", accountType, checkedAt)
}

// CheckStatus mirrors check_status: a cached status keyed by the credential
// snapshot, group, and root; a 30 second success TTL and a 5 second failure
// backoff; concurrent callers share one probe (singleflight).
func (c *Client) CheckStatus(rootID string) (model.WpsStatus, error) {
	if rootID == "" {
		return model.WpsStatus{}, errors.New("root_id is required")
	}
	checkedAt := int(time.Now().Unix())
	current, err := c.currentCredentials()
	if err != nil {
		if c.statusCredentialsAreMissing() {
			return notConfiguredStatus(checkedAt), nil
		}
		return invalidResponseCredentialsStatus(checkedAt), nil
	}
	if current.Cookie == "" {
		return notConfiguredStatus(checkedAt), nil
	}
	groupID, err := c.GroupID()
	if err != nil {
		if _, isAPI := model.AsWpsAPIError(err); isAPI {
			return notConfiguredStatus(checkedAt), nil
		}
		return model.WpsStatus{}, err
	}
	marker := strings.Join(
		[]string{current.Cookie, current.CSRFToken, groupID, rootID},
		"\x00",
	)

	c.statusMu.Lock()
	for {
		if marker != c.statusCacheMarker {
			c.statusCache = nil
			c.statusCacheUntil = time.Time{}
			c.statusCacheMarker = marker
		}
		now := time.Now()
		if c.statusCache != nil && c.statusCacheUntil.After(now) {
			cached := *c.statusCache
			remaining := int(math.Ceil(c.statusCacheUntil.Sub(now).Seconds()))
			if cached.Status == "connected" {
				remaining = 0
			}
			c.statusMu.Unlock()
			return cached.WithRetryAfter(remaining), nil
		}
		if c.statusInflight {
			done := c.statusDone
			probeWait := time.Duration(math.Max(c.config.Timeout, 1.0)+1.0) * time.Second
			deadline := now.Add(probeWait)
			c.statusMu.Unlock()
			timer := time.NewTimer(time.Until(deadline))
			var expired bool
			select {
			case <-done:
				timer.Stop()
			case <-timer.C:
				expired = true
			}
			if expired {
				return upstreamUnavailableStatus(checkedAt, 1), nil
			}
			c.statusMu.Lock()
			continue
		}
		c.statusInflight = true
		c.statusDone = make(chan struct{})
		c.statusMu.Unlock()
		break
	}

	result := c.probeStatus(rootID, checkedAt)

	c.statusMu.Lock()
	c.statusCache = &result
	cacheDuration := c.config.StatusFailureBackoff
	if result.Status == "connected" {
		cacheDuration = c.config.StatusProbeTTL
	}
	c.statusCacheUntil = time.Now().Add(time.Duration(cacheDuration * float64(time.Second)))
	c.statusCacheMarker = marker
	c.statusInflight = false
	close(c.statusDone)
	c.statusMu.Unlock()
	return result, nil
}
