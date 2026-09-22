package auth

import (
	"encoding/hex"
	"path/filepath"
	"sync"
)

// AccountStores isolates each user's factors, sessions and pending challenges.
// The original admin state remains at its existing path and requires no migration.
type AccountStores struct {
	mu       sync.Mutex
	admin    *Store
	provider Provider
	members  map[string]*Store
}

func NewAccountStores(admin *Store, provider Provider) (*AccountStores, error) {
	if admin == nil || provider == nil {
		return nil, ErrInvalidCredentials
	}
	hub := &AccountStores{admin: admin, provider: provider, members: make(map[string]*Store)}
	hub.bind(admin, InstallationID)
	return hub, nil
}
func (h *AccountStores) bind(store *Store, id string) {
	store.principalSource = func() (Principal, bool) { return h.provider.LookupID(id) }
	store.verifyPassword = func(name, password string) (Principal, error) {
		p, err := h.provider.Authenticate(name, password)
		if err != nil {
			return Principal{}, err
		}
		if p.ID != id {
			return Principal{}, ErrInvalidCredentials
		}
		return p, nil
	}
}
func (h *AccountStores) Provider() Provider { return h.provider }
func (h *AccountStores) AdminStore() *Store { return h.admin }
func (h *AccountStores) ForUsername(username string) (*Store, error) {
	p, ok := h.provider.LookupUsername(username)
	if !ok {
		return nil, ErrInvalidCredentials
	}
	return h.ForPrincipal(p)
}
func (h *AccountStores) ForPrincipal(p Principal) (*Store, error) {
	current, ok := h.provider.LookupID(p.ID)
	if !ok || current.PolicyVersion != p.PolicyVersion {
		return nil, ErrInvalidCredentials
	}
	if p.ID == InstallationID {
		return h.admin, nil
	}
	decoded, err := hex.DecodeString(p.ID)
	if err != nil || len(decoded) != 16 {
		return nil, ErrInvalidCredentials
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Deleted accounts no longer retain live sessions or loaded factor state.
	for id := range h.members {
		if _, ok := h.provider.LookupID(id); !ok {
			delete(h.members, id)
		}
	}
	if store, ok := h.members[p.ID]; ok {
		return store, nil
	}
	if len(h.members) >= 32 {
		return nil, ErrFactorState
	}
	file := ""
	if h.admin.securityPath != "" {
		file = filepath.Join(filepath.Dir(h.admin.securityPath), "auth-"+p.ID+".json")
	}
	store, err := NewPersistentStore(func() (string, string) {
		principal, ok := h.provider.LookupID(p.ID)
		if !ok {
			return "", ""
		}
		return principal.Username, ""
	}, file)
	if err != nil {
		return nil, err
	}
	h.bind(store, p.ID)
	h.members[p.ID] = store
	return store, nil
}
func (h *AccountStores) snapshot() []*Store {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []*Store{h.admin}
	for _, store := range h.members {
		out = append(out, store)
	}
	return out
}
func (h *AccountStores) Current(token string) (Principal, bool) {
	_, p, ok := h.ForSession(token)
	return p, ok
}
func (h *AccountStores) ForSession(token string) (*Store, Principal, bool) {
	for _, store := range h.snapshot() {
		if p, ok := store.Current(token); ok {
			return store, p, true
		}
	}
	return nil, Principal{}, false
}
func (h *AccountStores) ForChallenge(token string) (*Store, bool) {
	for _, store := range h.snapshot() {
		store.mu.Lock()
		challenge, ok := store.pending[token]
		if ok {
			_, ok = store.challengePrincipal(challenge)
			if !ok {
				delete(store.pending, token)
			}
		}
		store.mu.Unlock()
		if ok {
			return store, true
		}
	}
	return nil, false
}
func (h *AccountStores) Logout(token string) {
	for _, store := range h.snapshot() {
		store.Logout(token)
	}
}
