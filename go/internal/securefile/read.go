// Package securefile implements the shared read discipline for secret and
// state files: absolute paths without symlinked parents, private regular
// files owned by root or the service user, and bounded reads that are
// validated both before and after opening. Errors carry only a category
// code — paths and file contents never appear in error text or logs.
package securefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxCredentialFileBytes mirrors client.py MAX_CREDENTIAL_FILE_BYTES.
const MaxCredentialFileBytes = 4 * 1024 * 1024

// Code classifies a secure-file rejection. Callers translate codes into
// their own fixed messages (the Python reference uses different wording per
// caller); the codes themselves are the stable surface.
type Code string

const (
	CodeInvalidPath         Code = "invalid_path"
	CodeParentSymlink       Code = "parent_symlink"
	CodeParentUnavailable   Code = "parent_unavailable"
	CodeParentUnsafe        Code = "parent_unsafe"
	CodeStatFailed          Code = "stat_failed"
	CodeNotRegular          Code = "not_regular"
	CodeFileUnsafe          Code = "file_unsafe"
	CodeOpenFailed          Code = "open_failed"
	CodeOpenMissing         Code = "open_missing"
	CodePostOpenUnsafe      Code = "post_open_unsafe"
	CodeReadFailed          Code = "read_failed"
	CodeTooLarge            Code = "too_large"
	CodeNotUTF8             Code = "not_utf8"
	CodeNotJSON             Code = "not_json"
	CodeNotObject           Code = "not_object"
	CodeControlChar         Code = "control_char"
	CodeUnsupportedPlatform Code = "unsupported_platform"
)

// Error is a fixed-message secure-file failure. Error() intentionally never
// includes the path or any file content.
type Error struct{ Code Code }

func (e *Error) Error() string { return "securefile: " + string(e.Code) }

func errCode(code Code) error { return &Error{Code: code} }

// CodeOf returns the category of err, or "" when err is not a securefile
// error.
func CodeOf(err error) Code {
	var secureErr *Error
	if errors.As(err, &secureErr) {
		return secureErr.Code
	}
	return ""
}

// ReadSecret applies the credential-file discipline and returns the
// stripped UTF-8 value. Unlike ReadJSONState, the parent directory must
// already exist and a missing file is an error.
func ReadSecret(path string) (string, error) {
	if !supported() {
		return "", errCode(CodeUnsupportedPlatform)
	}
	if err := checkPathShape(path, false); err != nil {
		return "", err
	}
	if err := validateParent(path, true); err != nil {
		return "", err
	}
	file, err := openSecure(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := checkAfterOpen(file, 0); err != nil {
		return "", err
	}
	raw, err := readBounded(file, MaxCredentialFileBytes)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// ReadJSONState applies the state-file discipline shared by the workspace
// and web settings files and returns the decoded JSON object plus the file
// mtime in nanoseconds. A missing file or a missing parent directory is not
// an error: fresh installs stay startable while login is pending. A
// whitespace-only file yields a nil payload with its mtime.
func ReadJSONState(path string, maxBytes int64) (map[string]any, *int64, error) {
	if !supported() {
		return nil, nil, errCode(CodeUnsupportedPlatform)
	}
	if err := checkPathShape(path, true); err != nil {
		return nil, nil, err
	}
	if err := validateParent(path, false); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, errCode(CodeStatFailed)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, errCode(CodeNotRegular)
	}
	if info.Mode().Perm()&0o077 != 0 || !ownedByService(info) {
		return nil, nil, errCode(CodeFileUnsafe)
	}
	file, err := openSecure(path)
	if err != nil {
		if CodeOf(err) == CodeOpenMissing {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer file.Close()
	stat, err := checkAfterOpen(file, maxBytes)
	if err != nil {
		return nil, nil, err
	}
	raw, err := readBounded(file, maxBytes)
	if err != nil {
		return nil, nil, err
	}
	mtimeNs := stat.ModTime().UnixNano()
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &mtimeNs, nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, nil, errCode(CodeNotJSON)
	}
	payload, ok := decoded.(map[string]any)
	if !ok {
		return nil, nil, errCode(CodeNotObject)
	}
	return payload, &mtimeNs, nil
}

// CheckCredentialValues mirrors the value-level checks client.py applies
// before credentials become outbound HTTP headers.
func CheckCredentialValues(cookie string, csrfToken string) error {
	if len(cookie) > MaxCredentialFileBytes || len(csrfToken) > MaxCredentialFileBytes {
		return errCode(CodeTooLarge)
	}
	for _, r := range cookie + csrfToken {
		if r < 0x20 || r == 0x7F {
			return errCode(CodeControlChar)
		}
	}
	return nil
}

// rawDirName mirrors os.path.dirname: a pure string split that keeps ".."
// and "." components unnormalized, so the parent walk sees exactly what the
// Python realpath/abspath comparison would see.
func rawDirName(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return ""
	}
	if index == 0 {
		return "/"
	}
	return path[:index]
}

// checkPathShape rejects paths the discipline can never accept. Credential
// paths only reject NUL (client.py) while state paths also reject CR/LF
// (workspace.py/settings.py).
func checkPathShape(path string, rejectCRLF bool) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') ||
		(rejectCRLF && strings.ContainsAny(path, "\r\n")) {
		return errCode(CodeInvalidPath)
	}
	return nil
}

// readBounded reads at most maxBytes+1 bytes and rejects invalid UTF-8
// before the size check, matching the Python decode-then-measure order.
func readBounded(file *os.File, maxBytes int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, errCode(CodeReadFailed)
	}
	if !utf8.Valid(raw) {
		return nil, errCode(CodeNotUTF8)
	}
	if int64(len(raw)) > maxBytes {
		return nil, errCode(CodeTooLarge)
	}
	return raw, nil
}

// checkAfterOpen re-validates via fstat after opening, shrinking the
// check-vs-use race window the pre-open lstat cannot close. maxBytes <= 0
// skips the size check (credential files have no fstat size limit in the
// Python reference and rely on the read bound instead).
func checkAfterOpen(file *os.File, maxBytes int64) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, errCode(CodeStatFailed)
	}
	if !info.Mode().IsRegular() {
		return nil, errCode(CodeNotRegular)
	}
	if info.Mode().Perm()&0o077 != 0 || !ownedByService(info) {
		return nil, errCode(CodePostOpenUnsafe)
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return nil, errCode(CodePostOpenUnsafe)
	}
	return info, nil
}
