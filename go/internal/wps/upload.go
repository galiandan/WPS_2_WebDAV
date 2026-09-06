package wps

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// UploadOptions mirrors client.py's frozen UploadOptions dataclass: the
// captured-shape defaults for the normal upload fallback. The zero value is
// not the Python default — WithRapid starts true — so options are always
// built through defaultUploadOptions.
type UploadOptions struct {
	ParentPath          []string
	ReqByInternal       bool
	ClientStores        string
	Startswithfilename  string
	SuccessActionStatus int64
	FileID              int64
	WithRapid           bool
	TriedStore          []string
	IsUpNewVer          bool
}

// defaultUploadOptions mirrors UploadOptions()'s field defaults.
func defaultUploadOptions() *UploadOptions {
	return &UploadOptions{SuccessActionStatus: 200, WithRapid: true}
}

// orDefault mirrors `options = options or UploadOptions()`.
func (o *UploadOptions) orDefault() *UploadOptions {
	if o == nil {
		return defaultUploadOptions()
	}
	return o
}

// rebuiltForOverwrite mirrors the option rebuild client.upload performs when
// overwrite is set: the observed new-version upload shape with the file name
// as the startswith probe, status 201, and the ks3 store list.
func (o *UploadOptions) rebuiltForOverwrite(name string) *UploadOptions {
	clientStores := o.ClientStores
	if clientStores == "" {
		clientStores = "ks3,ks3sh"
	}
	startswith := o.Startswithfilename
	if startswith == "" {
		startswith = name
	}
	triedStore := o.TriedStore
	if len(triedStore) == 0 {
		triedStore = []string{"ks3,ks3sh"}
	}
	return &UploadOptions{
		ParentPath:          append([]string(nil), o.ParentPath...),
		ReqByInternal:       o.ReqByInternal,
		ClientStores:        clientStores,
		Startswithfilename:  startswith,
		SuccessActionStatus: 201,
		FileID:              o.FileID,
		WithRapid:           o.WithRapid,
		TriedStore:          append([]string(nil), triedStore...),
		IsUpNewVer:          o.IsUpNewVer,
	}
}

// pyStringList mirrors list(<tuple-of-str>): a JSON array, never null.
func pyStringList(values []string) []any {
	list := make([]any, len(values))
	for index, value := range values {
		list[index] = value
	}
	return list
}

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
	// Options mirrors the Python options keyword: nil means the defaults.
	Options *UploadOptions
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
	limiter     SpoolLimiter
	reserved    int64
	file        *spooledFile
	total       int64
	md5         string
	sha1        string
	sha256      string
	csrf        string
	contentType string
}

func (s *uploadSpool) close() {
	if s.limiter != nil {
		s.limiter.ReleaseSpool(s.reserved)
	}
	s.file.close()
}

// Upload mirrors client.upload through the signed object PUT. The spool is
// closed and removed when this method returns or raises; the file
// registration half lands with the next upload task, so a completed object
// PUT still answers the stage-boundary refusal.
func (c *Client) Upload(request UploadRequest) (model.RemoteEntry, error) {
	options := request.Options.orDefault()
	if request.Overwrite {
		options = options.rebuiltForOverwrite(request.Name)
	}
	spool, err := c.spoolUpload(request)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	defer spool.close()

	groupID, err := c.GroupID()
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if err := c.preCheckUpload(groupID, request.ParentID, request.Name, request.Overwrite); err != nil {
		return model.RemoteEntry{}, err
	}
	if spool.total >= c.config.MultipartThreshold {
		// The refusal point mirrors client.upload exactly: after the spool
		// and the pre_check, before any block request. An earlier rejection
		// would change the observable request order and needs a contract
		// decision first.
		if request.Overwrite {
			return model.RemoteEntry{}, model.NewStorageError(model.KindUnsupportedOperation, "multipart overwrite is disabled until independently verified")
		}
		identity := pyJSONIDString(groupID) + ":" + pyJSONIDString(request.ParentID) + ":" +
			request.Name + ":" + strconv.FormatInt(spool.total, 10) + ":" + spool.sha1
		return c.multipartUpload(spool, groupID, request.ParentID, request.Name, options, identity)
	}
	createResult, etag, err := c.objectUpload(groupID, request.ParentID, request.Name, spool, options, request.Overwrite)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	entry, err := c.registerUpload(groupID, request.ParentID, request.Name, spool, options, createResult, etag)
	if err != nil {
		// The object storage half succeeded, so the object may outlive this
		// failure unregistered. The error string is sanitized by
		// construction — operation and status only — and no delete API is
		// attempted, because none is confirmed.
		c.warnUpload("uploaded object may be left unregistered in WPS, manual cleanup may be needed: " + err.Error())
		return model.RemoteEntry{}, err
	}
	return entry, nil
}

