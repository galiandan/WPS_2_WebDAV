// Package shares stores revocable read-only grants without retaining bearer
// tokens or cleartext passwords. Browser grant cookies live only in memory.
package shares

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const (
	MaxRecords         = 1000
	MaxStateBytes      = 2 << 20
	MaxGrants          = 1024
	MaxOwnerRecords    = 100
	DefaultLifetime    = 7 * 24 * time.Hour
	MaxLifetime        = 30 * 24 * time.Hour
	GrantLifetime      = time.Hour
	passwordIterations = 600000
	maxAttempts        = 5
	attemptWindow      = 15 * time.Minute
)

var (
	ErrDenied  = errors.New("share is unavailable or access was denied")
	ErrInvalid = errors.New("invalid share request")
	ErrState   = errors.New("share state is unavailable")
	ErrBusy    = errors.New("share service is busy; retry later")
	ErrLimit   = errors.New("share limit reached")
)

type Spec struct {
	OwnerID string `json:"owner_id"`
	// State reads pass through generic JSON maps; a decimal string preserves
	// the full opaque 64-bit admin generation without float64 rounding.
	PolicyVersion uint64 `json:"policy_version,string"`
	Path          string `json:"path"`
	TargetID      string `json:"target_id"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Binding       string `json:"binding"`
}
type Record struct {
	ID           string    `json:"id"`
	Spec         Spec      `json:"spec"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Revoked      bool      `json:"revoked"`
	TokenHash    string    `json:"token_hash"`
	Salt         string    `json:"salt,omitempty"`
	PasswordHash string    `json:"password_hash,omitempty"`
	Iterations   int       `json:"iterations,omitempty"`
}

func (r Record) PasswordRequired() bool { return r.PasswordHash != "" }

type diskState struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}
type grant struct {
	ShareID   string
	ExpiresAt time.Time
}
type bucket struct {
	Count     int
	ExpiresAt time.Time
}
type Store struct {
	mu           sync.Mutex
	file         string
	records      []Record
	grants       map[[32]byte]grant
	attempts     map[[32]byte]bucket
	passwordGate chan struct{}
	now          func() time.Time
	write        func(string, string) (int64, error)
}

func New(file string) (*Store, error) {
	if file == "" {
		return nil, ErrState
	}
	payload, _, err := securefile.ReadJSONState(file, MaxStateBytes)
	if err != nil {
		return nil, ErrState
	}
	state := diskState{Version: 1, Records: []Record{}}
	if payload != nil {
		raw, _ := json.Marshal(payload)
		if json.Unmarshal(raw, &state) != nil || state.Version != 1 || len(state.Records) > MaxRecords {
			return nil, ErrState
		}
	}
	seen := map[string]bool{}
	for _, r := range state.Records {
		if !validHex(r.ID, 16) || seen[r.ID] || validateSpec(r.Spec) != nil || !validHex(r.TokenHash, 32) || !r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) > MaxLifetime {
			return nil, ErrState
		}
		if r.PasswordHash != "" {
			if !validHex(r.PasswordHash, 32) || !validHex(r.Salt, 32) || r.Iterations != passwordIterations {
				return nil, ErrState
			}
		} else if r.Salt != "" || r.Iterations != 0 {
			return nil, ErrState
		}
		seen[r.ID] = true
	}
	s := &Store{file: file, records: state.Records, grants: make(map[[32]byte]grant), attempts: make(map[[32]byte]bucket), passwordGate: make(chan struct{}, 2), now: time.Now, write: writeDurable}
	if payload == nil {
		if err := s.persistLocked(s.records); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func validHex(value string, size int) bool {
	if len(value) != size*2 {
		return false
	}
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == size
}
func validateSpec(spec Spec) error {
	if spec.OwnerID == "" || len(spec.OwnerID) > 64 || spec.PolicyVersion == 0 || spec.TargetID == "" || len(spec.TargetID) > 4096 || len(spec.Path) > 4096 || !validHex(spec.Binding, 32) || (spec.Kind != "file" && spec.Kind != "folder") {
		return ErrInvalid
	}
	parts, err := storage.SplitRemotePath(spec.Path)
	if err != nil {
		return ErrInvalid
	}
	canonical, _ := storage.JoinRemotePath(parts, false)
	if canonical != spec.Path {
		return ErrInvalid
	}
	if _, err := storage.JoinRemotePath([]string{spec.Name}, false); err != nil {
		return ErrInvalid
	}
	return nil
}
func validatePassword(password string) error {
	if len(password) < 4 || len(password) > 128 || !utf8.ValidString(password) {
		return ErrInvalid
	}
	for _, c := range password {
		if c < 32 || c == 127 {
			return ErrInvalid
		}
	}
	return nil
}
func randomHex(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", ErrState
	}
	return hex.EncodeToString(b), nil
}
func writeDurable(file, body string) (int64, error) {
	mtime, err := securefile.WriteAtomic(file, body)
	if err != nil {
		return 0, err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return 0, err
	}
	return mtime, nil
}
func (s *Store) persistLocked(records []Record) error {
	raw, err := json.Marshal(diskState{Version: 1, Records: records})
	if err != nil || len(raw) > MaxStateBytes {
		return ErrState
	}
	if _, err := s.write(s.file, string(raw)); err != nil {
		return ErrState
	}
	s.records = records
	return nil
}

