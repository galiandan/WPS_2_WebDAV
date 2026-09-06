// The multipart upload path mirrors client._multipart_upload: the block
// session initialization, per-part signed PUTs driven by captured
// instructions, the signed merge POST, and the final file registration,
// with an on-disk resume checkpoint whose file name is only the sanitized
// identity hash. The checkpoint never holds body bytes or credentials.

package wps

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
)

// maxMultipartSessionResets caps how often one upload may rebuild its block
// session. Python re-arms the reset after every rebuilt part and a server
// that keeps answering 400/404/410 could loop forever; the plan requires a
// bounded rebuild count instead.
const maxMultipartSessionResets = 3

// multipartState is the in-memory resume state, mirroring the checkpoint
// dict client.py persists: identity, session fields, the resolved part
// size, and the confirmed etag per part number.
type multipartState struct {
	identity string
	uploadID string
	key      string
	store    string
	partSize int64
	parts    map[string]string
}

// multipartCheckpointFile is the checkpoint encoding. Field order matches
// Python's json.dumps(sort_keys=True) output for the same keys.
type multipartCheckpointFile struct {
	Identity string            `json:"identity"`
	Key      string            `json:"key"`
	PartSize int64             `json:"part_size"`
	Parts    map[string]string `json:"parts"`
	Store    string            `json:"store"`
	UploadID string            `json:"upload_id"`
	Version  int               `json:"version"`
}

// resumeLocks serializes multipart uploads that share one checkpoint file.
// Keyed by the checkpoint path, which is the identity hash, so concurrent
// uploads of the same file cannot interleave checkpoint saves (plan B1103;
// the Python reference has no equivalent guard).
var resumeLocks sync.Map

