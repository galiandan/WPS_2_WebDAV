package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegisterPersistsOnlyPasswordHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "users.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.Register("alice@example.com", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID == "" || user.Username != "alice@example.com" {
		t.Fatalf("user = %+v", user)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "correct horse battery") {
		t.Fatal("password was persisted in plaintext")
	}
	if !strings.Contains(string(raw), "pbkdf2-sha256") {
		t.Fatal("password hash is missing")
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reloaded.Login("alice@example.com", "wrong password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v", err)
	}
	token, loggedIn, err := reloaded.Login("ALICE@EXAMPLE.COM", "correct horse battery")
	if err != nil || token == "" || loggedIn.ID != user.ID {
		t.Fatalf("login = %q, %+v, %v", token, loggedIn, err)
	}
	if current, ok := reloaded.Current(token); !ok || current.ID != user.ID {
		t.Fatalf("current = %+v, %v", current, ok)
	}
}

func TestStoreValidatesRegistrationAndExpiresSessions(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register("ab", "long enough password"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short username error = %v", err)
	}
	if _, err := store.Register("alice", "short"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short password error = %v", err)
	}
	if _, err := store.Register("alice", "long enough password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register("ALICE", "another password"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate error = %v", err)
	}
	token, _, err := store.Login("alice", "long enough password")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.now = func() time.Time { return time.Now().Add(SessionMaxAge + time.Second) }
	store.mu.Unlock()
	if _, ok := store.Current(token); ok {
		t.Fatal("expired session was accepted")
	}
}
