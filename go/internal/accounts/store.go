package accounts

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const (
	MaxMembers         = 32
	maxStateBytes      = 256 << 10
	passwordIterations = 600000
)

var (
	ErrInvalid   = errors.New("invalid account fields")
	ErrConflict  = errors.New("username already exists")
	ErrNotFound  = errors.New("account not found")
	ErrLimit     = errors.New("account limit reached")
	ErrImmutable = errors.New("installation account is managed by adapter configuration")
	ErrState     = errors.New("account state is unavailable")
	ErrBusy      = errors.New("authentication is busy; retry shortly")
)

type User struct {
	auth.Principal
	Enabled bool `json:"enabled"`
}
type storedUser struct {
	User         User   `json:"user"`
	Salt         string `json:"salt"`
	PasswordHash string `json:"password_hash"`
	Iterations   int    `json:"iterations"`
}
type diskState struct {
	Version int          `json:"version"`
	Key     string       `json:"key"`
	Users   []storedUser `json:"users"`
}
type CreateUser struct {
	Username, Password, RootPath, RootID, RootBinding string
	Permissions                                       auth.Permissions
}
type UpdateUser struct {
	Username, Password, RootPath, RootID, RootBinding *string
	Permissions                                       *auth.Permissions
	Enabled                                           *bool
}
type authCacheEntry struct {
	ID      string
	Version uint64
	Expires time.Time
}
type Store struct {
	mu         sync.Mutex
	path       string
	admin      auth.CredentialSource
	users      []storedUser
	verifyGate chan struct{}
	cache      map[[32]byte]authCacheEntry
	write      func(string, string) (int64, error)
	key        [32]byte
}

