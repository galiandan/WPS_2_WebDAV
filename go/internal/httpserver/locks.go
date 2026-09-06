// The process-local WebDAV write lock registry. WPS locking was not present
// in the confirmed captures, so locks protect this adapter's concurrent
// clients only: they expire by monotonic clock, are cleaned on every
// operation, and never survive a restart.

package httpserver

import (
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// DefaultDavLockLimits mirror the AdapterApplication defaults.
const (
	DefaultLockMaxTimeout = 86400
	DefaultLockMaxLocks   = 4096
)

// lockTokenPattern mirrors DavLockStore._token_pattern: any angle-bracketed
// opaquelocktoken URI, matched case-insensitively.
var lockTokenPattern = regexp.MustCompile(`(?i)<((?:opaquelocktoken:)[^>]+)>`)

// lockDepthInfinity is the only depth value that covers descendants.
const lockDepthInfinity = "infinity"

// ActiveLock mirrors the frozen ActiveLock dataclass. ExpiresAt is driven
// by the store's monotonic clock source.
type ActiveLock struct {
	Token          string
	Path           string
	Depth          string
	Owner          string
	TimeoutSeconds int
	ExpiresAt      time.Time
}

// lockTokenInvalidError and lockConflictError mirror the Python KeyError and
// RuntimeError surfaces the LOCK/UNLOCK handlers translate into status codes.
var (
	errLockTokenInvalid = errors.New("lock token is invalid")
	errLockConflict     = errors.New("resource is already locked")
)

// DavLockStore is the process-local WebDAV write lock registry. Every
// operation purges expired entries first; all state lives behind one mutex.
type DavLockStore struct {
	maxTimeout int
	maxLocks   int

	mu    sync.Mutex
	locks map[string]ActiveLock

	// now is the monotonic clock seam; production uses time.Now, whose
	// wall-clock reading carries the monotonic measurement Go compares.
	now func() time.Time
}

// NewDavLockStore mirrors DavLockStore.__init__: both limits must be
// positive. Process restarts always start from an empty registry.
func NewDavLockStore(maxTimeout int, maxLocks int) (*DavLockStore, error) {
	if maxTimeout <= 0 {
		return nil, errors.New("max_timeout must be positive")
	}
	if maxLocks <= 0 {
		return nil, errors.New("max_locks must be positive")
	}
	return &DavLockStore{
		maxTimeout: maxTimeout,
		maxLocks:   maxLocks,
		locks:      map[string]ActiveLock{},
		now:        time.Now,
	}, nil
}

// lockApplies mirrors _applies: an exact path always applies; only a depth
// infinity lock reaches descendants below it.
func lockApplies(lock ActiveLock, path string) bool {
	if lock.Path == path {
		return true
	}
	if lock.Depth != lockDepthInfinity {
		return false
	}
	return strings.HasPrefix(path, "/") && (lock.Path == "/" ||
		strings.HasPrefix(path, strings.TrimRight(lock.Path, "/")+"/"))
}

// purge drops expired locks; callers hold the mutex.
func (s *DavLockStore) purge() {
	now := s.now()
	for token, lock := range s.locks {
		if !lock.ExpiresAt.After(now) {
			delete(s.locks, token)
		}
	}
}

// TokensFromHeaders mirrors tokens_from_headers: every angle-bracketed
// opaquelocktoken URI plus the bare, angle-stripped header value when it
// names a lock token. Tokens keep their original case.
func TokensFromHeaders(headers ...string) map[string]struct{} {
	tokens := map[string]struct{}{}
	for _, header := range headers {
		if header == "" {
			continue
		}
		for _, match := range lockTokenPattern.FindAllStringSubmatch(header, -1) {
			tokens[match[1]] = struct{}{}
		}
		stripped := strings.Trim(strings.TrimSpace(header), "<>")
		if strings.HasPrefix(strings.ToLower(stripped), "opaquelocktoken:") {
			tokens[stripped] = struct{}{}
		}
	}
	return tokens
}

// Allows mirrors allows: every lock that applies to the path must be
// released by one of the presented tokens.
func (s *DavLockStore) Allows(path string, tokens map[string]struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge()
	for _, lock := range s.locks {
		if lockApplies(lock, path) {
			if _, ok := tokens[lock.Token]; !ok {
				return false
			}
		}
	}
	return true
}

// Acquire mirrors acquire: the timeout is clamped into [1, max_timeout], a
// refresh only renews the exact token on the exact path, a new lock refuses
// when any existing lock covers the path or would be covered by the new
// infinity lock, and the registry cap answers the service-busy error.
func (s *DavLockStore) Acquire(path string, depth string, owner string, timeoutSeconds int, refreshToken string) (ActiveLock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge()
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	if timeoutSeconds > s.maxTimeout {
		timeoutSeconds = s.maxTimeout
	}
	if refreshToken != "" {
		current, ok := s.locks[refreshToken]
		if !ok || current.Path != path {
			return ActiveLock{}, errLockTokenInvalid
		}
		refreshed := ActiveLock{
			Token:          current.Token,
			Path:           current.Path,
			Depth:          current.Depth,
			Owner:          current.Owner,
			TimeoutSeconds: timeoutSeconds,
			ExpiresAt:      s.now().Add(time.Duration(timeoutSeconds) * time.Second),
		}
		s.locks[refreshToken] = refreshed
		return refreshed, nil
	}
	for _, current := range s.locks {
		if lockApplies(current, path) {
			return ActiveLock{}, errLockConflict
		}
		// A new infinity lock also conflicts with any lock under it, which
		// the reference checks with a synthetic depth-infinity lock at the
		// new path.
		if depth == lockDepthInfinity && lockApplies(ActiveLock{Path: path, Depth: depth}, current.Path) {
			return ActiveLock{}, errLockConflict
		}
	}
	if len(s.locks) >= s.maxLocks {
		return ActiveLock{}, model.NewStorageError(model.KindServiceBusy, "too many active WebDAV locks")
	}
	token := "opaquelocktoken:" + newLockUUID()
	active := ActiveLock{
		Token:          token,
		Path:           path,
		Depth:          depth,
		Owner:          owner,
		TimeoutSeconds: timeoutSeconds,
		ExpiresAt:      s.now().Add(time.Duration(timeoutSeconds) * time.Second),
	}
	s.locks[token] = active
	return active, nil
}

// Unlock mirrors unlock: the token must exist and name a lock on exactly
// this path.
func (s *DavLockStore) Unlock(path string, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge()
	current, ok := s.locks[token]
	if !ok || current.Path != path {
		return errLockTokenInvalid
	}
	delete(s.locks, token)
	return nil
}

// newLockUUID renders a random RFC 4122 version-4 UUID like uuid.uuid4().
func newLockUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("crypto/rand is unavailable: " + err.Error())
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for index, value := range bytes {
		switch index {
		case 4, 6, 8, 10:
			out = append(out, '-')
		}
		out = append(out, hexDigits[value>>4], hexDigits[value&0x0f])
	}
	return string(out)
}
