package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
)

func accountFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "users.json")
	s, err := New(file, func() (string, string) { return "admin", "admin-password" })
	if err != nil {
		t.Fatal(err)
	}
	return s, file
}
func memberInput(name string) CreateUser {
	return CreateUser{Username: name, Password: "member-password", RootPath: "/Space/folder", RootID: "original-root", RootBinding: strings.Repeat("a", 64), Permissions: auth.Permissions{Read: true, Upload: true}}
}

func TestAccountsPersistHashesPrivateStateAndStableAdminBinding(t *testing.T) {
	s, file := accountFixture(t)
	member, err := s.Create(memberInput("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if member.ID == "" || member.Role != "member" || member.PolicyVersion != 1 || !member.Enabled {
		t.Fatal(member)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "member-password") || strings.Contains(string(data), "admin-password") {
		t.Fatal("plaintext password persisted")
	}
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	before, _ := s.LookupID(auth.InstallationID)
	reloaded, err := New(file, s.admin)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := reloaded.LookupID(auth.InstallationID)
	if before.PolicyVersion != after.PolicyVersion {
		t.Fatal("admin binding changed on restart")
	}
	if user, err := reloaded.Authenticate("alice", "member-password"); err != nil || user.ID != member.ID {
		t.Fatalf("user=%+v err=%v", user, err)
	}
	if _, err := reloaded.Authenticate("alice", "wrong-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatal(err)
	}
}

func TestAccountChangesRevokeCachedPasswordAndDisabledIdentity(t *testing.T) {
	s, _ := accountFixture(t)
	member, err := s.Create(memberInput("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("alice", "member-password"); err != nil {
		t.Fatal(err)
	}
	password := "replacement-password"
	updated, err := s.Update(member.ID, UpdateUser{Password: &password})
	if err != nil {
		t.Fatal(err)
	}
	if updated.PolicyVersion == member.PolicyVersion {
		t.Fatal("policy generation did not advance")
	}
	if _, err := s.Authenticate("alice", "member-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatal("old password cache survived", err)
	}
	if _, err := s.Authenticate("alice", password); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := s.Update(member.ID, UpdateUser{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupID(member.ID); ok {
		t.Fatal("disabled member resolved")
	}
	if _, err := s.Authenticate("alice", password); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatal("disabled account authenticated", err)
	}
}

func TestAccountPersistenceFailureDoesNotActivateEdits(t *testing.T) {
	s, _ := accountFixture(t)
	member, err := s.Create(memberInput("alice"))
	if err != nil {
		t.Fatal(err)
	}
	s.write = func(string, string) (int64, error) { return 0, errors.New("disk full") }
	name := "newname"
	if _, err := s.Update(member.ID, UpdateUser{Username: &name}); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	if current, ok := s.LookupID(member.ID); !ok || current.Username != "alice" || current.PolicyVersion != 1 {
		t.Fatal(current)
	}
	if err := s.Delete(member.ID); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	if _, ok := s.LookupID(member.ID); !ok {
		t.Fatal("failed deletion activated")
	}
}

func TestAccountReservedAdminAndBounds(t *testing.T) {
	s, _ := accountFixture(t)
	if _, err := s.Create(memberInput("admin")); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := s.Update(auth.InstallationID, UpdateUser{}); !errors.Is(err, ErrImmutable) {
		t.Fatal(err)
	}
	if err := s.Delete(auth.InstallationID); !errors.Is(err, ErrImmutable) {
		t.Fatal(err)
	}
	for _, name := range []string{"", "../outside", "a b", strings.Repeat("a", 65), "用户"} {
		if validateUsername(name) == nil {
			t.Fatal("invalid name accepted", name)
		}
	}
	for _, password := range []string{"short", "with\ncontrol", strings.Repeat("x", 257)} {
		if validatePassword(password) == nil {
			t.Fatal("invalid password accepted")
		}
	}
	for i := 0; i < cap(s.verifyGate); i++ {
		s.verifyGate <- struct{}{}
	}
	if _, err := s.Create(memberInput("busy")); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	for i := 0; i < cap(s.verifyGate); i++ {
		<-s.verifyGate
	}
}

func TestAdminCredentialRotationChangesOpaquePolicyGeneration(t *testing.T) {
	s, _ := accountFixture(t)
	password := "first-password"
	s.admin = func() (string, string) { return "admin", password }
	before, _ := s.LookupID(auth.InstallationID)
	password = "second-password"
	after, _ := s.LookupID(auth.InstallationID)
	if before.PolicyVersion == after.PolicyVersion {
		t.Fatal("admin rotation did not change policy")
	}
	if _, err := s.Authenticate("admin", "first-password"); err == nil {
		t.Fatal("old admin password accepted")
	}
	if p, err := s.Authenticate("admin", password); err != nil || !p.IsAdmin() {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
}
