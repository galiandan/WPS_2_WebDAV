package wps

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// UploadRequest mirrors client.upload's parameter surface.
type UploadRequest struct {
	ParentID string
	Name     string
	Source   io.Reader
	// Size mirrors the Python None-or-int keyword: nil means unknown.
	Size        *int64
	ContentType string
	CSRFToken   string
	Overwrite   bool
}

// SpoolLimiter coordinates upload spool reservations across the whole
// process. B501 lifted client.py's per-client reservation counters onto the
// shared Budget so the limits never scale with the number of mounted
// spaces; *budget.Budget satisfies this interface.
type SpoolLimiter interface {
	ReserveSpool(total int64, current int64) (int64, error)
	ReleaseSpool(reserved int64)
}

// uploadSpool is the spooled upload body with its streamed checksums. The
// reservation stays held for the object-storage half of the flow; close
// releases the reservation and removes any spilled temporary file, so
// deferring it on every return path mirrors Python's with-block.
type uploadSpool struct {
	limiter  SpoolLimiter
	reserved int64
	file     *spooledFile
	total    int64
	md5      string
	sha1     string
	sha256   string
}

func (s *uploadSpool) close() {
	if s.limiter != nil {
		s.limiter.ReleaseSpool(s.reserved)
	}
	s.file.close()
}

// Upload mirrors client.upload up to the pre_check request. The spool is
// closed and removed when this method returns or raises; the pre_check,
// object PUT, and registration halves land with the next upload tasks, so
// a completed spool answers the stage-boundary refusal.
func (c *Client) Upload(request UploadRequest) (model.RemoteEntry, error) {
	spool, err := c.spoolUpload(request)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	defer spool.close()
	return model.RemoteEntry{}, model.NewStorageError(model.KindUnsupportedOperation, "upload is not implemented in this stage")
}

// spoolUpload runs client.upload's validation prelude and spool loop: the
// name and configuration guards, the declared-size budget, the CSRF
// resolution, then streaming chunks into the spool while hashing
// MD5/SHA-1/SHA-256 and growing the coordinated spool reservation. The
// Python evaluation order is mirrored exactly so every rejection surfaces
// before the same request would.
func (c *Client) spoolUpload(request UploadRequest) (*uploadSpool, error) {
	name := request.Name
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return nil, model.NewStorageError(model.KindBadRequest, "name must be one remote file name")
	}
	if err := c.checkUploadConfig(); err != nil {
		return nil, err
	}
	if c.config.SpoolLimiter == nil {
		// Python always accounts for reservations inside the client; the Go
		// client refuses uploads rather than silently dropping the
		// process-wide coordination.
		return nil, model.NewStorageError(model.KindBadRequest, "upload spool limiter is required")
	}
	if request.Size != nil {
		if err := c.checkUploadBudget(*request.Size); err != nil {
			return nil, err
		}
	}
	csrf := request.CSRFToken
	if csrf == "" {
		credentials, err := c.currentCredentials()
		if err != nil {
			return nil, err
		}
		csrf = credentials.CSRFToken
	}
	if csrf == "" {
		return nil, model.NewStorageError(model.KindBadRequest, "csrf_token is required for write operation")
	}

	spoolDir := c.config.UploadSpoolDir
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}
	hasherMD5 := md5.New()
	hasherSHA1 := sha1.New()
	hasherSHA256 := sha256.New()
	total := int64(0)
	reserved := int64(0)

	file := newSpooledFile(c.config.UploadSpoolMemory, spoolDir)
	chunk := make([]byte, c.config.StreamChunkSize)
	fail := func(err error) (*uploadSpool, error) {
		c.config.SpoolLimiter.ReleaseSpool(reserved)
		file.close()
		return nil, err
	}
	for {
		read, readErr := request.Source.Read(chunk)
		if read > 0 {
			body := chunk[:read]
			if err := c.checkUploadBudget(total + int64(read)); err != nil {
				return fail(err)
			}
			newReservation, err := c.config.SpoolLimiter.ReserveSpool(total+int64(read), reserved)
			if err != nil {
				return fail(err)
			}
			reserved = newReservation
			if _, err := file.Write(body); err != nil {
				return fail(err)
			}
			hasherMD5.Write(body)
			hasherSHA1.Write(body)
			hasherSHA256.Write(body)
			total += int64(read)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fail(model.NewStorageError(model.KindIOFailure, "upload source read failed"))
		}
		if read == 0 {
			// Python's source.read(0) semantics: no bytes means the stream
			// ended, even without a distinct EOF marker.
			break
		}
	}
	if request.Size != nil && *request.Size != total {
		return fail(model.NewStorageError(model.KindBadRequest,
			fmt.Sprintf("source size mismatch: expected %d, read %d", *request.Size, total)))
	}
	return &uploadSpool{
		limiter:  c.config.SpoolLimiter,
		reserved: reserved,
		file:     file,
		total:    total,
		md5:      hex.EncodeToString(hasherMD5.Sum(nil)),
		sha1:     hex.EncodeToString(hasherSHA1.Sum(nil)),
		sha256:   hex.EncodeToString(hasherSHA256.Sum(nil)),
	}, nil
}

// checkUploadConfig mirrors client.upload's eight configuration guards.
func (c *Client) checkUploadConfig() error {
	if c.config.StreamChunkSize <= 0 {
		return model.NewStorageError(model.KindBadRequest, "stream_chunk_size must be positive")
	}
	if c.config.UploadSpoolMemory < 0 {
		return model.NewStorageError(model.KindBadRequest, "upload_spool_memory must not be negative")
	}
	if c.config.UploadMinFreeBytes < 0 {
		return model.NewStorageError(model.KindBadRequest, "upload_min_free_bytes must not be negative")
	}
	if c.config.MaxUploadBytes < 0 {
		return model.NewStorageError(model.KindBadRequest, "max_upload_bytes must not be negative")
	}
	if c.config.UploadRetries < 0 {
		return model.NewStorageError(model.KindBadRequest, "upload_retries must not be negative")
	}
	if c.config.UploadRetryDelay < 0 {
		return model.NewStorageError(model.KindBadRequest, "upload_retry_delay must not be negative")
	}
	if c.config.MultipartThreshold <= 0 {
		return model.NewStorageError(model.KindBadRequest, "multipart_threshold must be positive")
	}
	if c.config.MultipartPartSize <= 0 {
		return model.NewStorageError(model.KindBadRequest, "multipart_part_size must be positive")
	}
	return nil
}

// checkUploadBudget mirrors client._check_upload_budget: reject an upload
// before its temporary spool can exhaust the host. Uploads at or below the
// in-memory spool threshold never touch the disk accounting.
func (c *Client) checkUploadBudget(total int64) error {
	if total < 0 {
		return model.NewStorageError(model.KindBadRequest, "upload size must not be negative")
	}
	if c.config.MaxUploadBytes > 0 && total > c.config.MaxUploadBytes {
		return model.NewStorageError(model.KindInsufficientStorage, "upload exceeds the configured size limit")
	}
	if total <= c.config.UploadSpoolMemory {
		return nil
	}
	spoolDir := c.config.UploadSpoolDir
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}
	freeBytes, err := c.diskFree(spoolDir)
	if err != nil {
		return model.NewStorageError(model.KindInsufficientStorage, "upload spool directory is unavailable")
	}
	if freeBytes < total+c.config.UploadMinFreeBytes {
		return model.NewStorageError(model.KindInsufficientStorage, "not enough free space for the upload spool")
	}
	return nil
}
