package auth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func passkeyBytes(value []byte) []byte {
	if len(value) < 24 {
		return append([]byte{0x40 | byte(len(value))}, value...)
	}
	return append([]byte{0x58, byte(len(value))}, value...)
}

func registrationFixture(t *testing.T, challenge string, extensions bool) (PasskeyCredential, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// COSE EC2 / ES256 / P-256 key, encoded independently of the production decoder.
	cose := []byte{0xa5, 1, 2, 3, 0x26, 0x20, 1, 0x21}
	cose = append(cose, passkeyBytes(key.X.FillBytes(make([]byte, 32)))...)
	cose = append(cose, 0x22)
	cose = append(cose, passkeyBytes(key.Y.FillBytes(make([]byte, 32)))...)
	hash := sha256.Sum256([]byte("example.test"))
	data := append(hash[:], 0x41, 0, 0, 0, 0)
	data = append(data, make([]byte, 16)...)
	id := []byte("test-credential")
	data = append(data, 0, byte(len(id)))
	data = append(data, id...)
	data = append(data, cose...)
	if extensions {
		data[32] |= 0x80
		// Authenticator-generated credProtect extension, independent of client extensions.
		data = append(data, 0xa1, 0x6b)
		data = append(data, []byte("credProtect")...)
		data = append(data, 2)
	}
	attestation := append([]byte{0xa3, 0x63}, []byte("fmt")...)
	attestation = append(attestation, 0x64)
	attestation = append(attestation, []byte("none")...)
	attestation = append(attestation, 0x67)
	attestation = append(attestation, []byte("attStmt")...)
	attestation = append(attestation, 0xa0, 0x68)
	attestation = append(attestation, []byte("authData")...)
	attestation = append(attestation, passkeyBytes(data)...)
	payload := PasskeyCredential{ID: base64.RawURLEncoding.EncodeToString(id), RawID: base64.RawURLEncoding.EncodeToString(id), Type: "public-key"}
	client, _ := json.Marshal(map[string]string{"type": "webauthn.create", "challenge": challenge, "origin": "https://example.test"})
	payload.Response.ClientDataJSON = base64.RawURLEncoding.EncodeToString(client)
	payload.Response.AttestationObject = base64.RawURLEncoding.EncodeToString(attestation)
	return payload, key
}

func TestPasskeyRegistrationPersistenceAndDERLogin(t *testing.T) {
	for _, extensions := range []bool{false, true} {
		name := "plain"
		if extensions {
			name = "extensions"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "auth.json")
			source := func() (string, string) { return "alice", "password" }
			store, err := NewPersistentStore(source, path)
			if err != nil {
				t.Fatal(err)
			}
			token, _, err := store.Login("alice", "password")
			if err != nil {
				t.Fatal(err)
			}
			options, err := store.BeginPasskeyRegistration(token, "alice", "example.test")
			if err != nil {
				t.Fatal(err)
			}
			payload, key := registrationFixture(t, options.Challenge, extensions)
			if err := store.RegisterPasskey(token, options.Challenge, payload, "Test", "example.test", "https://example.test"); err != nil {
				t.Fatal(err)
			}
			store, err = NewPersistentStore(source, path)
			if err != nil || store.PasskeyCount() != 1 {
				t.Fatalf("reload: %v", err)
			}
			login, err := store.BeginPasskeyLogin("example.test")
			if err != nil {
				t.Fatal(err)
			}
			client, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": login.Challenge, "origin": "https://example.test"})
			hash := sha256.Sum256([]byte("example.test"))
			data := append(hash[:], 1, 0, 0, 0, 1)
			clientHash := sha256.Sum256(client)
			digest := sha256.Sum256(append(append([]byte{}, data...), clientHash[:]...))
			signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			payload.Response.ClientDataJSON = base64.RawURLEncoding.EncodeToString(client)
			payload.Response.AuthenticatorData = base64.RawURLEncoding.EncodeToString(data)
			payload.Response.Signature = base64.RawURLEncoding.EncodeToString(signature)
			saved, _ := decodeB64(store.factorState.Passkeys[0].PublicKey)
			damaged := bytes.Clone(signature)
			damaged[len(damaged)-1] ^= 1
			if verifyCOSESignature(saved, data, client, damaged) {
				t.Fatal("accepted invalid signature")
			}
			token, user, err := store.VerifyPasskeyLogin(login.Challenge, payload, "example.test", "https://example.test")
			if err != nil || token == "" || user.Username != "alice" {
				t.Fatalf("login failed: %v", err)
			}
		})
	}
}

func TestPasskeyRegistrationStorageFailureAndRetry(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewPersistentStore(func() (string, string) { return "alice", "password" }, filepath.Join(dir, "missing", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := store.Login("alice", "password")
	if _, err := store.BeginPasskeyRegistration(token, "alice", "example.test"); !errors.Is(err, ErrFactorState) {
		t.Fatalf("preflight error: %v", err)
	}
	if len(store.pending) != 0 {
		t.Fatal("created challenge despite storage failure")
	}
	store.securityPath = filepath.Join(dir, "auth.json")
	options, err := store.BeginPasskeyRegistration(token, "alice", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := registrationFixture(t, options.Challenge, false)
	store.securityPath = filepath.Join(dir, "missing", "auth.json")
	if err := store.RegisterPasskey(token, options.Challenge, payload, "Test", "example.test", "https://example.test"); !errors.Is(err, ErrFactorState) {
		t.Fatalf("save error: %v", err)
	}
	if store.PasskeyCount() != 0 {
		t.Fatal("failed save retained credential")
	}
	store.securityPath = filepath.Join(dir, "auth.json")
	if err := store.RegisterPasskey(token, options.Challenge, payload, "Test", "example.test", "https://example.test"); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestAttestedCredentialExtensionsAreBounded(t *testing.T) {
	payload, _ := registrationFixture(t, "test", true)
	encoded, _ := decodeB64(payload.Response.AttestationObject)
	root, err := decodeCBOR(encoded)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := root.mapBytes("authData")
	if _, _, err := parseAttestedCredentialData(data); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{data[:len(data)-1], append(bytes.Clone(data), 0), data[:len(data)-14]} {
		if _, _, err := parseAttestedCredentialData(invalid); err == nil {
			t.Fatal("accepted malformed extensions")
		}
	}
	data[32] &= ^byte(0x80)
	if _, _, err := parseAttestedCredentialData(data); err == nil {
		t.Fatal("accepted unflagged extensions")
	}
}
