// The Range layer ports server.py's _parse_range and _if_range_matches.
// Only the current ETag semantics are supported: a date If-Range never
// matches and yields the full download. Multiple ranges are rejected like
// Python (a comma in the spec is unsatisfiable), and 416 answers carry
// "bytes */N" or "bytes */*" without touching the download slot.

package httpserver

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// rangeNotSatisfiable mirrors _RangeNotSatisfiable: the parser rejection
// carrying the entry size for the 416 Content-Range (nil renders "*").
type rangeNotSatisfiable struct {
	size *int64
}

func (e *rangeNotSatisfiable) Error() string {
	return "requested byte range cannot be satisfied"
}

// parseRangeHeader mirrors _parse_range: exactly one byte range, in closed,
// open, or suffix form, clamped to the entry size. The returned length is
// end - start + 1, so a zero-size entry with a suffix range yields the
// Python (0, 0) result whose "bytes 0--1/0" header framing stays reachable.
func parseRangeHeader(value string, size *int64) (int64, int64, error) {
	if size == nil || *size < 0 {
		return 0, 0, &rangeNotSatisfiable{size: size}
	}
	unit, spec, found := strings.Cut(value, "=")
	if !found || strings.ToLower(strings.TrimSpace(unit)) != "bytes" || strings.Contains(spec, ",") {
		return 0, 0, &rangeNotSatisfiable{size: size}
	}
	startText, endText, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, &rangeNotSatisfiable{size: size}
	}
	// Python int() is unbounded; Go's ErrRange clamp agrees with every
	// downstream comparison (start >= size, min against size - 1,
	// size - suffix < 0), but the sign decides between the extremes.
	parse := func(text string) (int64, bool) {
		value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if errors.Is(err, strconv.ErrRange) {
			if strings.HasPrefix(strings.TrimSpace(text), "-") {
				return math.MinInt64, true
			}
			return math.MaxInt64, true
		}
		if err != nil {
			return 0, false
		}
		return value, true
	}
	if startText == "" {
		suffix, ok := parse(endText)
		if !ok || suffix <= 0 {
			return 0, 0, &rangeNotSatisfiable{size: size}
		}
		start := *size - suffix
		if start < 0 {
			start = 0
		}
		end := *size - 1
		return start, end - start + 1, nil
	}
	start, ok := parse(startText)
	if !ok || start < 0 || start >= *size {
		return 0, 0, &rangeNotSatisfiable{size: size}
	}
	end := *size - 1
	if endText != "" {
		parsed, parsedOK := parse(endText)
		if !parsedOK {
			return 0, 0, &rangeNotSatisfiable{size: size}
		}
		if parsed < end {
			end = parsed
		}
	}
	if end < start {
		return 0, 0, &rangeNotSatisfiable{size: size}
	}
	return start, end - start + 1, nil
}

// ifRangeMatches mirrors _if_range_matches: no header always matches, an
// entry without an ETag never does, and only the current ETag (quoted or
// bare) counts — date forms are not extended.
func ifRangeMatches(ifRange string, entry model.RemoteEntry) bool {
	if ifRange == "" {
		return true
	}
	if entry.Etag == nil || *entry.Etag == "" {
		return false
	}
	bare := strings.Trim(*entry.Etag, `"`)
	value := strings.TrimSpace(ifRange)
	return value == bare || value == `"`+bare+`"`
}

// resolveRange mirrors the shared Range/If-Range prelude of _send_download.
// ok=false means the 416 answer has been written and the caller stops; a
// mismatched or absent If-Range yields requested=false with ok=true.
func resolveRange(w http.ResponseWriter, r *http.Request, entry model.RemoteEntry, rest bool) (offset int64, length *int64, requested bool, ok bool) {
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" || !ifRangeMatches(r.Header.Get("If-Range"), entry) {
		return 0, nil, false, true
	}
	start, count, err := parseRangeHeader(rangeHeader, entry.Size)
	if err != nil {
		var unsatisfiable *rangeNotSatisfiable
		if errors.As(err, &unsatisfiable) {
			sizeText := "*"
			if unsatisfiable.size != nil {
				sizeText = strconv.FormatInt(*unsatisfiable.size, 10)
			}
			// Python reaches _send_error without close_connection, so the
			// 416 keeps the connection framing of a plain text/JSON error.
			sendError(w, r, http.StatusRequestedRangeNotSatisfiable,
				"requested byte range cannot be satisfied", rest,
				map[string]string{"Content-Range": "bytes */" + sizeText}, false)
			return 0, nil, false, false
		}
		return 0, nil, false, false
	}
	return start, &count, true, true
}
