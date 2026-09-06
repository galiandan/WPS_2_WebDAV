package wps

import (
	"bytes"
	"io"
	"os"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// spooledFile mirrors Python's SpooledTemporaryFile(max_size, mode="w+b",
// dir=...): bytes stay in a memory buffer until the buffered size would
// exceed maxMemory, then roll over to one temporary file in dir. The file
// is created with 0600 and removed again by close, so every exit path of
// the upload leaves no spool artefact behind.
type spooledFile struct {
	maxMemory int64
	dir       string

	buffer *bytes.Buffer // nil once rolled over to disk
	file   *os.File      // nil until the rollover happens
	size   int64
}

func newSpooledFile(maxMemory int64, dir string) *spooledFile {
	if dir == "" {
		dir = os.TempDir()
	}
	return &spooledFile{maxMemory: maxMemory, dir: dir, buffer: &bytes.Buffer{}}
}

// Write appends one chunk, rolling over to disk when the memory threshold
// would be exceeded — exactly Python's tell() > max_size check after each
// write, so a spool ending at exactly max_size never touches the disk.
func (s *spooledFile) Write(chunk []byte) (int, error) {
	if s.buffer != nil {
		s.buffer.Write(chunk)
		s.size += int64(len(chunk))
		if s.size > s.maxMemory {
			if err := s.rollover(); err != nil {
				return 0, err
			}
		}
		return len(chunk), nil
	}
	written, err := s.file.Write(chunk)
	s.size += int64(written)
	return written, err
}

func (s *spooledFile) rollover() error {
	file, err := os.CreateTemp(s.dir, "wps-upload-*")
	if err != nil {
		return model.NewStorageError(model.KindIOFailure, "upload spool rollover failed")
	}
	if _, err := file.Write(s.buffer.Bytes()); err != nil {
		file.Close()
		os.Remove(file.Name())
		return model.NewStorageError(model.KindIOFailure, "upload spool rollover failed")
	}
	s.file = file
	s.buffer = nil
	return nil
}

// reopen positions the spool at its start for the object-storage PUT and
// returns a reader over the buffered bytes.
func (s *spooledFile) reopen() (io.Reader, error) {
	if s.file != nil {
		if _, err := s.file.Seek(0, 0); err != nil {
			return nil, model.NewStorageError(model.KindIOFailure, "upload spool rewind failed")
		}
		return s.file, nil
	}
	return bytes.NewReader(s.buffer.Bytes()), nil
}

// close closes any spilled file and removes it. It is safe to defer on
// every return path and never reports a missing file as an error.
func (s *spooledFile) close() {
	if s.file != nil {
		os.Remove(s.file.Name())
		s.file.Close()
		s.file = nil
	}
	s.buffer = nil
}

// spillPath reports where the spool currently lives, for tests that verify
// the temporary file is really gone after close.
func (s *spooledFile) spillPath() string {
	if s.file == nil {
		return ""
	}
	return s.file.Name()
}
