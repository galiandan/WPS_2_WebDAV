// The download flow ports client.py's open_download: one control-plane
// resolve request against /api/v3/office/file/{id}/download, one observed
// 403 fallback that adds get_direct_external_download_url=true, and a signed
// object GET over the credential-free transport. Signed URLs are validated
// before any object traffic, and errors never echo them.

package wps

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// DefaultDownloadChecksums mirrors open_download's checksums default: the
// digest names requested from the download control endpoint.
var DefaultDownloadChecksums = []string{"md5", "sha1", "sha224", "sha256", "sha384", "sha512"}

// DownloadStream is a streaming object-storage response returned by
// OpenDownload. The pointer accessors mirror the Python dataclass's
// None-or-value fields.
type DownloadStream struct {
	response      *http.Response
	status        *string
	contentType   *string
	contentLength *int64
	httpStatus    int
	contentRange  *string

	closeOnce sync.Once
}

func (s *DownloadStream) Read(p []byte) (int, error) {
	return s.response.Body.Read(p)
}

// Close releases the object response exactly once, mirroring the Python
// close() guard.
func (s *DownloadStream) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.response.Body.Close() })
	return err
}

func (s *DownloadStream) HTTPStatus() int { return s.httpStatus }

func (s *DownloadStream) ContentType() *string { return s.contentType }

func (s *DownloadStream) ContentLength() *int64 { return s.contentLength }

func (s *DownloadStream) ContentRange() *string { return s.contentRange }

// Status exposes the resolve payload's "status" string, nil unless the
// control response carried one.
func (s *DownloadStream) Status() *string { return s.status }

// OpenDownload mirrors open_download with the storage-facing surface: the
// default checksum list, the direct-download flag unset so the observed 403
// fallback can enable it once, and cid falling back to the configured value.
func (c *Client) OpenDownload(fileID string, offset int64, length *int64, cid *string) (*DownloadStream, error) {
	if offset < 0 {
		return nil, errors.New("offset must not be negative")
	}
	if length != nil && *length <= 0 {
		return nil, errors.New("length must be positive")
	}
	rangeRequested := offset != 0 || length != nil
	if rangeRequested && !c.config.EnableRange {
		return nil, model.NewWpsAPIError(
			"range download is disabled until independently verified", 0, model.WpsCategoryUpstream)
	}

	effectiveCID := cid
	if effectiveCID == nil && c.config.CID != "" {
		configured := c.config.CID
		effectiveCID = &configured
	}
	path := "/api/v3/office/file/" + quotePathSegment(fileID) + "/download"
	checksums := strings.Join(DefaultDownloadChecksums, ",")
	if c.personal() {
		groupID, err := c.GroupID()
		if err != nil {
			return nil, err
		}
		path = "/api/v5/groups/" + quotePathSegment(groupID) + "/files/" +
			quotePathSegment(fileID) + "/download"
		checksums = "sha1"
	}

	// resolve mirrors the nested resolve closure: the direct flag is omitted
	// until the observed 403 asks for it, matching _bool(true) = "true".
	resolve := func(direct bool) (map[string]any, error) {
		query := []QueryPair{{
			Key:   "support_checksums",
			Value: checksums,
		}}
		if direct && !c.personal() {
			query = append(query, QueryPair{
				Key:   "get_direct_external_download_url",
				Value: "true",
			})
		}
		if effectiveCID != nil && !c.personal() {
			query = append(query, QueryPair{Key: "cid", Value: *effectiveCID})
		}
		return c.RequestJSON(JSONRequest{Path: path, Query: query, RetryOn401: true})
	}

	payload, err := resolve(false)
	if err != nil {
		if c.personal() {
			return nil, err
		}
		var apiErr *model.WpsAPIError
		if errors.As(err, &apiErr) && apiErr.Status == 403 {
			payload, err = resolve(true)
		}
		if err != nil {
			return nil, err
		}
	}

	// Python's `payload.get("download_url") or payload.get("url")`: any
	// falsy first value falls through to the second, even a non-string one.
	raw := payload["download_url"]
	if !pyTruthy(raw) {
		raw = payload["url"]
	}
	signedURL, isString := raw.(string)
	if !isString || !strings.HasPrefix(signedURL, "https://") {
		return nil, model.NewWpsAPIError("resolve download URL", 0, model.WpsCategoryUpstream)
	}
	if _, err := ParseSignedTarget(signedURL, "resolve download URL", c.config.ObjectStorageHostSuffix); err != nil {
		return nil, err
	}

	headers := []SignedHeader{{Name: "Accept", Value: "*/*"}}
	if rangeRequested {
		end := ""
		if length != nil {
			end = strconv.FormatInt(offset+*length-1, 10)
		}
		headers = append(headers, SignedHeader{
			Name:  "Range",
			Value: fmt.Sprintf("bytes=%d-%s", offset, end),
		})
	}
	response, err := c.signed.Do("object download", http.MethodGet, signedURL, headers, nil, 0)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		response.Body.Close()
		return nil, model.NewWpsAPIError("object download", response.StatusCode, model.WpsCategoryUpstream)
	}
	responseStatus := response.StatusCode
	if rangeRequested && responseStatus != http.StatusPartialContent {
		response.Body.Close()
		return nil, model.NewWpsAPIError("range download was not honored", responseStatus, model.WpsCategoryUpstream)
	}
	contentType := headerString(response.Header, "Content-Type")
	contentLength := headerInt(response.Header, "Content-Length")
	contentRange := headerString(response.Header, "Content-Range")
	if rangeRequested && !rangeResponseMatches(contentRange, offset, length, contentLength) {
		response.Body.Close()
		return nil, model.NewWpsAPIError(
			"range response metadata was not honored", responseStatus, model.WpsCategoryUpstream)
	}
	var status *string
	if value, ok := payload["status"].(string); ok {
		status = &value
	}
	return &DownloadStream{
		response:      response,
		status:        status,
		contentType:   contentType,
		contentLength: contentLength,
		httpStatus:    responseStatus,
		contentRange:  contentRange,
	}, nil
}