func lockResumeCheckpoint(path string) func() {
	value, _ := resumeLocks.LoadOrStore(path, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// resumePathFor mirrors the resume_path derivation: the directory must be
// absolute, and the file name is only the sha256 of the resume identity, so
// neither the file name nor any spool artefact can leak the upload name.
func resumePathFor(resumeDir string, identity string) (string, error) {
	if resumeDir == "" {
		return "", nil
	}
	if !filepath.IsAbs(resumeDir) {
		return "", model.NewStorageError(model.KindBadRequest, "upload_resume_dir must be absolute")
	}
	digest := sha256.Sum256([]byte(identity))
	return filepath.Join(resumeDir, hex.EncodeToString(digest[:])+".json"), nil
}

// loadResumeCheckpoint mirrors the checkpoint read. Every read, decode, or
// shape failure yields no state instead of an error — Python treats a
// broken or foreign checkpoint as absent and re-initializes. The raw
// payload is returned alongside the validated parts map because the
// session fields and part size are validated afterwards, exactly like
// client.py: a shape-valid checkpoint with a broken part size is a hard
// error, not a fresh start.
func loadResumeCheckpoint(path string, identity string) (map[string]any, map[string]string) {
	if path == "" {
		return nil, nil
	}
	payload, _, err := securefile.ReadJSONState(path, securefile.MaxCredentialFileBytes)
	if err != nil || payload == nil {
		return nil, nil
	}
	version, ok := pyToInt(payload["version"])
	if !ok || version != 1 {
		return nil, nil
	}
	if candidateIdentity, isString := payload["identity"].(string); !isString || candidateIdentity != identity {
		return nil, nil
	}
	rawParts, ok := payload["parts"].(map[string]any)
	if !ok {
		return nil, nil
	}
	parts := make(map[string]string, len(rawParts))
	for name, value := range rawParts {
		if !isASCIIDecimal(name) {
			return nil, nil
		}
		etag, isString := value.(string)
		if !isString {
			return nil, nil
		}
		parts[name] = etag
	}
	return payload, parts
}

// saveResumeCheckpoint mirrors save_state: an atomic 0600 replace through
// the securefile discipline. A missing path (no resume dir configured)
// keeps the state in memory only.
func saveResumeCheckpoint(path string, state *multipartState) error {
	if path == "" {
		return nil
	}
	encoded, err := json.Marshal(&multipartCheckpointFile{
		Identity: state.identity,
		Key:      state.key,
		PartSize: state.partSize,
		Parts:    state.parts,
		Store:    state.store,
		UploadID: state.uploadID,
		Version:  1,
	})
	if err != nil {
		return model.NewStorageError(model.KindIOFailure, "resume checkpoint write failed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return model.NewStorageError(model.KindIOFailure, "resume checkpoint directory is unavailable")
	}
	if _, err := securefile.WriteAtomic(path, string(encoded)); err != nil {
		return model.NewStorageError(model.KindIOFailure, "resume checkpoint write failed")
	}
	return nil
}

// removeResumeCheckpoint mirrors the post-registration unlink: only a
// missing checkpoint is tolerated, any other removal failure surfaces
// after the successful registration exactly like Python's unlink.
func removeResumeCheckpoint(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return model.NewStorageError(model.KindIOFailure, "resume checkpoint removal failed")
	}
	return nil
}

// pyToInt mirrors int(value) over the JSON shapes client.py feeds it: int
// and float forms (via json.Number), decimal strings with surrounding
// whitespace, and bools. Anything else fails like int(None) would.
func pyToInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed, true
		}
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return int64(parsed), true
	case float64:
		return int64(typed), true
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// firstHeaderValue mirrors `headers.get(A) or headers.get(a)`: the first
// present truthy value, whatever its JSON type.
func firstHeaderValue(headers map[string]any, names ...string) (any, bool) {
	for _, name := range names {
		if value, present := headers[name]; present && pyTruthy(value) {
			return value, true
		}
	}
	return nil, false
}

// expectFirstCode200 mirrors the part/merge gate
// `codes = meta.get("expect_code", [200]); isinstance(codes, list) and
// codes and codes[0] == 200`: an absent key defaults to 200, a present
// null, non-list, empty list, or first code other than 200 fails.
func expectFirstCode200(meta map[string]any) bool {
	raw, present := meta["expect_code"]
	if !present {
		return true
	}
	codes, isList := raw.([]any)
	if !isList || len(codes) == 0 {
		return false
	}
	number, isNumber := codes[0].(json.Number)
	if !isNumber {
		return false
	}
	if parsed, err := number.Int64(); err == nil {
		return parsed == 200
	}
	parsed, err := number.Float64()
	return err == nil && parsed == 200
}

// normalizeEtag mirrors _normalise_etag: surrounding whitespace, then all
// surrounding double quotes, are stripped.
func normalizeEtag(value string) string {
	return strings.Trim(strings.TrimSpace(value), "\"")
}

// multipartPartSize mirrors _multipart_part_size: the configured part size
// raised to the upstream minimum and to keep within max_parts, then
// checked against the upstream maximum and the 64 MiB memory ceiling.
func (c *Client) multipartPartSize(total int64, limit map[string]any) (int64, error) {
	minSize, ok := pyToInt(limit["min_part_size"])
	if !ok {
		return 0, model.NewWpsAPIError("parse multipart limits", 0, model.WpsCategoryUpstream)
	}
	maxSize, ok := pyToInt(limit["max_part_size"])
	if !ok {
		return 0, model.NewWpsAPIError("parse multipart limits", 0, model.WpsCategoryUpstream)
	}
	maxParts, ok := pyToInt(limit["max_parts"])
	if !ok {
		return 0, model.NewWpsAPIError("parse multipart limits", 0, model.WpsCategoryUpstream)
	}
	if minSize <= 0 || maxSize < minSize || maxParts <= 0 {
		return 0, model.NewWpsAPIError("invalid multipart limits", 0, model.WpsCategoryUpstream)
	}
	if c.config.MultipartPartSize <= 0 {
		return 0, model.NewStorageError(model.KindBadRequest, "multipart_part_size must be positive")
	}
	partSize := c.config.MultipartPartSize
	if partSize < minSize {
		partSize = minSize
	}
	if ceil := (total + maxParts - 1) / maxParts; partSize < ceil {
		partSize = ceil
	}
	if partSize > maxSize {
		return 0, model.NewWpsAPIError("file exceeds multipart size limits", 0, model.WpsCategoryUpstream)
	}
	if partSize > MaxMultipartPartBuffer {
		return 0, model.NewStorageError(model.KindInsufficientStorage, "multipart part exceeds the memory safety limit")
	}
	return partSize, nil
}

// multipartUpload mirrors client._multipart_upload: block session
// initialization (or checkpoint resume), the per-part upload loop with its
// retry and session-rebuild handling, the signed merge, and the final file
// registration. The checkpoint is removed only after registration
// succeeded and the entry parsed.
func (c *Client) multipartUpload(spool *uploadSpool, groupID string, parentID string, name string, options *UploadOptions, identity string) (model.RemoteEntry, error) {
	groupText := pyJSONIDString(groupID)
	parentText := pyJSONIDString(parentID)
	resumePath, err := resumePathFor(c.config.UploadResumeDir, identity)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if resumePath != "" {
		unlock := lockResumeCheckpoint(resumePath)
		defer unlock()
	}

	payload, parts := loadResumeCheckpoint(resumePath, identity)
	reusable := payload != nil
	if reusable {
		for _, field := range []string{"upload_id", "key", "store"} {
			value, isString := payload[field].(string)
			if !isString || value == "" {
				reusable = false
				break
			}
		}
	}
	state := &multipartState{identity: identity, parts: map[string]string{}}
	if reusable {
		state.uploadID = payload["upload_id"].(string)
		state.key = payload["key"].(string)
		state.store = payload["store"].(string)
		partSize, ok := pyToInt(payload["part_size"])
		if !ok {
			return model.RemoteEntry{}, model.NewWpsAPIError("invalid multipart resume checkpoint", 0, model.WpsCategoryUpstream)
		}
		state.partSize = partSize
		state.parts = parts
	} else {
		initBody := &pyObject{
			keys: []string{"with_rapid", "hash", "size", "group_id", "name", "parent_id", "tried_store", "csrfmiddlewaretoken"},
			values: map[string]any{
				"with_rapid":          options.WithRapid,
				"hash":                spool.sha1,
				"size":                pyInt(spool.total),
				"group_id":            groupText,
				"name":                name,
				"parent_id":           parentText,
				"tried_store":         pyStringList(options.TriedStore),
				"csrfmiddlewaretoken": spool.csrf,
			},
		}
		encoded, err := dumpPYValue(initBody)
		if err != nil {
			return model.RemoteEntry{}, err
		}
		initPayload, err := c.RequestJSON(JSONRequest{
			Path:       "/3rd/drive/api/v5/files/upload/block",
			Method:     http.MethodPost,
			Body:       encoded,
			RetryOn401: true,
		})
		if err != nil {
			return model.RemoteEntry{}, err
		}
		if result, present := initPayload["result"]; present && result != nil && result != "ok" {
			return model.RemoteEntry{}, model.NewWpsAPIError("initialize multipart upload", 0, model.WpsCategoryUpstream)
		}
		var ok bool
		state.uploadID, ok = initPayload["upload_id"].(string)
		if ok {
			state.key, ok = initPayload["key"].(string)
		}
		if ok {
			state.store, ok = initPayload["store"].(string)
		}
		limit, limitIsMap := initPayload["limit"].(map[string]any)
		if !ok || state.uploadID == "" || state.key == "" || state.store == "" || !limitIsMap {
			return model.RemoteEntry{}, model.NewWpsAPIError("multipart initialization response is incomplete", 0, model.WpsCategoryUpstream)
		}
		state.partSize, err = c.multipartPartSize(spool.total, limit)
		if err != nil {
			return model.RemoteEntry{}, err
		}
		if err := saveResumeCheckpoint(resumePath, state); err != nil {
			return model.RemoteEntry{}, err
		}
	}

	partInfos := make([]any, 0, 8)
	sessionResets := 0
	partNumber := int64(1)
	for {
		if known, ok := state.parts[strconv.FormatInt(partNumber, 10)]; ok && known != "" {
			partInfos = append(partInfos, multipartPartInfo(known, partNumber))
			partNumber++
			if (partNumber-1)*state.partSize >= spool.total {
				break
			}
			continue
		}
		data, err := spool.file.readPart((partNumber-1)*state.partSize, state.partSize)
		if err != nil {
			return model.RemoteEntry{}, err
		}
		if len(data) == 0 {
			break
		}
		md5Sum := md5.Sum(data)
		var etag string
		sessionReset := false
		for attempt := 0; ; attempt++ {
			etag, err = c.uploadMultipartPart(partNumber, data, md5Sum, state, options, spool.csrf)
			if err == nil {
				break
			}
			var apiErr *model.WpsAPIError
			if errors.As(err, &apiErr) &&
				(apiErr.Status == 400 || apiErr.Status == 404 || apiErr.Status == 410) &&
				resumePath != "" && !sessionReset {
				if sessionResets >= maxMultipartSessionResets {
					return model.RemoteEntry{}, err
				}
				sessionResets++
				if err := c.reinitializeMultipart(spool, groupText, parentText, name, options, state, resumePath); err != nil {
					return model.RemoteEntry{}, err
				}
				sessionReset = true
				break
			}
			if attempt >= c.config.UploadRetries {
				return model.RemoteEntry{}, err
			}
			c.uploadRetrySleep(int64(attempt + 1))
		}
		if sessionReset {
			partNumber = 1
			continue
		}
		partInfos = append(partInfos, multipartPartInfo(etag, partNumber))
		state.parts[strconv.FormatInt(partNumber, 10)] = etag
		if err := saveResumeCheckpoint(resumePath, state); err != nil {
			return model.RemoteEntry{}, err
		}
		partNumber++
	}

	return c.multipartRegister(spool, groupText, parentText, name, options, state, partInfos, resumePath)
}

// multipartPartInfo builds one merge part_infos entry: the etag first, the
// part number as a JSON integer.
func multipartPartInfo(etag string, partNumber int64) *pyObject {
	return &pyObject{
		keys:   []string{"etag", "part_number"},
		values: map[string]any{"etag": etag, "part_number": pyInt(partNumber)},
	}
}

// reinitializeMultipart mirrors the rebuild inside the part retry handler:
// a fresh block POST, strict field validation, a recomputed part size, and
// a wiped parts map so no part of the dead session survives into the new
// upload_id. Failures here propagate immediately — the surrounding retry
// loop never retries the rebuild itself.
func (c *Client) reinitializeMultipart(spool *uploadSpool, groupText string, parentText string, name string, options *UploadOptions, state *multipartState, resumePath string) error {
	initBody := &pyObject{
		keys: []string{"with_rapid", "hash", "size", "group_id", "name", "parent_id", "tried_store", "csrfmiddlewaretoken"},
		values: map[string]any{
			"with_rapid":          options.WithRapid,
			"hash":                spool.sha1,
			"size":                pyInt(spool.total),
			"group_id":            groupText,
			"name":                name,
			"parent_id":           parentText,
			"tried_store":         pyStringList(options.TriedStore),
			"csrfmiddlewaretoken": spool.csrf,
		},
	}
	encoded, err := dumpPYValue(initBody)
	if err != nil {
		return err
	}
	fresh, err := c.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/files/upload/block",
		Method:     http.MethodPost,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return err
	}
	if result, present := fresh["result"]; present && result != nil && result != "ok" {
		return model.NewWpsAPIError("reinitialize multipart upload", 0, model.WpsCategoryUpstream)
	}
	uploadID, _ := fresh["upload_id"].(string)
	key, _ := fresh["key"].(string)
	store, _ := fresh["store"].(string)
	limit, limitIsMap := fresh["limit"].(map[string]any)
	if uploadID == "" || key == "" || store == "" || !limitIsMap {
		return model.NewWpsAPIError("reinitialize multipart response is incomplete", 0, model.WpsCategoryUpstream)
	}
	partSize, err := c.multipartPartSize(spool.total, limit)
	if err != nil {
		return err
	}
	state.uploadID = uploadID
	state.key = key
	state.store = store
	state.partSize = partSize
	state.parts = map[string]string{}
	return saveResumeCheckpoint(resumePath, state)
}