func (s *Store) Create(spec Spec, expires time.Time, password string) (Record, string, error) {
	if err := validateSpec(spec); err != nil {
		return Record{}, "", err
	}
	now := s.now().UTC()
	if expires.IsZero() {
		expires = now.Add(DefaultLifetime)
	}
	if !expires.After(now) || expires.After(now.Add(MaxLifetime)) {
		return Record{}, "", ErrInvalid
	}
	if password != "" {
		if err := validatePassword(password); err != nil {
			return Record{}, "", err
		}
	}
	id, err := randomHex(16)
	if err != nil {
		return Record{}, "", err
	}
	token, err := randomHex(32)
	if err != nil {
		return Record{}, "", err
	}
	digest := sha256.Sum256([]byte(token))
	record := Record{ID: id, Spec: spec, CreatedAt: now, ExpiresAt: expires.UTC(), TokenHash: hex.EncodeToString(digest[:])}
	if password != "" {
		select {
		case s.passwordGate <- struct{}{}:
		default:
			return Record{}, "", ErrBusy
		}
		defer func() { <-s.passwordGate }()
		salt := make([]byte, 32)
		if _, err := rand.Read(salt); err != nil {
			return Record{}, "", ErrState
		}
		hash, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
		if err != nil {
			return Record{}, "", ErrState
		}
		record.Salt, record.PasswordHash, record.Iterations = hex.EncodeToString(salt), hex.EncodeToString(hash), passwordIterations
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Expired/revoked rows can be dropped once the bounded history fills.
	records := append([]Record(nil), s.records...)
	if len(records) >= MaxRecords {
		kept := records[:0]
		for _, r := range records {
			if !r.Revoked && r.ExpiresAt.After(now) {
				kept = append(kept, r)
			}
		}
		records = kept
	}
	owned := 0
	for _, r := range records {
		if r.Spec.OwnerID == spec.OwnerID && r.Spec.PolicyVersion == spec.PolicyVersion && !r.Revoked && r.ExpiresAt.After(now) {
			owned++
		}
	}
	if len(records) >= MaxRecords || owned >= MaxOwnerRecords {
		return Record{}, "", ErrLimit
	}
	records = append(records, record)
	if err := s.persistLocked(records); err != nil {
		return Record{}, "", err
	}
	return record, token, nil
}
func (s *Store) findLocked(id string) (Record, bool) {
	for _, r := range s.records {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}
func (s *Store) Active(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.findLocked(id)
	if !ok || r.Revoked || !s.now().Before(r.ExpiresAt) {
		return Record{}, ErrDenied
	}
	return r, nil
}
func (s *Store) List(owner string, admin bool) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0)
	for i := len(s.records) - 1; i >= 0; i-- {
		r := s.records[i]
		if admin || r.Spec.OwnerID == owner {
			out = append(out, r)
		}
	}
	return out
}
func (s *Store) Revoke(id, owner string, admin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.records {
		if r.ID != id {
			continue
		}
		if !admin && r.Spec.OwnerID != owner {
			return ErrDenied
		}
		if r.Revoked {
			return nil
		}
		records := append([]Record(nil), s.records...)
		records[i].Revoked = true
		if err := s.persistLocked(records); err != nil {
			return err
		}
		for key, g := range s.grants {
			if g.ShareID == id {
				delete(s.grants, key)
			}
		}
		return nil
	}
	return ErrDenied
}