// preCheckUpload mirrors client.upload's pre_check call: the exact ordered
// query, a continuation that only fires when overwrite observes HTTP 403
// (never a delete-first substitute), and the result gate that accepts an
// absent, null, or "ok" result.
func (c *Client) preCheckUpload(groupID string, parentID string, name string, overwrite bool) error {
	payload, err := c.RequestJSON(JSONRequest{
		Path: "/3rd/drive/api/v5/files/upload/pre_check",
		Query: []QueryPair{
			{Key: "file_name", Value: name},
			{Key: "group_id", Value: pyJSONIDString(groupID)},
			{Key: "parent_id", Value: pyJSONIDString(parentID)},
		},
		RetryOn401: true,
	})
	if err != nil {
		apiErr, ok := model.AsWpsAPIError(err)
		if !ok || apiErr.Status != 403 || !overwrite {
			return err
		}
		payload = map[string]any{"result": "ok"}
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return model.NewWpsAPIError("upload pre-check", 0, model.WpsCategoryUpstream)
	}
	return nil
}

// objectUpload mirrors client.upload's create_update instruction and the
// signed object PUT with its retry loop: the exact captured body, the
// instruction validation, one fresh signed URL per attempt after an
// exponential backoff, and the raw ETag off the final response. The bounded
// response read reuses MaxObjectResponseBytes.
func (c *Client) objectUpload(groupID string, parentID string, name string, spool *uploadSpool, options *UploadOptions, overwrite bool) (map[string]any, string, error) {
	createBody := &pyObject{
		keys: []string{"groupid", "parentid", "parent_path", "size", "name", "req_by_internal",
			"client_stores", "contenttype", "startswithfilename", "successactionstatus",
			"group_id", "parent_id", "file_id", "with_rapid", "tried_store", "sha256",
			"csrfmiddlewaretoken"},
		values: map[string]any{
			"groupid":             pyJSONID(groupID),
			"parentid":            pyJSONID(parentID),
			"parent_path":         pyStringList(options.ParentPath),
			"size":                pyInt(spool.total),
			"name":                name,
			"req_by_internal":     options.ReqByInternal,
			"client_stores":       options.ClientStores,
			"contenttype":         spool.contentType,
			"startswithfilename":  options.Startswithfilename,
			"successactionstatus": pyInt(options.SuccessActionStatus),
			"group_id":            pyJSONID(groupID),
			"parent_id":           pyJSONID(parentID),
			"file_id":             pyInt(options.FileID),
			"with_rapid":          options.WithRapid,
			"tried_store":         pyStringList(options.TriedStore),
			"sha256":              spool.sha256,
			"csrfmiddlewaretoken": spool.csrf,
		},
	}
	if overwrite {
		// Python appends md5 last, only when overwriting.
		createBody.keys = append(createBody.keys, "md5")
		createBody.values["md5"] = spool.md5
	}
	encoded, err := dumpPYValue(createBody)
	if err != nil {
		return nil, "", err
	}

	// createUploadInstruction mirrors the nested closure: a fresh PUT each
	// call, a string url, and the expect_code gate. Instruction failures
	// propagate immediately — only object PUT failures are retried.
	createUploadInstruction := func() (map[string]any, string, error) {
		result, err := c.RequestJSON(JSONRequest{
			Path:       "/3rd/drive/api/v5/files/upload/create_update",
			Method:     http.MethodPut,
			Body:       encoded,
			RetryOn401: true,
		})
		if err != nil {
			return nil, "", err
		}
		signedURL, isString := result["url"].(string)
		if !isString {
			return nil, "", model.NewWpsAPIError("create upload URL", 0, model.WpsCategoryUpstream)
		}
		expectedCode := 200
		if responseMeta, isMap := result["response"].(map[string]any); isMap {
			if codes, isList := responseMeta["expect_code"].([]any); isList && len(codes) > 0 {
				expectedCode = 0
				if number, isNumber := codes[0].(json.Number); isNumber {
					if parsed, convErr := number.Int64(); convErr == nil {
						expectedCode = int(parsed)
					} else if parsed, convErr := number.Float64(); convErr == nil && parsed == 200 {
						expectedCode = 200
					}
				}
			}
		}
		if expectedCode != 200 {
			return nil, "", model.NewWpsAPIError("unsupported object upload status", 0, model.WpsCategoryUpstream)
		}
		return result, signedURL, nil
	}

	createResult, signedURL, err := createUploadInstruction()
	if err != nil {
		return nil, "", err
	}
	for attempt := int64(0); ; attempt++ {
		reader, err := spool.file.reopen()
		if err != nil {
			return nil, "", err
		}
		etag, err := c.putSignedObject(signedURL, reader, spool.total)
		if err == nil {
			if etag == nil {
				return createResult, "", model.NewWpsAPIError("object upload response missing ETag", 0, model.WpsCategoryUpstream)
			}
			return createResult, *etag, nil
		}
		retryable := false
		var apiErr *model.WpsAPIError
		var storageErr *model.StorageError
		if errors.As(err, &apiErr) || errors.As(err, &storageErr) {
			retryable = true
		}
		if !retryable || attempt >= int64(c.config.UploadRetries) {
			return nil, "", err
		}
		c.uploadRetrySleep(attempt + 1)
		if createResult, signedURL, err = createUploadInstruction(); err != nil {
			return nil, "", err
		}
	}
}