// uploadMultipartPart mirrors one part attempt: the exact PUT body, the
// instruction validation in client.py's order, the Content-MD5 agreement
// check, and the credential-free signed PUT that returns the normalized
// ETag.
func (c *Client) uploadMultipartPart(partNumber int64, data []byte, md5Sum [md5.Size]byte, state *multipartState, options *UploadOptions, csrf string) (string, error) {
	body := &pyObject{
		keys: []string{"key", "md5", "part_number", "part_size", "req_by_internal", "store", "upload_id", "csrfmiddlewaretoken"},
		values: map[string]any{
			"key":                 state.key,
			"md5":                 hex.EncodeToString(md5Sum[:]),
			"part_number":         pyInt(partNumber),
			"part_size":           pyInt(int64(len(data))),
			"req_by_internal":     options.ReqByInternal,
			"store":               state.store,
			"upload_id":           state.uploadID,
			"csrfmiddlewaretoken": csrf,
		},
	}
	encoded, err := dumpPYValue(body)
	if err != nil {
		return "", err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/files/upload/block",
		Method:     http.MethodPut,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return "", err
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return "", model.NewWpsAPIError("get multipart part URL", 0, model.WpsCategoryUpstream)
	}
	partURL, urlIsString := payload["url"].(string)
	method, _ := payload["method"].(string)
	requestInfo, _ := payload["request"].(map[string]any)
	responseInfo, _ := payload["response"].(map[string]any)
	if method != "PUT" || !urlIsString {
		return "", model.NewWpsAPIError("invalid multipart part instruction", 0, model.WpsCategoryUpstream)
	}
	if requestInfo == nil || requestInfo["body_type"] != "file" {
		return "", model.NewWpsAPIError("invalid multipart part request instruction", 0, model.WpsCategoryUpstream)
	}
	if responseInfo == nil {
		return "", model.NewWpsAPIError("invalid multipart part response instruction", 0, model.WpsCategoryUpstream)
	}
	if !expectFirstCode200(responseInfo) {
		return "", model.NewWpsAPIError("unsupported multipart part status", 0, model.WpsCategoryUpstream)
	}
	partHeaders, _ := requestInfo["headers"].(map[string]any)
	if partHeaders == nil {
		return "", model.NewWpsAPIError("multipart part headers missing", 0, model.WpsCategoryUpstream)
	}
	contentMD5Value, _ := firstHeaderValue(partHeaders, "Content-MD5", "content-md5")
	contentTypeValue, _ := firstHeaderValue(partHeaders, "Content-Type", "content-type")
	expectedMD5 := base64.StdEncoding.EncodeToString(md5Sum[:])
	contentMD5, _ := contentMD5Value.(string)
	contentType, _ := contentTypeValue.(string)
	if contentMD5 != expectedMD5 || contentType != "application/octet-stream" {
		return "", model.NewWpsAPIError("multipart part headers do not match content", 0, model.WpsCategoryUpstream)
	}
	return c.putSignedPart(partURL, data, contentMD5)
}

