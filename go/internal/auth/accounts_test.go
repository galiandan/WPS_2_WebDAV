package auth

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type accountProviderFake struct {
	mu    sync.Mutex
	users map[string]Principal
}

func (p *accountProviderFake) LookupID(id string) (Principal, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u, ok := p.users[id]
	return u, ok
}
func (p *accountProviderFake) LookupUsername(name string) (Principal, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range p.users {
		if u.Username == name {
			return u, true
		}
	}
	return Principal{}, false
}
func (p *accountProviderFake) Authenticate(name, password string) (Principal, error) {
	if password != "password" {
		return Principal{}, ErrInvalidCredentials
	}
	if u, ok := p.LookupUsername(name); ok {
		return u, nil
	}
	return Principal{}, ErrInvalidCredentials
}
func (p *accountProviderFake) bump(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.users[id]
	u.PolicyVersion++
	p.users[id] = u
}
func accountHubFixture(t *testing.T) (*AccountStores, *accountProviderFake) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	admin, err := NewPersistentStore(func() (string, string) { return "admin", "password" }, filepath.Join(dir, "auth-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &accountProviderFake{users: map[string]Principal{
		InstallationID:                     {ID: InstallationID, Username: "admin", Role: "admin", PolicyVersion: 1},
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Username: "alice", Role: "member", PolicyVersion: 1},
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": {ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Username: "bob", Role: "member", PolicyVersion: 1},
	}}
	hub, err := NewAccountStores(admin, provider)
	if err != nil {
		t.Fatal(err)
	}
	return hub, provider
}

func TestAccountStoresKeepFactorsAndSessionsIsolated(t *testing.T) {
	hub, _ := accountHubFixture(t)
	alice, err := hub.ForUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := hub.ForUsername("bob")
	if err != nil {
		t.Fatal(err)
	}
	setup, err := alice.BeginTOTPSetup("alice")
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(setup.Secret, uint64(alice.now().Unix()/totpPeriod))
	recovery, err := alice.EnableTOTP(setup.Secret, code)
	if err != nil {
		t.Fatal(err)
	}
	if !alice.TwoFactorEnabled() || bob.TwoFactorEnabled() || hub.admin.TwoFactorEnabled() {
		t.Fatal("member TOTP affected another account")
	}
	login, err := alice.LoginWithFactors("alice", "password")
	if err != nil || !login.TwoFactorRequired {
		t.Fatalf("login=%+v err=%v", login, err)
	}
	if _, _, err := bob.VerifyTwoFactor(login.Challenge, recovery[0]); !errors.Is(err, ErrFactorChallengeExpired) {
		t.Fatal("foreign factor challenge accepted", err)
	}
	owner, ok := hub.ForChallenge(login.Challenge)
	if !ok || owner != alice {
		t.Fatal("challenge routed to wrong store")
	}
	token, user, err := owner.VerifyTwoFactor(login.Challenge, recovery[0])
	if err != nil || user.Username != "alice" {
		t.Fatalf("user=%+v err=%v", user, err)
	}
	if p, ok := hub.Current(token); !ok || p.ID != user.ID {
		t.Fatal("session did not retain member identity")
	}
	if _, ok := bob.Current(token); ok {
		t.Fatal("member cookie resolved in another account")
	}
	if info, err := os.Stat(alice.securityPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("factor mode=%v err=%v", info, err)
	}
	reloadedAdmin, err := NewPersistentStore(func() (string, string) { return "admin", "password" }, hub.admin.securityPath)
	if err != nil {
		t.Fatal(err)
	}
	reloadedHub, err := NewAccountStores(reloadedAdmin, hub.provider)
	if err != nil {
		t.Fatal(err)
	}
	reloadedAlice, err := reloadedHub.ForUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	reloadedBob, err := reloadedHub.ForUsername("bob")
	if err != nil {
		t.Fatal(err)
	}
	if !reloadedAlice.TwoFactorEnabled() || reloadedBob.TwoFactorEnabled() || reloadedAdmin.TwoFactorEnabled() {
		t.Fatal("persisted factor files crossed account boundaries")
	}
}

func TestAccountPolicyChangeRevokesSessionsAndPendingFactors(t *testing.T) {
	hub, provider := accountHubFixture(t)
	alice, _ := hub.ForUsername("alice")
	token, user, err := alice.Login("alice", "password")
	if err != nil {
		t.Fatal(err)
	}
	setup, _ := alice.BeginTOTPSetup("alice")
	_, err = alice.EnableTOTP(setup.Secret, totpCode(setup.Secret, uint64(alice.now().Unix()/totpPeriod)))
	if err != nil {
		t.Fatal(err)
	}
	login, err := alice.LoginWithFactors("alice", "password")
	if err != nil {
		t.Fatal(err)
	}
	provider.bump(user.ID)
	if _, ok := hub.Current(token); ok {
		t.Fatal("old policy session remained valid")
	}
	if _, ok := hub.ForChallenge(login.Challenge); ok {
		t.Fatal("old policy factor challenge remained valid")
	}
	if _, _, err := alice.VerifyTwoFactor(login.Challenge, totpCode(setup.Secret, uint64(alice.now().Unix()/totpPeriod))); !errors.Is(err, ErrFactorChallengeExpired) {
		t.Fatal(err)
	}
	if next, err := alice.LoginWithFactors("alice", "password"); err != nil || !next.TwoFactorRequired {
		t.Fatal("factor state disappeared after policy change")
	}
}

func TestMemberPasskeyRegistrationCannotUseAnotherUsersSession(t *testing.T) {
	hub, _ := accountHubFixture(t)
	alice, _ := hub.ForUsername("alice")
	bob, _ := hub.ForUsername("bob")
	token, _, err := alice.Login("alice", "password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.BeginPasskeyRegistration(token, "bob", "example.test"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("foreign cookie registered member key", err)
	}
	options, err := alice.BeginPasskeyRegistration(token, "alice", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := registrationFixture(t, options.Challenge, false)
	if err := alice.RegisterPasskey(token, options.Challenge, credential, "Alice key", "example.test", "https://example.test"); err != nil {
		t.Fatal(err)
	}
	if alice.PasskeyCount() != 1 || bob.PasskeyCount() != 0 || hub.admin.PasskeyCount() != 0 {
		t.Fatal("passkey lists are not isolated")
	}
	if err := bob.DeletePasskey(credential.ID); !errors.Is(err, ErrPasskeyNotConfigured) {
		t.Fatal("another user deleted key", err)
	}
}
