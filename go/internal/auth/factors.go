package auth

// This file contains the optional browser factors. TOTP follows RFC 6238
// with SHA-1 and six digits. Passkeys implement the WebAuthn ceremony using
// only the standard library so the released adapter remains dependency-free.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
)

const (
	factorStateVersion         = 1
	factorStateMaxBytes        = 256 * 1024
	factorChallengeLifetime    = 5 * time.Minute
	maxPasskeyResponseBytes    = 128 * 1024
	maxPasskeys                = 32
	maxPendingChallenges       = 128
	maxFactorAttempts          = 5
	totpDigits                 = 6
	totpPeriod                 = 30
	totpSecretBytes            = 20
	passkeyUserVerification    = "preferred"
	passkeyTimeoutMilliseconds = 60000
)

var (
	ErrTwoFactorRequired        = errors.New("two-factor authentication required")
	ErrInvalidTwoFactor         = errors.New("invalid two-factor code")
	ErrFactorChallengeExpired   = errors.New("authentication challenge expired")
	ErrPasskeyNotConfigured     = errors.New("passkey is not configured")
	ErrInvalidPasskey           = errors.New("invalid passkey assertion")
	ErrPasskeyAlreadyRegistered = errors.New("passkey is already registered")
	ErrPasskeyLimit             = errors.New("passkey limit reached")
	ErrFactorState              = errors.New("authentication settings unavailable")
)

type totpState struct {
	Secret        string   `json:"secret"`
	RecoveryCodes []string `json:"recovery_codes,omitempty"`
}

type storedPasskey struct {
	ID        string `json:"id"`
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	SignCount uint32 `json:"sign_count"`
	CreatedAt string `json:"created_at"`
}

type factorState struct {
	Version  int             `json:"version"`
	TOTP     *totpState      `json:"totp,omitempty"`
	Passkeys []storedPasskey `json:"passkeys,omitempty"`
}

type factorChallenge struct {
	Kind      string
	Challenge []byte
	Username  string
	Session   string
	RPID      string
	Origin    string
	Expires   time.Time
	Attempts  int
}

type PasswordLogin struct {
	Token             string
	User              User
	TwoFactorRequired bool
	Challenge         string
}

type TOTPSetup struct {
	Secret        string   `json:"secret"`
	OTPAuthURI    string   `json:"otpauth_uri"`
	RecoveryCodes []string `json:"recovery_codes"`
}