func New(path string, admin auth.CredentialSource) (*Store, error) {
	if path == "" || admin == nil {
		return nil, ErrState
	}
	payload, _, err := securefile.ReadJSONState(path, maxStateBytes)
	if err != nil {
		return nil, ErrState
	}
	state := diskState{Version: 1, Users: []storedUser{}}
	if payload != nil {
		raw, _ := json.Marshal(payload)
		if json.Unmarshal(raw, &state) != nil || state.Version != 1 || len(state.Users) > MaxMembers {
			return nil, ErrState
		}
	}
	s := &Store{path: path, admin: admin, users: state.Users, verifyGate: make(chan struct{}, 2), cache: make(map[[32]byte]authCacheEntry), write: writeDurable}
	if payload == nil {
		if _, err := rand.Read(s.key[:]); err != nil {
			return nil, ErrState
		}
	} else {
		key, err := hex.DecodeString(state.Key)
		if err != nil || len(key) != 32 {
			return nil, ErrState
		}
		copy(s.key[:], key)
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for _, u := range s.users {
		p := u.User.Principal
		salt, se := hex.DecodeString(u.Salt)
		hash, he := hex.DecodeString(u.PasswordHash)
		id, ie := hex.DecodeString(p.ID)
		if validateUsername(p.Username) != nil || validateRoot(p.RootPath, p.RootID, p.RootBinding) != nil || !p.Permissions.Read || p.Role != "member" || p.PolicyVersion == 0 || se != nil || len(salt) != 32 || he != nil || len(hash) != 32 || ie != nil || len(id) != 16 || u.Iterations != passwordIterations || names[p.Username] || ids[p.ID] {
			return nil, ErrState
		}
		names[p.Username], ids[p.ID] = true, true
	}
	if payload == nil {
		if err := s.persistLocked(s.users); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func writeDurable(path, body string) (int64, error) {
	mtime, err := securefile.WriteAtomic(path, body)
	if err != nil {
		return 0, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return 0, err
	}
	return mtime, nil
}
func validateUsername(name string) error {
	if name == "" || len(name) > 64 || name != strings.TrimSpace(name) {
		return ErrInvalid
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return ErrInvalid
		}
	}
	return nil
}
func validateRoot(path, id, binding string) error {
	decoded, err := hex.DecodeString(binding)
	if err != nil || len(decoded) != 32 {
		return ErrInvalid
	}
	if len(path) > 4096 || id == "" || len(id) > 4096 {
		return ErrInvalid
	}
	parts, err := storage.SplitRemotePath(path)
	if err != nil {
		return ErrInvalid
	}
	canonical, err := storage.JoinRemotePath(parts, false)
	if err != nil || canonical != path {
		return ErrInvalid
	}
	return nil
}
func validatePassword(password string) error {
	if len(password) < 8 || len(password) > 256 || !utf8.ValidString(password) {
		return ErrInvalid
	}
	for _, c := range password {
		if c < 32 || c == 127 {
			return ErrInvalid
		}
	}
	return nil
}
func (s *Store) adminPrincipal() (auth.Principal, bool) {
	name, password := s.admin()
	return s.adminIdentity(name, password)
}
func (s *Store) adminIdentity(name, password string) (auth.Principal, bool) {
	if name == "" || password == "" {
		return auth.Principal{}, false
	}
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write([]byte(name + "\x00" + password))
	digest := mac.Sum(nil)
	version := binary.BigEndian.Uint64(digest[:8])
	if version == 0 {
		version = 1
	}
	return auth.Principal{ID: auth.InstallationID, Username: name, Role: "admin", PolicyVersion: version, RootPath: "/", Permissions: auth.Permissions{Read: true, Upload: true, Delete: true}}, true
}
func (s *Store) LookupID(id string) (auth.Principal, bool) {
	if id == auth.InstallationID {
		return s.adminPrincipal()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.User.ID == id && u.User.Enabled {
			return u.User.Principal, true
		}
	}
	return auth.Principal{}, false
}
func (s *Store) LookupUsername(name string) (auth.Principal, bool) {
	if admin, ok := s.adminPrincipal(); ok && admin.Username == name {
		return admin, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.User.Username == name && u.User.Enabled {
			return u.User.Principal, true
		}
	}
	return auth.Principal{}, false
}
func (s *Store) List() []User {
	s.mu.Lock()
	users := make([]User, 0, len(s.users)+1)
	for _, u := range s.users {
		users = append(users, u.User)
	}
	s.mu.Unlock()
	if admin, ok := s.adminPrincipal(); ok {
		users = append([]User{{Principal: admin, Enabled: true}}, users...)
	}
	return users
}
func (s *Store) persistLocked(users []storedUser) error {
	raw, err := json.Marshal(diskState{Version: 1, Key: hex.EncodeToString(s.key[:]), Users: users})
	if err != nil || len(raw) > maxStateBytes {
		return ErrState
	}
	if _, err := s.write(s.path, string(raw)); err != nil {
		return ErrState
	}
	s.users = users
	s.cache = make(map[[32]byte]authCacheEntry)
	return nil
}

func (s *Store) hashPassword(password string) (string, string, error) {
	if err := validatePassword(password); err != nil {
		return "", "", err
	}
	select {
	case s.verifyGate <- struct{}{}:
	default:
		return "", "", ErrBusy
	}
	defer func() { <-s.verifyGate }()
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", "", ErrState
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", "", ErrState
	}
	return hex.EncodeToString(salt), hex.EncodeToString(hash), nil
}

func (s *Store) Create(input CreateUser) (User, error) {
	if validateUsername(input.Username) != nil || validateRoot(input.RootPath, input.RootID, input.RootBinding) != nil || !input.Permissions.Read {
		return User{}, ErrInvalid
	}
	salt, hash, err := s.hashPassword(input.Password)
	if err != nil {
		return User{}, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return User{}, ErrState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	admin, _ := s.admin()
	if input.Username == admin {
		return User{}, ErrConflict
	}
	if len(s.users) >= MaxMembers {
		return User{}, ErrLimit
	}
	for _, u := range s.users {
		if u.User.Username == input.Username {
			return User{}, ErrConflict
		}
	}
	u := storedUser{User: User{Principal: auth.Principal{ID: hex.EncodeToString(id), Username: input.Username, Role: "member", PolicyVersion: 1, RootPath: input.RootPath, RootID: input.RootID, RootBinding: input.RootBinding, Permissions: input.Permissions}, Enabled: true}, Salt: salt, PasswordHash: hash, Iterations: passwordIterations}
	users := append(append([]storedUser(nil), s.users...), u)
	if err := s.persistLocked(users); err != nil {
		return User{}, err
	}
	return u.User, nil
}

func (s *Store) Update(id string, input UpdateUser) (User, error) {
	if id == auth.InstallationID {
		return User{}, ErrImmutable
	}
	var salt, hash string
	var err error
	if input.Password != nil {
		salt, hash, err = s.hashPassword(*input.Password)
		if err != nil {
			return User{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, u := range s.users {
		if u.User.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return User{}, ErrNotFound
	}
	u := s.users[index]
	if input.Username != nil {
		u.User.Username = *input.Username
	}
	if input.RootPath != nil {
		u.User.RootPath = *input.RootPath
	}
	if input.RootID != nil {
		u.User.RootID = *input.RootID
	}
	if input.RootBinding != nil {
		u.User.RootBinding = *input.RootBinding
	}
	if input.Permissions != nil {
		u.User.Permissions = *input.Permissions
	}
	if input.Enabled != nil {
		u.User.Enabled = *input.Enabled
	}
	if validateUsername(u.User.Username) != nil || validateRoot(u.User.RootPath, u.User.RootID, u.User.RootBinding) != nil || !u.User.Permissions.Read {
		return User{}, ErrInvalid
	}
	admin, _ := s.admin()
	if u.User.Username == admin {
		return User{}, ErrConflict
	}
	for i, other := range s.users {
		if i != index && other.User.Username == u.User.Username {
			return User{}, ErrConflict
		}
	}
	u.User.PolicyVersion++
	if u.User.PolicyVersion == 0 {
		return User{}, ErrState
	}
	if input.Password != nil {
		u.Salt, u.PasswordHash = salt, hash
	}
	users := append([]storedUser(nil), s.users...)
	users[index] = u
	if err := s.persistLocked(users); err != nil {
		return User{}, err
	}
	return u.User, nil
}
func (s *Store) Delete(id string) error {
	if id == auth.InstallationID {
		return ErrImmutable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, u := range s.users {
		if u.User.ID == id {
			users := append([]storedUser(nil), s.users[:i]...)
			users = append(users, s.users[i+1:]...)
			return s.persistLocked(users)
		}
	}
	return ErrNotFound
}

func (s *Store) Authenticate(username, password string) (auth.Principal, error) {
	if len(username) > 256 || len(password) > 256 {
		return auth.Principal{}, auth.ErrInvalidCredentials
	}
	adminName, expected := s.admin()
	if admin, ok := s.adminIdentity(adminName, expected); ok && username == admin.Username {
		if subtle.ConstantTimeCompare([]byte(password), []byte(expected)) == 1 {
			return admin, nil
		}
		return auth.Principal{}, auth.ErrInvalidCredentials
	}
	key := sha256.Sum256([]byte(username + "\x00" + password))
	s.mu.Lock()
	cached, ok := s.cache[key]
	var member storedUser
	found := false
	for _, u := range s.users {
		if u.User.Username == username && u.User.Enabled {
			member = u
			found = true
			break
		}
	}
	if ok && found && cached.ID == member.User.ID && cached.Version == member.User.PolicyVersion && time.Now().Before(cached.Expires) {
		s.mu.Unlock()
		return member.User.Principal, nil
	}
	s.mu.Unlock()
	if !found {
		return auth.Principal{}, auth.ErrInvalidCredentials
	}
	select {
	case s.verifyGate <- struct{}{}:
	default:
		return auth.Principal{}, ErrBusy
	}
	defer func() { <-s.verifyGate }()
	salt, _ := hex.DecodeString(member.Salt)
	expectedHash, _ := hex.DecodeString(member.PasswordHash)
	hash, err := pbkdf2.Key(sha256.New, password, salt, member.Iterations, 32)
	if err != nil || subtle.ConstantTimeCompare(hash, expectedHash) != 1 {
		return auth.Principal{}, auth.ErrInvalidCredentials
	}
	current, ok := s.LookupID(member.User.ID)
	if !ok || current.PolicyVersion != member.User.PolicyVersion {
		return auth.Principal{}, auth.ErrInvalidCredentials
	}
	s.mu.Lock()
	if len(s.cache) >= 128 {
		s.cache = make(map[[32]byte]authCacheEntry)
	}
	s.cache[key] = authCacheEntry{ID: current.ID, Version: current.PolicyVersion, Expires: time.Now().Add(30 * time.Second)}
	s.mu.Unlock()
	return current, nil
}
