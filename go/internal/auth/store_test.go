package auth

import (
	"errors"
	"testing"
	"time"
)

func TestStoreUsesInstallationCredentialsAndKeepsNoDatabase(t *testing.T) {
	username, password := "adapter", "installation secret"
	store := NewStore(func() (string, string) { return username, password })

	if _, _, err := store.Login("other", password); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong username error = %v", err)
	}
	if _, _, err := store.Login(username, "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v", err)
	}
	token, user, err := store.Login(username, password)
	if err != nil || token == "" || user.Username != username {
		t.Fatalf("login = %q, %+v, %v", token, user, err)
	}
	if current, ok := store.Current(token); !ok || current.Username != username {
		t.Fatalf("current = %+v, %v", current, ok)
	}

	username, password = "new-adapter", "new secret"
	if _, _, err := store.Login("adapter", "installation secret"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old credentials remained valid: %v", err)
	}
	if _, _, err := store.Login("new-adapter", "new secret"); err != nil {
		t.Fatalf("reloaded credentials failed: %v", err)
	}
}

func TestStoreExpiresAndLogsOutSessions(t *testing.T) {
	store := NewStore(func() (string, string) { return "adapter", "long secret" })
	token, _, err := store.Login("adapter", "long secret")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.now = func() time.Time { return time.Now().Add(SessionMaxAge + time.Second) }
	store.mu.Unlock()
	if _, ok := store.Current(token); ok {
		t.Fatal("expired session was accepted")
	}

	store = NewStore(func() (string, string) { return "adapter", "long secret" })
	token, _, err = store.Login("adapter", "long secret")
	if err != nil {
		t.Fatal(err)
	}
	store.Logout(token)
	if _, ok := store.Current(token); ok {
		t.Fatal("logged out session was accepted")
	}
}