type PasskeyCredential struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AttestationObject string `json:"attestationObject"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
		UserHandle        string `json:"userHandle"`
	} `json:"response"`
}

type PasskeyCreationOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"rp"`
	User struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"user"`
	PubKeyCredParams       []map[string]any  `json:"pubKeyCredParams"`
	Timeout                int               `json:"timeout"`
	Attestation            string            `json:"attestation"`
	AuthenticatorSelection map[string]string `json:"authenticatorSelection"`
	ExcludeCredentials     []map[string]any  `json:"excludeCredentials,omitempty"`
}

type PasskeyRequestOptions struct {
	Challenge        string           `json:"challenge"`
	RPID             string           `json:"rpId"`
	Timeout          int              `json:"timeout"`
	UserVerification string           `json:"userVerification"`
	AllowCredentials []map[string]any `json:"allowCredentials,omitempty"`
}

func (s *Store) loadFactorState() error {
	if s.securityPath == "" {
		return nil
	}
	payload, _, err := securefile.ReadJSONState(s.securityPath, factorStateMaxBytes)
	if err != nil {
		return ErrFactorState
	}
	if payload == nil {
		s.factorState = factorState{Version: factorStateVersion}
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil || json.Unmarshal(raw, &s.factorState) != nil {
		return ErrFactorState
	}
	if s.factorState.Version == 0 {
		s.factorState.Version = factorStateVersion
	}
	if s.factorState.Version != factorStateVersion || len(s.factorState.Passkeys) > maxPasskeys {
		return ErrFactorState
	}
	return nil
}

func (s *Store) persistFactorStateLocked() error {
	if s.securityPath == "" {
		return nil
	}
	s.factorState.Version = factorStateVersion
	raw, err := json.Marshal(s.factorState)
	if err != nil {
		return ErrFactorState
	}
	if _, err := securefile.WriteAtomic(s.securityPath, string(raw)); err != nil {
		return ErrFactorState
	}
	return nil
}

// TwoFactorEnabled reports whether password login requires a TOTP/recovery
// code. Passkeys remain an independent login option.
func (s *Store) TwoFactorEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.factorState.TOTP != nil && s.factorState.TOTP.Secret != ""
}

func (s *Store) PasskeyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.factorState.Passkeys)
}

func (s *Store) BeginTOTPSetup(username string) (TOTPSetup, error) {
	if strings.TrimSpace(username) == "" {
		return TOTPSetup{}, ErrInvalidCredentials
	}
	secretBytes := make([]byte, totpSecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return TOTPSetup{}, ErrFactorState
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secretBytes)
	issuer := "WPS Enterprise Drive"
	uri := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + username}
	query := uri.Query()
	query.Set("secret", secret)
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", "6")
	query.Set("period", "30")
	uri.RawQuery = query.Encode()
	return TOTPSetup{Secret: secret, OTPAuthURI: uri.String()}, nil
}

// EnableTOTP verifies the first code before persisting the secret and returns
// one-time recovery codes. Recovery-code hashes are stored, never the codes.
func (s *Store) EnableTOTP(secret, code string) ([]string, error) {
	secret = normalizeSecret(secret)
	if !validTOTPSecret(secret) || !validTOTPCode(code, secret, s.now()) {
		return nil, ErrInvalidTwoFactor
	}
	recovery := make([]string, 8)
	hashes := make([]string, len(recovery))
	for i := range recovery {
		value, err := randomRecoveryCode()
		if err != nil {
			return nil, ErrFactorState
		}
		recovery[i] = value
		hash := sha256.Sum256([]byte(normalizeRecoveryCode(value)))
		hashes[i] = hex.EncodeToString(hash[:])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var previous *totpState
	if s.factorState.TOTP != nil {
		copyValue := *s.factorState.TOTP
		copyValue.RecoveryCodes = append([]string(nil), s.factorState.TOTP.RecoveryCodes...)
		previous = &copyValue
	}
	s.factorState.TOTP = &totpState{Secret: secret, RecoveryCodes: hashes}
	if err := s.persistFactorStateLocked(); err != nil {
		s.factorState.TOTP = previous
		return nil, err
	}
	return recovery, nil
}

func (s *Store) DisableTOTP(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.factorState.TOTP == nil {
		return ErrInvalidTwoFactor
	}
	previous := *s.factorState.TOTP
	valid, verifyErr := s.verifySecondFactorLocked(code, false)
	if verifyErr != nil || !valid {
		return ErrInvalidTwoFactor
	}
	s.factorState.TOTP = nil
	if err := s.persistFactorStateLocked(); err != nil {
		s.factorState.TOTP = &previous
		return err
	}
	return nil
}

// LoginWithFactors performs password authentication and returns a short-lived
// challenge instead of a session when TOTP is enabled.
func (s *Store) LoginWithFactors(username, password string) (PasswordLogin, error) {
	configuredUsername, configuredPassword := s.credentialsValue()
	username = strings.TrimSpace(username)
	if username == "" || configuredUsername == "" || configuredPassword == "" ||
		subtle.ConstantTimeCompare([]byte(username), []byte(configuredUsername)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(configuredPassword)) != 1 {
		return PasswordLogin{}, ErrInvalidCredentials
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.factorState.TOTP != nil && s.factorState.TOTP.Secret != "" {
		challenge, err := s.newChallengeLocked(factorChallenge{Kind: "totp", Username: configuredUsername})
		if err != nil {
			return PasswordLogin{}, err
		}
		return PasswordLogin{TwoFactorRequired: true, Challenge: challenge}, nil
	}
	token, err := s.newSessionLocked(configuredUsername)
	if err != nil {
		return PasswordLogin{}, err
	}
	return PasswordLogin{Token: token, User: User{Username: configuredUsername}}, nil
}

func (s *Store) VerifyTwoFactor(challenge, code string) (string, User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.pending[challenge]
	if !ok || pending.Kind != "totp" || !s.now().Before(pending.Expires) {
		delete(s.pending, challenge)
		return "", User{}, ErrFactorChallengeExpired
	}
	pending.Attempts++
	s.pending[challenge] = pending
	if pending.Attempts > maxFactorAttempts {
		if pending.Attempts >= maxFactorAttempts {
			delete(s.pending, challenge)
		}
		return "", User{}, ErrInvalidTwoFactor
	}
	valid, persistErr := s.verifySecondFactorLocked(code, true)
	if persistErr != nil {
		return "", User{}, persistErr
	}
	if !valid {
		if pending.Attempts >= maxFactorAttempts {
			delete(s.pending, challenge)
		}
		return "", User{}, ErrInvalidTwoFactor
	}
	delete(s.pending, challenge)
	token, err := s.newSessionLocked(pending.Username)
	if err != nil {
		return "", User{}, err
	}
	return token, User{Username: pending.Username}, nil
}

func (s *Store) verifySecondFactorLocked(code string, consumeRecovery bool) (bool, error) {
	if s.factorState.TOTP == nil {
		return false, nil
	}
	if validTOTPCode(code, s.factorState.TOTP.Secret, s.now()) {
		return true, nil
	}
	want := normalizeRecoveryCode(code)
	hash := sha256.Sum256([]byte(want))
	wantHex := hex.EncodeToString(hash[:])
	for i, stored := range s.factorState.TOTP.RecoveryCodes {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(wantHex)) == 1 {
			if !consumeRecovery {
				return true, nil
			}
			previous := append([]string(nil), s.factorState.TOTP.RecoveryCodes...)
			s.factorState.TOTP.RecoveryCodes = append(s.factorState.TOTP.RecoveryCodes[:i], s.factorState.TOTP.RecoveryCodes[i+1:]...)
			if err := s.persistFactorStateLocked(); err != nil {
				s.factorState.TOTP.RecoveryCodes = previous
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) BeginPasskeyLogin(rpID string) (PasskeyRequestOptions, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.factorState.Passkeys) == 0 {
		return PasskeyRequestOptions{}, ErrPasskeyNotConfigured
	}
	challenge, err := s.newChallengeLocked(factorChallenge{Kind: "passkey-login", RPID: rpID})
	if err != nil {
		return PasskeyRequestOptions{}, err
	}
	options := PasskeyRequestOptions{
		Challenge:        challenge,
		RPID:             rpID,
		Timeout:          passkeyTimeoutMilliseconds,
		UserVerification: passkeyUserVerification,
	}
	for _, credential := range s.factorState.Passkeys {
		options.AllowCredentials = append(options.AllowCredentials, map[string]any{
			"type": "public-key", "id": credential.ID,
		})
	}
	return options, nil
}

func (s *Store) VerifyPasskeyLogin(challengeToken string, payload PasskeyCredential, rpID, origin string) (string, User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	challenge, err := s.findChallengeLocked(challengeToken, "passkey-login")
	if err != nil {
		return "", User{}, err
	}
	if challenge.RPID != rpID {
		return "", User{}, ErrInvalidPasskey
	}
	credentialID := payload.RawID
	if credentialID == "" {
		credentialID = payload.ID
	}
	previousCount := uint32(0)
	for _, item := range s.factorState.Passkeys {
		if item.ID == credentialID {
			previousCount = item.SignCount
			break
		}
	}
	credential, err := s.verifyAssertionLocked(payload, challenge, rpID, origin)
	if err != nil {
		return "", User{}, err
	}
	delete(s.pending, challenge.ChallengeString)
	if err := s.persistFactorStateLocked(); err != nil {
		for index := range s.factorState.Passkeys {
			if s.factorState.Passkeys[index].ID == credential.ID {
				s.factorState.Passkeys[index].SignCount = previousCount
				break
			}
		}
		s.pending[challenge.ChallengeString] = challenge.factorChallenge
		return "", User{}, err
	}
	token, err := s.newSessionLocked(credential.Username)
	if err != nil {
		return "", User{}, err
	}
	return token, User{Username: credential.Username}, nil
}

// BeginPasskeyRegistration starts a ceremony tied to the current session.
func (s *Store) BeginPasskeyRegistration(sessionToken, username, rpID string) (PasskeyCreationOptions, error) {
	if _, ok := s.Current(sessionToken); !ok {
		return PasskeyCreationOptions{}, ErrInvalidCredentials
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.factorState.Passkeys) >= maxPasskeys {
		return PasskeyCreationOptions{}, ErrPasskeyLimit
	}
	challenge, err := s.newChallengeLocked(factorChallenge{Kind: "passkey-register", Username: username, Session: sessionToken, RPID: rpID})
	if err != nil {
		return PasskeyCreationOptions{}, err
	}
	var options PasskeyCreationOptions
	options.Challenge = challenge
	options.RP.Name = "WPS Enterprise Drive"
	options.RP.ID = rpID
	userHash := sha256.Sum256([]byte(username))
	options.User.ID = base64.RawURLEncoding.EncodeToString(userHash[:16])
	options.User.Name = username
	options.User.DisplayName = username
	options.PubKeyCredParams = []map[string]any{
		{"type": "public-key", "alg": -7},
		{"type": "public-key", "alg": -8},
		{"type": "public-key", "alg": -257},
	}
	options.Timeout = passkeyTimeoutMilliseconds
	options.Attestation = "none"
	options.AuthenticatorSelection = map[string]string{"residentKey": "preferred", "userVerification": passkeyUserVerification}
	for _, credential := range s.factorState.Passkeys {
		options.ExcludeCredentials = append(options.ExcludeCredentials, map[string]any{"type": "public-key", "id": credential.ID})
	}
	return options, nil
}

func (s *Store) RegisterPasskey(sessionToken, challengeToken string, payload PasskeyCredential, name, rpID, origin string) error {
	if _, ok := s.Current(sessionToken); !ok {
		return ErrInvalidCredentials
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	challenge, ok := s.pending[challengeToken]
	if !ok || challenge.Kind != "passkey-register" || challenge.Session != sessionToken || !s.now().Before(challenge.Expires) {
		return ErrFactorChallengeExpired
	}
	if challenge.RPID != rpID {
		return ErrInvalidPasskey
	}
	credential, err := parseAttestation(payload, challengeMatch{ChallengeString: challengeToken, factorChallenge: challenge}, rpID, origin)
	if err != nil {
		return err
	}
	for _, existing := range s.factorState.Passkeys {
		if existing.ID == credential.ID {
			return ErrPasskeyAlreadyRegistered
		}
	}
	if strings.TrimSpace(name) == "" {
		name = "Passkey"
	}
	previous := append([]storedPasskey(nil), s.factorState.Passkeys...)
	s.factorState.Passkeys = append(s.factorState.Passkeys, storedPasskey{
		ID: credential.ID, PublicKey: credential.PublicKey, Name: name,
		Username:  credential.Username,
		CreatedAt: s.now().UTC().Format(time.RFC3339),
	})
	delete(s.pending, challengeToken)
	if err := s.persistFactorStateLocked(); err != nil {
		s.factorState.Passkeys = previous
		return err
	}
	return nil
}

func (s *Store) Passkeys() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]map[string]any, 0, len(s.factorState.Passkeys))
	for _, item := range s.factorState.Passkeys {
		result = append(result, map[string]any{"id": item.ID, "name": item.Name, "created_at": item.CreatedAt})
	}
	return result
}

func (s *Store) DeletePasskey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.factorState.Passkeys {
		if item.ID == id {
			previous := append([]storedPasskey(nil), s.factorState.Passkeys...)
			s.factorState.Passkeys = append(s.factorState.Passkeys[:i], s.factorState.Passkeys[i+1:]...)
			if err := s.persistFactorStateLocked(); err != nil {
				s.factorState.Passkeys = previous
				return err
			}
			return nil
		}
	}
	return ErrPasskeyNotConfigured
}

func (s *Store) newSessionLocked(username string) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", errors.New("login failed")
	}
	s.sessions[token] = session{Username: username, Expires: s.now().Add(SessionMaxAge)}
	return token, nil
}

func (s *Store) newChallengeLocked(value factorChallenge) (string, error) {
	now := s.now()
	for key, pending := range s.pending {
		if !now.Before(pending.Expires) {
			delete(s.pending, key)
		}
	}
	if len(s.pending) >= maxPendingChallenges {
		return "", ErrFactorState
	}
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return "", ErrFactorState
	}
	encoded := base64.RawURLEncoding.EncodeToString(challenge)
	value.Challenge = challenge
	value.Expires = now.Add(factorChallengeLifetime)
	s.pending[encoded] = value
	return encoded, nil
}

func (s *Store) currentUsername() string {
	username, _ := s.credentialsValue()
	return username
}

type challengeMatch struct {
	ChallengeString string
	factorChallenge
}

func (s *Store) findChallengeLocked(token, kind string) (challengeMatch, error) {
	challenge, ok := s.pending[token]
	if !ok || challenge.Kind != kind || !s.now().Before(challenge.Expires) {
		delete(s.pending, token)
		return challengeMatch{}, ErrFactorChallengeExpired
	}
	return challengeMatch{ChallengeString: token, factorChallenge: challenge}, nil
}

func validTOTPSecret(secret string) bool {
	if len(secret) < 16 || len(secret) > 64 {
		return false
	}
	_, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalizeSecret(secret))
	return err == nil
}

func normalizeSecret(secret string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
}

func validTOTPCode(code, secret string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	for offset := -1; offset <= 1; offset++ {
		counter := uint64(now.Unix()/totpPeriod + int64(offset))
		if offset < 0 && now.Unix()/totpPeriod == 0 {
			continue
		}
		value := totpCode(secret, counter)
		if subtle.ConstantTimeCompare([]byte(code), []byte(value)) == 1 {
			return true
		}
	}
	return false
}

func totpCode(secret string, counter uint64) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalizeSecret(secret))
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)
	h := hmac.New(sha1Hash, key)
	_, _ = h.Write(counterBytes[:])
	sum := h.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	number := (uint32(sum[offset])&0x7f)<<24 | uint32(sum[offset+1])<<16 | uint32(sum[offset+2])<<8 | uint32(sum[offset+3])
	return fmt.Sprintf("%06d", number%1000000)
}

// sha1Hash is kept as a function value so the TOTP implementation's use of
// SHA-1 is explicit and limited to RFC 6238; passkey signatures use SHA-256.
func sha1Hash() hash.Hash { return sha1.New() }

func randomRecoveryCode() (string, error) {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	value := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	return value[:4] + "-" + value[4:8] + "-" + value[8:12], nil
}

func normalizeRecoveryCode(value string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
}

// The following WebAuthn helpers intentionally accept only attestation
// format "none". Registration still receives the authenticator's public key;
// enterprise attestation is not needed for this single-user adapter.
func parseAttestation(payload PasskeyCredential, challenge challengeMatch, rpID, origin string) (parsedCredential, error) {
	if payload.Type != "public-key" {
		return parsedCredential{}, ErrInvalidPasskey
	}
	clientData, err := decodeB64(payload.Response.ClientDataJSON)
	if err != nil || !validateClientData(clientData, "webauthn.create", base64.RawURLEncoding.EncodeToString(challenge.Challenge), origin) {
		return parsedCredential{}, ErrInvalidPasskey
	}
	attestation, err := decodeB64(payload.Response.AttestationObject)
	if err != nil {
		return parsedCredential{}, ErrInvalidPasskey
	}
	root, err := decodeCBOR(attestation)
	if err != nil {
		return parsedCredential{}, ErrInvalidPasskey
	}
	authData, ok := root.mapBytes("authData")
	if !ok || len(authData) < 37 || !verifyRelyingParty(authData, rpID) || authData[32]&0x01 == 0 || authData[32]&0x40 == 0 {
		return parsedCredential{}, ErrInvalidPasskey
	}
	credentialID, publicKey, err := parseAttestedCredentialData(authData)
	if err != nil {
		return parsedCredential{}, ErrInvalidPasskey
	}
	return parsedCredential{ID: base64.RawURLEncoding.EncodeToString(credentialID), PublicKey: base64.RawURLEncoding.EncodeToString(publicKey), Username: challenge.Username}, nil
}

type parsedCredential struct {
	ID        string
	PublicKey string
	Username  string
}

func (s *Store) verifyAssertionLocked(payload PasskeyCredential, challenge challengeMatch, rpID, origin string) (storedPasskey, error) {
	if payload.Type != "public-key" {
		return storedPasskey{}, ErrInvalidPasskey
	}
	clientData, err := decodeB64(payload.Response.ClientDataJSON)
	if err != nil || !validateClientData(clientData, "webauthn.get", base64.RawURLEncoding.EncodeToString(challenge.Challenge), origin) {
		return storedPasskey{}, ErrInvalidPasskey
	}
	credentialID := payload.RawID
	if credentialID == "" {
		credentialID = payload.ID
	}
	credentialID = strings.TrimSpace(credentialID)
	authenticatorData, err := decodeB64(payload.Response.AuthenticatorData)
	if err != nil || len(authenticatorData) < 37 || !verifyRelyingParty(authenticatorData, rpID) || authenticatorData[32]&0x01 == 0 {
		return storedPasskey{}, ErrInvalidPasskey
	}
	signature, err := decodeB64(payload.Response.Signature)
	if err != nil {
		return storedPasskey{}, ErrInvalidPasskey
	}
	var found *storedPasskey
	for i := range s.factorState.Passkeys {
		if s.factorState.Passkeys[i].ID == credentialID {
			found = &s.factorState.Passkeys[i]
			break
		}
	}
	if found == nil {
		return storedPasskey{}, ErrInvalidPasskey
	}
	publicKey, err := decodeB64(found.PublicKey)
	if err != nil || !verifyCOSESignature(publicKey, authenticatorData, clientData, signature) {
		return storedPasskey{}, ErrInvalidPasskey
	}
	count := binary.BigEndian.Uint32(authenticatorData[33:37])
	if found.SignCount != 0 && count != 0 && count <= found.SignCount {
		return storedPasskey{}, ErrInvalidPasskey
	}
	if count > found.SignCount {
		found.SignCount = count
	}
	return *found, nil
}

func validateClientData(data []byte, expectedType, expectedChallenge, expectedOrigin string) bool {
	var value struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Origin    string `json:"origin"`
	}
	if json.Unmarshal(data, &value) != nil || value.Type != expectedType || value.Origin != expectedOrigin {
		return false
	}
	challenge, err := base64.RawURLEncoding.DecodeString(value.Challenge)
	expected, expectedErr := base64.RawURLEncoding.DecodeString(expectedChallenge)
	return err == nil && expectedErr == nil && subtle.ConstantTimeCompare(challenge, expected) == 1
}

func verifyRelyingParty(authenticatorData []byte, rpID string) bool {
	hash := sha256.Sum256([]byte(rpID))
	return subtle.ConstantTimeCompare(authenticatorData[:32], hash[:]) == 1
}

func verifyCOSESignature(coseKey, authenticatorData, clientData, signature []byte) bool {
	key, err := decodeCBOR(coseKey)
	if err != nil {
		return false
	}
	clientHash := sha256.Sum256(clientData)
	message := append(append([]byte(nil), authenticatorData...), clientHash[:]...)
	messageHash := sha256.Sum256(message)
	alg, algOK := key.mapInt(3)
	kty, ktyOK := key.mapInt(1)
	if !algOK || !ktyOK || alg == nil || kty == nil {
		return false
	}
	algorithm, okAlg := alg.int64Value()
	keyType, okType := kty.int64Value()
	if !okAlg || !okType {
		return false
	}
	switch {
	case keyType == 2 && algorithm == -7:
		return verifyECDSA(key, messageHash[:], signature)
	case keyType == 1 && algorithm == -8:
		return verifyEd25519(key, message, signature)
	case keyType == 3 && algorithm == -257:
		return verifyRSA(key, messageHash[:], signature)
	}
	return false
}

func verifyECDSA(key cborValue, digest, signature []byte) bool {
	x, okX := key.mapInt(-2)
	y, okY := key.mapInt(-3)
	if !okX || !okY || x == nil || y == nil || x.kind != cborBytes || y.kind != cborBytes {
		return false
	}
	public := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x.bytes), Y: new(big.Int).SetBytes(y.bytes)}
	if !public.Curve.IsOnCurve(public.X, public.Y) || len(signature) != 64 {
		return false
	}
	return ecdsa.Verify(public, digest, new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
}

func verifyEd25519(key cborValue, message, signature []byte) bool {
	value, ok := key.mapInt(-2)
	if !ok || value == nil || value.kind != cborBytes || len(value.bytes) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(value.bytes), message, signature)
}

func verifyRSA(key cborValue, digest, signature []byte) bool {
	n, okN := key.mapInt(-1)
	e, okE := key.mapInt(-2)
	if !okN || !okE || n == nil || e == nil || n.kind != cborBytes || e.kind != cborBytes {
		return false
	}
	exponent := new(big.Int).SetBytes(e.bytes)
	if !exponent.IsInt64() {
		return false
	}
	public := &rsa.PublicKey{N: new(big.Int).SetBytes(n.bytes), E: int(exponent.Int64())}
	return rsa.VerifyPKCS1v15(public, crypto.SHA256, digest, signature) == nil
}

func decodeB64(value string) ([]byte, error) {
	if value == "" || len(value) > maxPasskeyResponseBytes {
		return nil, ErrInvalidPasskey
	}
	return base64.RawURLEncoding.DecodeString(value)
}

func parseAttestedCredentialData(authData []byte) ([]byte, []byte, error) {
	if len(authData) < 55 {
		return nil, nil, ErrInvalidPasskey
	}
	position := 37 + 16
	if position+2 > len(authData) {
		return nil, nil, ErrInvalidPasskey
	}
	length := int(binary.BigEndian.Uint16(authData[position : position+2]))
	position += 2
	if length <= 0 || position+length >= len(authData) {
		return nil, nil, ErrInvalidPasskey
	}
	id := append([]byte(nil), authData[position:position+length]...)
	position += length
	key := authData[position:]
	if _, err := decodeCBOR(key); err != nil {
		return nil, nil, err
	}
	return id, append([]byte(nil), key...), nil
}

// Minimal bounded CBOR decoder for WebAuthn authenticator data. It supports
// the definite/indefinite maps, arrays, byte strings and integer keys used by
// browser passkeys, while rejecting oversized or deeply nested input.
const (
	cborUnsigned byte = 0
	cborNegative byte = 1
	cborBytes    byte = 2
	cborText     byte = 3
	cborArray    byte = 4
	cborMap      byte = 5
	cborTag      byte = 6
	cborSimple   byte = 7
)

type cborValue struct {
	kind    byte
	uint    uint64
	bytes   []byte
	text    string
	items   []cborValue
	pairs   []cborPair
	boolean bool
}

type cborPair struct{ key, value cborValue }

type cborDecoder struct {
	data     []byte
	position int
	depth    int
}

func decodeCBOR(data []byte) (cborValue, error) {
	if len(data) == 0 || len(data) > maxPasskeyResponseBytes {
		return cborValue{}, ErrInvalidPasskey
	}
	d := &cborDecoder{data: data}
	value, err := d.value()
	if err != nil || d.position != len(data) {
		return cborValue{}, ErrInvalidPasskey
	}
	return value, nil
}

func (d *cborDecoder) value() (cborValue, error) {
	if d.depth > 32 || d.position >= len(d.data) {
		return cborValue{}, ErrInvalidPasskey
	}
	d.depth++
	defer func() { d.depth-- }()
	header := d.data[d.position]
	d.position++
	major, additional := header>>5, header&0x1f
	length, indefinite, err := d.length(additional)
	if err != nil {
		return cborValue{}, err
	}
	if indefinite && major != cborBytes && major != cborText && major != cborArray && major != cborMap {
		return cborValue{}, ErrInvalidPasskey
	}
	switch major {
	case cborUnsigned:
		return cborValue{kind: major, uint: length}, nil
	case cborNegative:
		return cborValue{kind: major, uint: length}, nil
	case cborBytes:
		if indefinite {
			return d.indefiniteBytes()
		}
		return d.takeBytes(length, major)
	case cborText:
		if indefinite {
			return d.indefiniteText()
		}
		value, err := d.takeBytes(length, major)
		if err != nil {
			return cborValue{}, err
		}
		return value, nil
	case cborArray:
		return d.collection(length, indefinite, false)
	case cborMap:
		return d.collection(length, indefinite, true)
	case cborTag:
		return d.value()
	case cborSimple:
		if additional == 20 || additional == 21 {
			return cborValue{kind: major, boolean: additional == 21}, nil
		}
		if additional == 22 || additional == 23 {
			return cborValue{kind: major}, nil
		}
	}
	return cborValue{}, ErrInvalidPasskey
}

func (d *cborDecoder) length(additional byte) (uint64, bool, error) {
	if additional == 31 {
		return 0, true, nil
	}
	var count int
	switch {
	case additional < 24:
		return uint64(additional), false, nil
	case additional == 24:
		count = 1
	case additional == 25:
		count = 2
	case additional == 26:
		count = 4
	case additional == 27:
		count = 8
	default:
		return 0, false, ErrInvalidPasskey
	}
	if d.position+count > len(d.data) {
		return 0, false, ErrInvalidPasskey
	}
	var value uint64
	for _, item := range d.data[d.position : d.position+count] {
		value = value<<8 | uint64(item)
	}
	d.position += count
	if value > maxPasskeyResponseBytes {
		return 0, false, ErrInvalidPasskey
	}
	return value, false, nil
}

func (d *cborDecoder) takeBytes(length uint64, kind byte) (cborValue, error) {
	if length > uint64(len(d.data)-d.position) {
		return cborValue{}, ErrInvalidPasskey
	}
	end := d.position + int(length)
	raw := append([]byte(nil), d.data[d.position:end]...)
	d.position = end
	if kind == cborText {
		return cborValue{kind: kind, text: string(raw)}, nil
	}
	return cborValue{kind: kind, bytes: raw}, nil
}

func (d *cborDecoder) collection(length uint64, indefinite, mapValue bool) (cborValue, error) {
	value := cborValue{kind: cborArray}
	if mapValue {
		value.kind = cborMap
	}
	for index := uint64(0); indefinite || index < length; index++ {
		if indefinite && d.position < len(d.data) && d.data[d.position] == 0xff {
			d.position++
			break
		}
		if index > 4096 {
			return cborValue{}, ErrInvalidPasskey
		}
		key, err := d.value()
		if err != nil {
			return cborValue{}, err
		}
		if mapValue {
			item, err := d.value()
			if err != nil {
				return cborValue{}, err
			}
			value.pairs = append(value.pairs, cborPair{key: key, value: item})
		} else {
			value.items = append(value.items, key)
		}
	}
	return value, nil
}

func (d *cborDecoder) indefiniteBytes() (cborValue, error) { return d.indefiniteString(cborBytes) }
func (d *cborDecoder) indefiniteText() (cborValue, error)  { return d.indefiniteString(cborText) }
func (d *cborDecoder) indefiniteString(kind byte) (cborValue, error) {
	var output []byte
	for len(output) <= maxPasskeyResponseBytes {
		if d.position >= len(d.data) {
			return cborValue{}, ErrInvalidPasskey
		}
		if d.data[d.position] == 0xff {
			d.position++
			if kind == cborText {
				return cborValue{kind: kind, text: string(output)}, nil
			}
			return cborValue{kind: kind, bytes: output}, nil
		}
		part, err := d.value()
		if err != nil || part.kind != kind {
			return cborValue{}, ErrInvalidPasskey
		}
		if kind == cborText {
			output = append(output, []byte(part.text)...)
		} else {
			output = append(output, part.bytes...)
		}
	}
	return cborValue{}, ErrInvalidPasskey
}

func (v cborValue) mapInt(key int64) (*cborValue, bool) {
	for _, pair := range v.pairs {
		if value, ok := pair.key.int64Value(); ok && value == key {
			copy := pair.value
			return &copy, true
		}
	}
	return nil, false
}
func (v cborValue) mapBytes(key string) ([]byte, bool) {
	for _, pair := range v.pairs {
		if pair.key.kind == cborText && pair.key.text == key && pair.value.kind == cborBytes {
			return pair.value.bytes, true
		}
	}
	return nil, false
}
func (v cborValue) int64Value() (int64, bool) {
	if v.kind == cborUnsigned && v.uint <= uint64(^uint64(0)>>1) {
		return int64(v.uint), true
	}
	if v.kind == cborNegative && v.uint <= uint64(^uint64(0)>>1) {
		return -1 - int64(v.uint), true
	}
	return 0, false
}
