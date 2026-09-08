package auth

import (
	"testing"
	"time"
)

func TestTOTPSetupRequiresAValidFirstCodeAndRecoveryCodesAreOneTime(t *testing.T) {
	store := NewStore(func() (string, string) { return "alice", "password" })
	setup, err := store.BeginTOTPSetup("alice")
	if err != nil || setup.Secret == "" || setup.OTPAuthURI == "" {
		t.Fatalf("setup = %+v, %v", setup, err)
	}
	if _, err := store.EnableTOTP(setup.Secret, "000000"); err != ErrInvalidTwoFactor {
		t.Fatalf("invalid setup code error = %v", err)
	}
	code := totpCode(setup.Secret, uint64(store.now().Unix()/totpPeriod))
	recovery, err := store.EnableTOTP(setup.Secret, code)
	if err != nil || len(recovery) != 8 {
		t.Fatalf("enable = %v, recovery=%v", err, recovery)
	}
	if !store.TwoFactorEnabled() {
		t.Fatal("TOTP was not enabled")
	}
	login, err := store.LoginWithFactors("alice", "password")
	if err != nil || !login.TwoFactorRequired || login.Challenge == "" {
		t.Fatalf("factor login = %+v, %v", login, err)
	}
	token, user, err := store.VerifyTwoFactor(login.Challenge, recovery[0])
	if err != nil || token == "" || user.Username != "alice" {
		t.Fatalf("recovery login = %q, %+v, %v", token, user, err)
	}
	second, err := store.LoginWithFactors("alice", "password")
	if err != nil || !second.TwoFactorRequired {
		t.Fatalf("second factor login = %+v, %v", second, err)
	}
	if _, _, err := store.VerifyTwoFactor(second.Challenge, recovery[0]); err != ErrInvalidTwoFactor {
		t.Fatalf("reused recovery code error = %v", err)
	}
	if _, _, err := store.VerifyTwoFactor(second.Challenge, totpCode(setup.Secret, uint64(store.now().Unix()/totpPeriod))); err != nil {
		t.Fatalf("TOTP login = %v", err)
	}
}

func TestTOTPDisablingRequiresAValidFactor(t *testing.T) {
	store := NewStore(func() (string, string) { return "alice", "password" })
	secret := "JBSWY3DPEHPK3PXP"
	code := totpCode(secret, uint64(time.Now().Unix()/totpPeriod))
	if _, err := store.EnableTOTP(secret, code); err != nil {
		t.Fatal(err)
	}
	if err := store.DisableTOTP("000000"); err != ErrInvalidTwoFactor {
		t.Fatalf("invalid disable error = %v", err)
	}
	if err := store.DisableTOTP(totpCode(secret, uint64(time.Now().Unix()/totpPeriod))); err != nil {
		t.Fatalf("disable = %v", err)
	}
	if store.TwoFactorEnabled() {
		t.Fatal("TOTP remained enabled")
	}
}
