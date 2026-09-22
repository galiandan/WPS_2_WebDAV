// Package auth manages the adapter's browser sessions.
//
// Each Store owns one account's factors and short-lived session tokens.
// AccountStores composes the installation account with isolated member stores;
// installation credentials remain in the existing protected secret files.
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
type User = Principal

// CredentialSource returns the one adapter account. It is called for each
// login, so replacing the protected credential files takes effect without a
// process restart and keeps browser login aligned with WebDAV Basic Auth.
type CredentialSource func() (username, password string)

type session struct {
	Username      string
	Expires       time.Time
	UserID        string
	PolicyVersion uint64
}

// Store is a process-local session and factor store for one account.
// Restarting the service invalidates every browser session.
type Store struct {
	credentials CredentialSource
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]session

	// factorState and pending are protected by the same mutex as sessions.
	// Keeping the factor state in this store makes password, TOTP, and
	// passkey authentication share one session boundary.
	securityPath    string
	factorState     factorState
	pending         map[string]factorChallenge
	verifyPassword  func(string, string) (Principal, error)
	principalSource func() (Principal, bool)
}

// NewStore creates a session store. A nil source is allowed for embedded
// tests and simply makes every login fail.
func NewStore(credentials CredentialSource) *Store {
	return &Store{
		credentials: credentials,
		now:         time.Now,
		sessions:    make(map[string]session),
		pending:     make(map[string]factorChallenge),
	}
}

// NewPersistentStore creates a browser authentication store whose optional
// second factors survive service restarts. An empty path keeps factor state
// in memory, which is useful for focused tests and preserves NewStore's
// legacy behaviour.
func NewPersistentStore(credentials CredentialSource, path string) (*Store, error) {
	store := NewStore(credentials)
	store.securityPath = path
	if err := store.loadFactorState(); err != nil {
		return nil, err
	}
	return store, nil
}

// Login verifies the supplied credentials against the installation account
// and creates a process-local session token.
func (s *Store) Login(username, password string) (string, User, error) {
	user, err := s.verifyCredentials(username, password)
	if err != nil {
		return "", User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.newSessionForUserLocked(user)
	return token, user, err
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
	if s.principalSource != nil {
		principal, ok := s.principalSource()
		if !ok || principal.ID != current.UserID || principal.PolicyVersion != current.PolicyVersion {
			delete(s.sessions, token)
			return User{}, false
		}
		return principal, true
	}
	return User{Username: current.Username}, true
}

func (s *Store) verifyCredentials(username, password string) (Principal, error) {
	username = strings.TrimSpace(username)
	if s.verifyPassword != nil {
		return s.verifyPassword(username, password)
	}
	configuredUsername, configuredPassword := s.credentialsValue()
	if username == "" || configuredUsername == "" || configuredPassword == "" || subtle.ConstantTimeCompare([]byte(username), []byte(configuredUsername)) != 1 || subtle.ConstantTimeCompare([]byte(password), []byte(configuredPassword)) != 1 {
		return Principal{}, ErrInvalidCredentials
	}
	return Principal{Username: configuredUsername}, nil
}

func (s *Store) newSessionForUserLocked(user Principal) (string, error) {
	if s.principalSource != nil {
		current, ok := s.principalSource()
		if !ok || current.ID != user.ID || current.PolicyVersion != user.PolicyVersion {
			return "", ErrInvalidCredentials
		}
	}
	for token, current := range s.sessions {
		if !s.now().Before(current.Expires) {
			delete(s.sessions, token)
		}
	}
	if len(s.sessions) >= 128 {
		return "", errors.New("too many active sessions")
	}
	token, err := randomToken()
	if err != nil {
		return "", errors.New("login failed")
	}
	s.sessions[token] = session{Username: user.Username, Expires: s.now().Add(SessionMaxAge), UserID: user.ID, PolicyVersion: user.PolicyVersion}
	return token, nil
}

func (s *Store) challengePrincipal(challenge factorChallenge) (Principal, bool) {
	if s.principalSource == nil {
		return Principal{Username: challenge.Username}, true
	}
	current, ok := s.principalSource()
	return current, ok && current.ID == challenge.UserID && current.PolicyVersion == challenge.PolicyVersion
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