// headerString mirrors headers.get(name): the first value, or nil.
func headerString(header http.Header, name string) *string {
	values := header.Values(name)
	if len(values) == 0 {
		return nil
	}
	return &values[0]
}

// headerInt mirrors int(headers.get(name, "")) with the TypeError/ValueError
// swallow: surrounding whitespace is tolerated and any malformed value
// degrades to nil.
func headerInt(header http.Header, name string) *int64 {
	values := header.Values(name)
	if len(values) == 0 {
		return nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

// rangeHeaderPattern pins the accepted Content-Range shape of
// _range_response_matches.
var rangeHeaderPattern = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+|\*)$`)

// rangeResponseMatches mirrors _range_response_matches: the object response
// must carry a bytes Content-Range whose start equals the requested offset,
// whose covered length equals the response Content-Length and — for a finite
// request — the requested length, and whose total exceeds the end or is
// unknown.
func rangeResponseMatches(value *string, offset int64, length *int64, contentLength *int64) bool {
	if value == nil || *value == "" || contentLength == nil || *contentLength < 0 {
		return false
	}
	match := rangeHeaderPattern.FindStringSubmatch(strings.TrimSpace(*value))
	if match == nil {
		return false
	}
	start, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		// A start beyond int64 can never equal the requested offset.
		return false
	}
	end, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil {
		// An oversized end can never match a bounded content length either.
		return false
	}
	if start != offset || end < start {
		return false
	}
	covered := end - start
	if covered == math.MaxInt64 {
		// covered+1 overflows here; Python's big integer could then never
		// equal any parsed Content-Length.
		return false
	}
	if covered+1 != *contentLength {
		return false
	}
	if length != nil && *contentLength != *length {
		return false
	}
	if match[3] == "*" {
		return true
	}
	total, err := strconv.ParseInt(match[3], 10, 64)
	if err != nil {
		// An oversized total is greater than any 64-bit end.
		return true
	}
	return total > end
}