func (s *Store) reserveAttempt(client, id string) ([32]byte, error) {
	key := sha256.Sum256([]byte(client + "\x00" + id))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, b := range s.attempts {
		if !b.ExpiresAt.After(now) {
			delete(s.attempts, k)
		}
	}
	b := s.attempts[key]
	if b.Count >= maxAttempts {
		return key, ErrDenied
	}
	if b.ExpiresAt.IsZero() {
		if len(s.attempts) >= 1024 {
			return key, ErrBusy
		}
		b.ExpiresAt = now.Add(attemptWindow)
	}
	b.Count++
	s.attempts[key] = b
	return key, nil
}
func (s *Store) Unlock(id, token, password, client string, validate ...func(Record) error) (Record, string, time.Time, error) {
	if !validHex(id, 16) || !validHex(token, 32) || len(password) > 128 || len(client) > 256 {
		return Record{}, "", time.Time{}, ErrDenied
	}
	key, err := s.reserveAttempt(client, id)
	if err != nil {
		return Record{}, "", time.Time{}, err
	}
	r, err := s.Active(id)
	if err != nil {
		return Record{}, "", time.Time{}, ErrDenied
	}
	digest := sha256.Sum256([]byte(token))
	expected, _ := hex.DecodeString(r.TokenHash)
	if subtle.ConstantTimeCompare(digest[:], expected) != 1 {
		return Record{}, "", time.Time{}, ErrDenied
	}
	if r.PasswordRequired() {
		select {
		case s.passwordGate <- struct{}{}:
		default:
			return Record{}, "", time.Time{}, ErrBusy
		}
		defer func() { <-s.passwordGate }()
		salt, _ := hex.DecodeString(r.Salt)
		hash, err := pbkdf2.Key(sha256.New, password, salt, r.Iterations, 32)
		expected, _ := hex.DecodeString(r.PasswordHash)
		if err != nil || subtle.ConstantTimeCompare(hash, expected) != 1 {
			return Record{}, "", time.Time{}, ErrDenied
		}
	}
	if len(validate) > 0 && validate[0] != nil {
		if err := validate[0](r); err != nil {
			return Record{}, "", time.Time{}, ErrDenied
		}
	}
	cookie, err := randomHex(32)
	if err != nil {
		return Record{}, "", time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	current, ok := s.findLocked(id)
	if !ok || current.Revoked || !current.ExpiresAt.After(now) {
		return Record{}, "", time.Time{}, ErrDenied
	}
	for key, g := range s.grants {
		if !g.ExpiresAt.After(now) {
			delete(s.grants, key)
		}
	}
	if len(s.grants) >= MaxGrants {
		return Record{}, "", time.Time{}, ErrBusy
	}
	expires := now.Add(GrantLifetime)
	if current.ExpiresAt.Before(expires) {
		expires = current.ExpiresAt
	}
	s.grants[sha256.Sum256([]byte(cookie))] = grant{ShareID: id, ExpiresAt: expires}
	delete(s.attempts, key)
	return current, cookie, expires, nil
}
func (s *Store) Authorize(id, cookie string) (Record, error) {
	r, _, err := s.AuthorizeGrant(id, cookie)
	return r, err
}
func (s *Store) AuthorizeGrant(id, cookie string) (Record, time.Time, error) {
	if !validHex(id, 16) || !validHex(cookie, 32) {
		return Record{}, time.Time{}, ErrDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sha256.Sum256([]byte(cookie))
	g, ok := s.grants[key]
	if !ok || !s.now().Before(g.ExpiresAt) {
		delete(s.grants, key)
		return Record{}, time.Time{}, ErrDenied
	}
	if g.ShareID != id {
		return Record{}, time.Time{}, ErrDenied
	}
	r, ok := s.findLocked(id)
	if !ok || r.Revoked || !s.now().Before(r.ExpiresAt) {
		delete(s.grants, key)
		return Record{}, time.Time{}, ErrDenied
	}
	return r, g.ExpiresAt, nil
}

// ValidID is safe for literal routing; path separators or encoded aliases are
// never accepted as public share identifiers.
func ValidID(id string) bool { return validHex(id, 16) && strings.ToLower(id) == id }