// putSignedPart mirrors _put_signed_part: Content-MD5 plus the fixed
// Content-Type over the credential-free signed transport, a bounded
// response read before the status gate, and the normalized ETag header.
func (c *Client) putSignedPart(signedURL string, data []byte, contentMD5 string) (string, error) {
	response, err := c.signed.Do("multipart part upload", http.MethodPut, signedURL, []SignedHeader{
		{Name: "Content-MD5", Value: contentMD5},
		{Name: "Content-Type", Value: "application/octet-stream"},
	}, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if _, err := readLimitedResponse(response.Body, response.ContentLength, MaxObjectResponseBytes, "multipart part upload", model.WpsCategoryUpstream); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", model.NewWpsAPIError("multipart part upload", response.StatusCode, model.WpsCategoryUpstream)
	}
	etag := headerString(response.Header, "Etag")
	if etag == nil || *etag == "" {
		return "", model.NewWpsAPIError("multipart part response missing ETag", 0, model.WpsCategoryUpstream)
	}
	return normalizeEtag(*etag), nil
}

// multipartRegister mirrors the merge half of _multipart_upload plus the
// final registration: the exact merge body with the collected part_infos,
// the instruction validation in client.py's order, the signed POST, the
// DTD-refusing XML ETag parse, and the registration body whose group and
// parent are strings here (unlike the normal upload's JSON numbers).
func (c *Client) multipartRegister(spool *uploadSpool, groupText string, parentText string, name string, options *UploadOptions, state *multipartState, partInfos []any, resumePath string) (model.RemoteEntry, error) {
	mergeBody := &pyObject{
		keys: []string{"key", "req_by_internal", "store", "part_infos", "upload_id", "csrfmiddlewaretoken"},
		values: map[string]any{
			"key":                 state.key,
			"req_by_internal":     options.ReqByInternal,
			"store":               state.store,
			"part_infos":          partInfos,
			"upload_id":           state.uploadID,
			"csrfmiddlewaretoken": spool.csrf,
		},
	}
	encoded, err := dumpPYValue(mergeBody)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	payload, err := c.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/files/upload/block/merge",
		Method:     http.MethodPost,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if result, present := payload["result"]; present && result != nil && result != "ok" {
		return model.RemoteEntry{}, model.NewWpsAPIError("prepare multipart merge", 0, model.WpsCategoryUpstream)
	}
	mergeURL, urlIsString := payload["url"].(string)
	mergeMethod, _ := payload["method"].(string)
	mergeRequest, _ := payload["request"].(map[string]any)
	mergeResponse, _ := payload["response"].(map[string]any)
	var mergeBodyData any
	var mergeHeaders any
	if mergeRequest != nil {
		mergeBodyData = mergeRequest["body_data"]
		mergeHeaders = mergeRequest["headers"]
	}
	if mergeMethod != "POST" || !urlIsString {
		return model.RemoteEntry{}, model.NewWpsAPIError("invalid multipart merge instruction", 0, model.WpsCategoryUpstream)
	}
	if mergeRequest == nil || mergeRequest["body_type"] != "data" {
		return model.RemoteEntry{}, model.NewWpsAPIError("invalid multipart merge request instruction", 0, model.WpsCategoryUpstream)
	}
	mergeBodyDataText, bodyIsString := mergeBodyData.(string)
	mergeHeadersMap, headersIsMap := mergeHeaders.(map[string]any)
	if !bodyIsString || !headersIsMap {
		return model.RemoteEntry{}, model.NewWpsAPIError("multipart merge body is missing", 0, model.WpsCategoryUpstream)
	}
	mergeContentTypeValue, _ := firstHeaderValue(mergeHeadersMap, "Content-Type", "content-type")
	mergeContentType, _ := mergeContentTypeValue.(string)
	if mergeContentType != "application/xml" {
		return model.RemoteEntry{}, model.NewWpsAPIError("unsupported multipart merge content type", 0, model.WpsCategoryUpstream)
	}
	if mergeResponse == nil {
		return model.RemoteEntry{}, model.NewWpsAPIError("invalid multipart merge response instruction", 0, model.WpsCategoryUpstream)
	}
	if !expectFirstCode200(mergeResponse) {
		return model.RemoteEntry{}, model.NewWpsAPIError("unsupported multipart merge status", 0, model.WpsCategoryUpstream)
	}
	mergedBody, err := c.postSignedData(mergeURL, []byte(mergeBodyDataText), mergeContentType)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	mergedEtag, err := multipartEtag(mergedBody)
	if err != nil {
		return model.RemoteEntry{}, err
	}

	fileBody := &pyObject{
		keys: []string{"key", "groupid", "parentid", "name", "parent_path", "sha1",
			"size", "store", "etag", "isUpNewVer", "apiErrorInfo", "csrfmiddlewaretoken"},
		values: map[string]any{
			"key":                 state.key,
			"groupid":             groupText,
			"parentid":            parentText,
			"name":                name,
			"parent_path":         pyStringList(options.ParentPath),
			"sha1":                spool.sha1,
			"size":                pyInt(spool.total),
			"store":               state.store,
			"etag":                mergedEtag,
			"isUpNewVer":          options.IsUpNewVer,
			"apiErrorInfo":        nil,
			"csrfmiddlewaretoken": spool.csrf,
		},
	}
	encoded, err = dumpPYValue(fileBody)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	finalPayload, err := c.RequestJSON(JSONRequest{
		Path:       "/3rd/drive/api/v5/files/file",
		Method:     http.MethodPost,
		Body:       encoded,
		RetryOn401: true,
	})
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if result, present := finalPayload["result"]; present && result != nil && result != "ok" {
		return model.RemoteEntry{}, model.NewWpsAPIError("register multipart file", 0, model.WpsCategoryUpstream)
	}
	entry, err := entryFromItem(finalPayload)
	if err != nil {
		return model.RemoteEntry{}, err
	}
	if err := removeResumeCheckpoint(resumePath); err != nil {
		return model.RemoteEntry{}, err
	}
	return entry, nil
}

