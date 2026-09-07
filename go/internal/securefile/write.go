package securefile

import (
	"os"
	"path/filepath"
)

// Codes for the write stages, so callers can map each stage to their own
// fixed message (the Python reference distinguishes protect/create/replace
// failures in wording).
const (
	CodeChmodDir   Code = "chmod_dir"
	CodeTempCreate Code = "temp_create"
	CodeTempWrite  Code = "temp_write"
	CodeReplace    Code = "replace"
)

// WriteAtomic applies the state-file write discipline (workspace / web
// settings): the parent is validated without requiring existence, the
// content plus a trailing newline lands in a 0600 temp file in the target's
// directory, is fsynced, and is renamed over the target atomically. On any
// failure the previous target stays readable and temp files are cleaned up.
// Returns the new file's mtime in nanoseconds.
func WriteAtomic(path string, content string) (int64, error) {
	return writeAtomicInto(path, content, false, writeAll)
}

// WriteCredentialAtomic applies the credential-file write discipline: the
// parent must already exist and is tightened to 0700 before writing.
func WriteCredentialAtomic(path string, value string) error {
	_, err := writeAtomicInto(path, value, true, writeAll)
	return err
}

// WriteCredentialPair writes the cookie/CSRF pair as one unit. Both current
// values are snapshotted first; if either write fails, both files are
// rewritten with the snapshot values (rollback errors are ignored) so the
// pair never stays half-new, and the original error is returned.
func WriteCredentialPair(cookiePath string, cookieValue string, csrfPath string, csrfValue string) error {
	previousCookie, err := ReadSecret(cookiePath)
	if err != nil {
		return err
	}
	previousCSRF, err := ReadSecret(csrfPath)
	if err != nil {
		return err
	}
	err = WriteCredentialAtomic(cookiePath, cookieValue)
	if err == nil {
		err = WriteCredentialAtomic(csrfPath, csrfValue)
	}
	if err == nil {
		return nil
	}
	_ = WriteCredentialAtomic(cookiePath, previousCookie)
	_ = WriteCredentialAtomic(csrfPath, previousCSRF)
	return err
}

// writeAtomicInto runs the full write discipline; write is a seam so tests
// can fail the content-write stage deterministically.
func writeAtomicInto(path string, content string, tightenParent bool, write func(*os.File, string) error) (int64, error) {
	if !supported() {
		return 0, errCode(CodeUnsupportedPlatform)
	}
	if err := checkPathShape(path, !tightenParent); err != nil {
		return 0, err
	}
	if err := validateParent(path, tightenParent); err != nil {
		return 0, err
	}
	parent := rawDirName(path)
	if tightenParent {
		if err := os.Chmod(parent, 0o700); err != nil {
			return 0, errCode(CodeChmodDir)
		}
	}
	temp, err := os.CreateTemp(parent, "."+filepath.Base(path)+".")
	if err != nil {
		return 0, errCode(CodeTempCreate)
	}
	defer os.Remove(temp.Name())
	if err := write(temp, content+"\n"); err != nil {
		temp.Close()
		return 0, errCode(CodeTempWrite)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return 0, errCode(CodeTempWrite)
	}
	if err := temp.Close(); err != nil {
		return 0, errCode(CodeTempWrite)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return 0, errCode(CodeReplace)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, errCode(CodeStatFailed)
	}
	return info.ModTime().UnixNano(), nil
}

// writeAll writes the whole content, failing when any part is not written.
func writeAll(file *os.File, content string) error {
	_, err := file.WriteString(content)
	return err
}

// leftoverTemps lists surviving temp files for one target; tests use it to
// assert cleanup.
func leftoverTemps(dir string, targetName string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "."+targetName+".*"))
	return matches
}
