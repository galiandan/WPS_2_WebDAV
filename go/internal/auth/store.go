// Package auth manages the adapter's browser sessions.
//
// The adapter has one account: the username and password configured during
// installation. This package deliberately keeps only short-lived session
// tokens in memory; the installation credentials remain in the adapter's
// existing protected secret files and are supplied through a callback.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	SessionCookieName = "wps_session"
	SessionMaxAge     = 7 * 24 * time.Hour
)

var ErrInvalidCredentials = errors.New("invalid credentials")

// User is the public identity attached to a browser request.
type User struct {
	Username string `json:"username"`
}

// CredentialSource returns the one adapter account. It is called for each
// login, so replacing the protected credential files takes effect without a
// process restart and keeps browser login aligned with WebDAV Basic Auth.
type CredentialSource func() (username, password string)

type session struct {
	Username string
	Expires  time.Time
}

// Store is a process-local session store for the single installation account.
// Restarting the service invalidates every browser session.
type Store struct {
	credentials CredentialSource
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]session
}

// NewStore creates a session store. A nil source is allowed for embedded
// tests and simply makes every login fail.
func NewStore(credentials CredentialSource) *Store {
	return &Store{
		credentials: credentials,
		now:         time.Now,
		sessions:    make(map[string]session),
	}
}

// Login verifies the supplied credentials against the installation account
// and creates a process-local session token.
func (s *Store) Login(username, password string) (string, User, error) {
	configuredUsername, configuredPassword := s.credentialsValue()
	username = strings.TrimSpace(username)
	if username == "" || configuredUsername == "" || configuredPassword == "" ||
		subtle.ConstantTimeCompare([]byte(username), []byte(configuredUsername)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(configuredPassword)) != 1 {
		return "", User{}, ErrInvalidCredentials
	}
	token, err := randomToken()
	if err != nil {
		return "", User{}, errors.New("login failed")
	}
	s.mu.Lock()
	s.sessions[token] = session{Username: configuredUsername, Expires: s.now().Add(SessionMaxAge)}
	s.mu.Unlock()
	return token, User{Username: configuredUsername}, nil
}

// Current resolves a session cookie and expires it lazily.
func (s *Store) Current(token string) (User, bool) {
	if token == "" {
		return User{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sessions[token]
	if !ok || !s.now().Before(current.Expires) {
		delete(s.sessions, token)
		return User{}, false
	}
	return User{Username: current.Username}, true
}

// Logout invalidates the current browser session.
func (s *Store) Logout(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func (s *Store) credentialsValue() (string, string) {
	if s.credentials == nil {
		return "", ""
	}
	return s.credentials()
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