// uploadRetrySleep mirrors _retry_delay: delay * 2**(attempt-1) seconds.
func (c *Client) uploadRetrySleep(attempt int64) {
	if attempt > 0 && c.config.UploadRetryDelay > 0 {
		time.Sleep(seconds(c.config.UploadRetryDelay * math.Pow(2, float64(attempt-1))))
	}
}

// registerUpload mirrors the file registration half of client.upload: the
// exact captured body with the instruction's store and the object ETag, the
// result gate, and the strict entry parse. Only a result success with a
// parseable entry counts as success.
func (c *Client) registerUpload(groupID string, parentID string, name string, spool *uploadSpool, options *UploadOptions, createResult map[string]any, etag string) (model.RemoteEntry, error) {
	// Python's create_result.get("store", ""): an absent key falls back to
	// the empty string, a present null stays null.
	store, present := createResult["store"]
	if !present {
		store = ""
	}
	body := &pyObject{
		keys: []string{"key", "groupid", "parentid", "name", "parent_path", "sha1",
			"size", "store", "etag", "isUpNewVer", "apiErrorInfo", "csrfmiddlewaretoken"},
		values: map[string]any{
			"key":                 spool.sha1,
			"groupid":             pyJSONID(groupID),
			"parentid":            pyJSONID(parentID),
			"name":                name,
			"parent_path":         pyStringList(options.ParentPath),
			"sha1":                spool.sha1,
			"size":                pyInt(spool.total),
			"store":               store,
			"etag":                etag,
			"isUpNewVer":          options.IsUpNewVer,
			"apiErrorInfo":        nil,
			"csrfmiddlewaretoken": spool.csrf,
		},
	}
	encoded, err := dumpPYValue(body)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/files/file",
		Method:     http.MethodPost,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return model.RemoteEntry{}, model.NewWpsAPIError("register uploaded file", 0, model.WpsCategoryUpstream)
	}
	return entryFromItem(payload)
}

// putSignedObject mirrors _put_signed_object: one streaming PUT of the
// rewound spool over the credential-free signed transport with the exact
// header set, a bounded response read before the status gate, and the raw
// ETag header.
func (c *Client) putSignedObject(signedURL string, source io.Reader, size int64) (*string, error) {
	var body io.Reader
	var contentLength int64
	if size > 0 {
		body = source
		contentLength = size
	}
	response, err := c.signed.Do("object upload", http.MethodPut, signedURL, []SignedHeader{
		{Name: "Content-Type", Value: "application/octet-stream"},
	}, body, contentLength)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if _, err := readLimitedResponse(response.Body, response.ContentLength, MaxObjectResponseBytes, "object upload", model.WpsCategoryUpstream); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, model.NewWpsAPIError("object upload", response.StatusCode, model.WpsCategoryUpstream)
	}
	return headerString(response.Header, "Etag"), nil
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
	contentType := request.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
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
		limiter:     c.config.SpoolLimiter,
		reserved:    reserved,
		file:        file,
		total:       total,
		md5:         hex.EncodeToString(hasherMD5.Sum(nil)),
		sha1:        hex.EncodeToString(hasherSHA1.Sum(nil)),
		sha256:      hex.EncodeToString(hasherSHA256.Sum(nil)),
		csrf:        csrf,
		contentType: contentType,
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