// postSignedData mirrors _post_signed_data: one POST of the instruction
// body over the credential-free signed transport with the instructed
// Content-Type, bounded to the XML response ceiling before the status
// gate, returning the raw XML body.
func (c *Client) postSignedData(signedURL string, body []byte, contentType string) ([]byte, error) {
	response, err := c.signed.Do("multipart merge", http.MethodPost, signedURL, []SignedHeader{
		{Name: "Content-Type", Value: contentType},
	}, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := readLimitedResponse(response.Body, response.ContentLength, MaxXMLResponseBytes, "multipart merge", model.WpsCategoryUpstream)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, model.NewWpsAPIError("multipart merge", response.StatusCode, model.WpsCategoryUpstream)
	}
	return payload, nil
}

// multipartEtag mirrors _multipart_etag: doctype and entity constructs are
// refused outright, the whole XML document must be well formed exactly
// like ElementTree.fromstring (unclosed elements, a second root, junk
// around the root, and an empty document all fail), and then the first
// ETag element's leading text is normalized (whitespace and quotes
// stripped). An ETag element with whitespace-only text normalizes to the
// empty string exactly like Python.
func multipartEtag(body []byte) (string, error) {
	lowered := bytes.ToLower(body)
	if bytes.Contains(lowered, []byte("<!doctype")) || bytes.Contains(lowered, []byte("<!entity")) {
		return "", model.NewWpsAPIError("parse multipart merge response", 0, model.WpsCategoryUpstream)
	}
	parseFailed := func() (string, error) {
		return "", model.NewWpsAPIError("parse multipart merge response", 0, model.WpsCategoryUpstream)
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var capturing bool
	var text []byte
	var firstETag string
	etagFound := false
	depth := 0
	sawRoot := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if depth != 0 || !sawRoot {
				return parseFailed()
			}
			break
		}
		if err != nil {
			return parseFailed()
		}
		switch element := token.(type) {
		case xml.StartElement:
			if depth == 0 && sawRoot {
				return parseFailed()
			}
			sawRoot = true
			if capturing {
				// element.text only spans the bytes before the first child.
				if len(text) > 0 && !etagFound {
					firstETag = normalizeEtag(string(text))
					etagFound = true
				}
				capturing = false
			}
			if element.Name.Local == "ETag" {
				capturing = true
				text = text[:0]
			}
			depth++
		case xml.CharData:
			if depth == 0 {
				if len(bytes.TrimSpace(element)) > 0 {
					return parseFailed()
				}
				continue
			}
			if capturing {
				text = append(text, element...)
			}
		case xml.EndElement:
			if depth == 0 {
				return parseFailed()
			}
			depth--
			if capturing {
				if len(text) > 0 && !etagFound {
					firstETag = normalizeEtag(string(text))
					etagFound = true
				}
				capturing = false
			}
		}
	}
	if etagFound {
		return firstETag, nil
	}
	return "", model.NewWpsAPIError("multipart merge response missing ETag", 0, model.WpsCategoryUpstream)
}
