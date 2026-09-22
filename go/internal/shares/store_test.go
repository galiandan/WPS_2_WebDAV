package shares

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shareStoreFixture(t *testing.T) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := New(filepath.Join(dir, "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func fixtureShareSpec() Spec {
	return Spec{OwnerID: "owner", PolicyVersion: 1, Path: "/private/folder", TargetID: "original-id", Kind: "folder", Name: "Shared folder", Binding: strings.Repeat("a", 64)}
}

func TestSharePersistenceNeverContainsClearTokensOrPasswords(t *testing.T) {
	s := shareStoreFixture(t)
	record, token, err := s.Create(fixtureShareSpec(), time.Time{}, "code-4321")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) || strings.Contains(string(data), "code-4321") {
		t.Fatal("bearer or password persisted in clear text")
	}
	if record.ExpiresAt.Sub(record.CreatedAt) != DefaultLifetime {
		t.Fatal(record.ExpiresAt, record.CreatedAt)
	}
	info, _ := os.Stat(s.file)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	reloaded, err := New(s.file)
	if err != nil {
		t.Fatal(err)
	}
	_, grant, expires, err := reloaded.Unlock(record.ID, token, "code-4321", "client")
	if err != nil {
		t.Fatal(err)
	}
	if expires.After(time.Now().Add(GrantLifetime)) || len(grant) != 64 {
		t.Fatal(expires, grant)
	}
	if _, err := reloaded.Authorize(record.ID, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(record.ID, grant); !errors.Is(err, ErrDenied) {
		t.Fatal("grant survived process boundary")
	}
}

func TestShareExpiryRevokeOwnerAndGrantBinding(t *testing.T) {
	s := shareStoreFixture(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	record, token, err := s.Create(fixtureShareSpec(), now.Add(10*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	_, cookie, expires, err := s.Unlock(record.ID, token, "", "client")
	if err != nil || !expires.Equal(record.ExpiresAt) {
		t.Fatal(expires, err)
	}
	other, _, err := s.Create(fixtureShareSpec(), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(other.ID, cookie); !errors.Is(err, ErrDenied) {
		t.Fatal("grant accepted on a different share")
	}
	if _, err := s.Authorize(record.ID, cookie); err != nil {
		t.Fatal("foreign share request invalidated the original grant")
	}
	if err := s.Revoke(record.ID, "other-owner", false); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if err := s.Revoke(record.ID, "administrator", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(record.ID, cookie); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked grant accepted")
	}
	if _, _, _, err := s.Unlock(record.ID, token, "", "client"); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked token accepted")
	}
	now = other.ExpiresAt
	if _, err := s.Active(other.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("expired share accepted")
	}
}

func TestShareWrongCredentialsAreGenericAndThrottled(t *testing.T) {
	s := shareStoreFixture(t)
	record, token, err := s.Create(fixtureShareSpec(), time.Time{}, "4321")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxAttempts; i++ {
		if _, _, _, err := s.Unlock(record.ID, token, "wrong", "client"); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
	}
	if _, _, _, err := s.Unlock(record.ID, token, "4321", "client"); !errors.Is(err, ErrDenied) {
		t.Fatal("throttle bypassed", err)
	}
	if _, _, _, err := s.Unlock(record.ID, token, "4321", "different-client"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Unlock(record.ID, strings.Repeat("0", 64), "4321", "third-client"); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestSharePersistenceFailureKeepsPriorRevocationStateAndGrants(t *testing.T) {
	s := shareStoreFixture(t)
	record, token, err := s.Create(fixtureShareSpec(), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, cookie, _, err := s.Unlock(record.ID, token, "", "client")
	if err != nil {
		t.Fatal(err)
	}
	s.write = func(string, string) (int64, error) { return 0, errors.New("disk full secret") }
	if err := s.Revoke(record.ID, "owner", false); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	if _, err := s.Authorize(record.ID, cookie); err != nil {
		t.Fatal("failed revoke changed in-memory state")
	}
	if _, token, err := s.Create(fixtureShareSpec(), time.Time{}, ""); !errors.Is(err, ErrState) || token != "" {
		t.Fatal("unsaved token exposed")
	}
	if len(s.List("owner", false)) != 1 {
		t.Fatal("unsaved share activated")
	}
}

func TestInvalidShareFieldsAndLifetimeFailBeforePersistence(t *testing.T) {
	s := shareStoreFixture(t)
	for _, expiry := range []time.Time{time.Now().Add(-time.Second), time.Now().Add(MaxLifetime + time.Hour)} {
		if _, _, err := s.Create(fixtureShareSpec(), expiry, ""); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	for _, password := range []string{"123", strings.Repeat("x", 129), "new\nline"} {
		if _, _, err := s.Create(fixtureShareSpec(), time.Time{}, password); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	spec := fixtureShareSpec()
	spec.Path = "/../outside"
	if _, _, err := s.Create(spec, time.Time{}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	spec = fixtureShareSpec()
	spec.Binding = ""
	if _, _, err := s.Create(spec, time.Time{}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestShareGrantAndRecordBounds(t *testing.T) {
	s := shareStoreFixture(t)
	record, token, err := s.Create(fixtureShareSpec(), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxGrants; i++ {
		var key [32]byte
		key[0] = byte(i)
		key[1] = byte(i >> 8)
		s.grants[key] = grant{ShareID: record.ID, ExpiresAt: time.Now().Add(time.Hour)}
	}
	if _, _, _, err := s.Unlock(record.ID, token, "", "client"); !errors.Is(err, ErrBusy) {
		t.Fatal("grant limit ignored", err)
	}
	s.records = make([]Record, MaxOwnerRecords)
	for i := range s.records {
		s.records[i] = record
	}
	if _, _, err := s.Create(fixtureShareSpec(), time.Time{}, ""); !errors.Is(err, ErrLimit) {
		t.Fatal("owner limit ignored", err)
	}
}

func TestOwnerValidationFailureDoesNotIssueGrant(t *testing.T) {
	s := shareStoreFixture(t)
	record, token, err := s.Create(fixtureShareSpec(), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, cookie, _, err := s.Unlock(record.ID, token, "", "client", func(Record) error { return errors.New("owner disabled") }); !errors.Is(err, ErrDenied) || cookie != "" {
		t.Fatal(err)
	}
	if len(s.grants) != 0 {
		t.Fatal("failed owner validation consumed a grant slot")
	}
}

func TestPersistedSharePreservesFullAdministratorPolicyGeneration(t *testing.T) {
	s := shareStoreFixture(t)
	spec := fixtureShareSpec()
	spec.OwnerID = "installation"
	spec.PolicyVersion = ^uint64(0) - 2
	record, token, err := s.Create(spec, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(s.file)
	if err != nil {
		t.Fatal(err)
	}
	loaded, _, _, err := reloaded.Unlock(record.ID, token, "", "client")
	if err != nil || loaded.Spec.PolicyVersion != spec.PolicyVersion {
		t.Fatalf("policy=%d wanted=%d err=%v", loaded.Spec.PolicyVersion, spec.PolicyVersion, err)
	}
}

func TestRevocationDuringUnlockNeverIssuesGrant(t *testing.T) {
	s := shareStoreFixture(t)
	record, token, err := s.Create(fixtureShareSpec(), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := s.Unlock(record.ID, token, "", "client", func(Record) error { close(started); <-release; return nil })
		done <- err
	}()
	<-started
	if err := s.Revoke(record.ID, "owner", false); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrDenied) {
		t.Fatal("revocation raced grant issuance", err)
	}
	if len(s.grants) != 0 {
		t.Fatal("revoked share retained a new grant")
	}
}

func TestShareStateRejectsCorruptionAndUnsafePermissions(t *testing.T) {
	s := shareStoreFixture(t)
	for _, body := range []string{`{"version":2,"records":[]}`, `{"version":1,"records":[{}]}`, strings.Repeat("x", MaxStateBytes+1)} {
		if err := os.WriteFile(s.file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(s.file); !errors.Is(err, ErrState) {
			t.Fatal("invalid share state accepted", err)
		}
	}
	if err := os.WriteFile(s.file, []byte(`{"version":1,"records":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(s.file); !errors.Is(err, ErrState) {
		t.Fatal("publicly readable share state accepted", err)
	}
}
