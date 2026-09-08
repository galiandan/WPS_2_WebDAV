// Package auth stores the adapter's local web accounts and short-lived
// browser sessions. It deliberately knows nothing about WPS credentials;
// that boundary lets per-user WPS profiles be added without changing the
// browser authentication contract.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
)

const (
	SessionCookieName = "wps_session"
	SessionMaxAge     = 7 * 24 * time.Hour
	passwordRounds    = 120000
	maxDatabaseBytes  = 2 * 1024 * 1024
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{2,63}$`)

var (
	ErrInvalidInput       = errors.New("invalid account input")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUsernameTaken      = errors.New("username already exists")
	ErrRegistrationClosed = errors.New("registration is disabled")
)

// User is the public identity attached to a web request. Password material
// and session tokens are intentionally absent.
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	CreatedAt int64  `json:"created_at"`
}

type storedUser struct {
	User
	PasswordHash string `json:"password_hash"`
}

type database struct {
	Version int          `json:"version"`
	Users   []storedUser `json:"users"`
}

type session struct {
	UserID  string
	Expires time.Time
}

// Store is safe for concurrent HTTP requests. Accounts survive restarts;
// sessions are intentionally process-local so a restart invalidates every
// browser cookie without having to persist bearer tokens on disk.
type Store struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	users    map[string]storedUser
	byID     map[string]storedUser
	sessions map[string]session
}

// NewStore loads an existing private JSON database. A blank path creates an
// in-memory store, which keeps focused tests and embedded deployments simple.
func NewStore(path string) (*Store, error) {
	store := &Store{
		path:     path,
		now:      time.Now,
		users:    make(map[string]storedUser),
		byID:     make(map[string]storedUser),
		sessions: make(map[string]session),
	}
	if path == "" {
		return store, nil
	}
	if err := securefile.ValidateStatePath(path); err != nil {
		return nil, fmt.Errorf("user database is not safe: %w", err)
	}
	payload, _, err := securefile.ReadJSONState(path, maxDatabaseBytes)
	if err != nil {
		return nil, fmt.Errorf("read user database: %w", err)
	}
	if payload == nil {
		return store, nil
	}
	raw, ok := payload["users"].([]any)
	if !ok {
		return nil, errors.New("read user database: users must be an array")
	}
	for _, item := range raw {
		encoded, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("read user database: invalid user record")
		}
		user, err := decodeStoredUser(encoded)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(user.Username)
		if _, exists := store.users[key]; exists || user.ID == "" {
			return nil, errors.New("read user database: duplicate user")
		}
		store.users[key] = user
		store.byID[user.ID] = user
	}
	return store, nil
}

func decodeStoredUser(value map[string]any) (storedUser, error) {
	var user storedUser
	bytes, err := json.Marshal(value)
	if err != nil || json.Unmarshal(bytes, &user) != nil || user.ID == "" || user.Username == "" || user.PasswordHash == "" {
		return storedUser{}, errors.New("read user database: invalid user record")
	}
	return user, nil
}

// UserCount reports the number of registered local accounts.
func (s *Store) UserCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.users)
}

// Register creates and persists a local account.
func (s *Store) Register(username, password string) (User, error) {
	username, password, err := validateCredentials(username, password)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(username)
	if _, exists := s.users[key]; exists {
		return User{}, ErrUsernameTaken
	}
	id, err := randomID()
	if err != nil {
		return User{}, errors.New("create account failed")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, errors.New("create account failed")
	}
	record := storedUser{User: User{ID: id, Username: username, CreatedAt: s.now().Unix()}, PasswordHash: hash}
	if err := s.persistLocked(record); err != nil {
		return User{}, errors.New("save account failed")
	}
	s.users[key] = record
	s.byID[id] = record
	return record.User, nil
}

// Login verifies credentials and creates a process-local session token.
func (s *Store) Login(username, password string) (string, User, error) {
	username = strings.TrimSpace(username)
	if !validUsername(username) || password == "" {
		return "", User{}, ErrInvalidCredentials
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.users[strings.ToLower(username)]
	if !ok || !verifyPassword(password, record.PasswordHash) {
		return "", User{}, ErrInvalidCredentials
	}
	token, err := randomToken()
	if err != nil {
		return "", User{}, errors.New("login failed")
	}
	s.sessions[token] = session{UserID: record.ID, Expires: s.now().Add(SessionMaxAge)}
	return token, record.User, nil
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
	record, ok := s.byID[current.UserID]
	if !ok {
		delete(s.sessions, token)
		return User{}, false
	}
	return record.User, true
}

// Logout invalidates the current browser session.
func (s *Store) Logout(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func validateCredentials(username, password string) (string, string, error) {
	username = strings.TrimSpace(username)
	if !validUsername(username) || !utf8.ValidString(password) || len(password) < 8 || len(password) > 256 {
		return "", "", ErrInvalidInput
	}
	for _, char := range password {
		if char < 0x20 || char == 0x7f {
			return "", "", ErrInvalidInput
		}
	}
	return username, password, nil
}

func validUsername(username string) bool {
	return utf8.ValidString(username) && usernamePattern.MatchString(username)
}

func (s *Store) persistLocked(record storedUser) error {
	database := database{Version: 1, Users: make([]storedUser, 0, len(s.users)+1)}
	for _, existing := range s.users {
		database.Users = append(database.Users, existing)
	}
	database.Users = append(database.Users, record)
	encoded, err := json.Marshal(database)
	if err != nil {
		return err
	}
	if s.path == "" {
		return nil
	}
	if _, err := securefile.WriteAtomic(s.path, string(encoded)); err != nil {
		return err
	}
	return nil
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func hashPassword(password string) (string, error) {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	derived := pbkdf2SHA256([]byte(password), salt[:], passwordRounds, sha256.Size)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordRounds,
		base64.RawStdEncoding.EncodeToString(salt[:]), base64.RawStdEncoding.EncodeToString(derived)), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	var rounds int
	if _, err := fmt.Sscanf(parts[1], "%d", &rounds); err != nil || rounds < 10000 || rounds > 1000000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) < 8 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, rounds, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func pbkdf2SHA256(password, salt []byte, rounds, size int) []byte {
	result := make([]byte, 0, size)
	for block := byte(1); len(result) < size; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write([]byte{0, 0, 0, block})
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for iteration := 1; iteration < rounds; iteration++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for index := range t {
				t[index] ^= u[index]
			}
		}
		result = append(result, t...)
	}
	return result[:size]
}
