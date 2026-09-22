package httpserver

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const (
	zipBrowseArchiveLimit    int64 = 10 << 30
	zipBrowseDirectoryLimit        = 8 << 20
	zipBrowseEntryLimit            = 10000
	zipBrowseNameLimit             = 4 << 20
	zipBrowseOutputLimit           = 128 << 20
	zipBrowseCompressedLimit       = 64 << 20
	zipBrowseReadLimit       int64 = 74 << 20
	zipBrowseRequestLimit          = 300
	zipBrowseBlockSize       int64 = 256 << 10
	zipBrowseDuration              = 2 * time.Minute
)

// Shared across administrators and member dispatchers: opening more roots must
// not multiply ZIP parser buffers or inflate concurrency.
var zipBrowseActive = make(chan struct{}, 2)
var zipBrowseWaiting = make(chan struct{}, 8)

func zipBrowsePermit(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case zipBrowseWaiting <- struct{}{}:
	default:
		return nil, model.NewStorageError(model.KindServiceBusy, "ZIP browser queue is full")
	}
	defer func() { <-zipBrowseWaiting }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case zipBrowseActive <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-zipBrowseActive
			return nil, err
		}
		return func() { <-zipBrowseActive }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, model.NewStorageError(model.KindServiceBusy, "ZIP browser is busy")
	}
}

func zipInvalid() error {
	return model.NewStorageError(model.KindBadRequest, "ZIP archive is malformed or contains unsafe paths")
}
func zipUnsupported() error {
	return model.NewStorageError(model.KindUnsupportedOperation, "ZIP contains encryption, unsupported compression, links or multiple disks")
}
func zipLimit() error {
	return model.NewStorageError(model.KindInsufficientStorage, "ZIP archive exceeds a browsing or extraction limit")
}
func zipChanged() error {
	return model.NewStorageError(model.KindAlreadyExists, "ZIP archive or storage changed; reopen the archive")
}

type zipSegment struct {
	offset int64
	body   []byte
}
type zipRangeReader struct {
	d         *RESTDispatcher
	ctx       context.Context
	path      string
	entry     model.RemoteEntry
	identity  [32]byte
	size      int64
	segments  []zipSegment
	block     zipSegment
	requests  int
	readBytes int64
}

func (z *zipRangeReader) verify(fresh bool) error {
	if err := z.ctx.Err(); err != nil {
		return err
	}
	identity, err := z.d.textIdentity()
	if err != nil || identity != z.identity {
		return zipChanged()
	}
	if fresh {
		z.d.invalidateTextMetadata()
	}
	entry, err := z.d.downloads.Metadata(z.path)
	if err != nil {
		return err
	}
	if entry.Kind != model.KindFile || entry.ID != z.entry.ID || !sameArchiveOptional(entry.Size, z.entry.Size) || !sameArchiveOptional(entry.Etag, z.entry.Etag) || !sameArchiveOptional(entry.ModifiedAt, z.entry.ModifiedAt) {
		return zipChanged()
	}
	return nil
}

func (z *zipRangeReader) remote(offset, length int64) ([]byte, error) {
	if length <= 0 || offset < 0 || offset > z.size || length > z.size-offset {
		return nil, zipInvalid()
	}
	if z.requests >= zipBrowseRequestLimit || length > zipBrowseReadLimit-z.readBytes {
		return nil, zipLimit()
	}
	if err := z.verify(false); err != nil {
		return nil, err
	}
	z.requests++
	z.readBytes += length
	stream, err := z.d.downloads.OpenPath(z.ctx, z.path, offset, &length)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	stop := context.AfterFunc(z.ctx, func() { stream.Close() })
	defer stop()
	wantRange := fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, z.size)
	if stream.HTTPStatus() != http.StatusPartialContent || stream.ContentLength() == nil || *stream.ContentLength() != length || stream.ContentRange() == nil || *stream.ContentRange() != wantRange {
		return nil, model.NewWpsAPIError("ZIP byte range was not honored", 0, model.WpsCategoryUpstream)
	}
	body, err := io.ReadAll(io.LimitReader(stream, length+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != length {
		return nil, zipChanged()
	}
	if err := z.ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

// ReaderAt caches only the bounded central directory, tail and one data block.
// Deflate's small reads therefore become a bounded sequence of range requests.
func (z *zipRangeReader) ReadAt(buffer []byte, offset int64) (int, error) {
	if err := z.ctx.Err(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, zipInvalid()
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if offset >= z.size {
		return 0, io.EOF
	}
	written := 0
	for len(buffer) > 0 && offset < z.size {
		var data []byte
		for _, segment := range z.segments {
			if offset >= segment.offset && offset < segment.offset+int64(len(segment.body)) {
				data = segment.body[offset-segment.offset:]
				break
			}
		}
		if data == nil && offset >= z.block.offset && offset < z.block.offset+int64(len(z.block.body)) {
			data = z.block.body[offset-z.block.offset:]
		}
		if data == nil {
			start := offset / zipBrowseBlockSize * zipBrowseBlockSize
			length := min(zipBrowseBlockSize, z.size-start)
			body, err := z.remote(start, length)
			if err != nil {
				return written, err
			}
			z.block = zipSegment{start, body}
			data = body[offset-start:]
		}
		n := copy(buffer, data)
		buffer = buffer[n:]
		written += n
		offset += int64(n)
	}
	if len(buffer) > 0 {
		return written, io.EOF
	}
	return written, nil
}

type zipRecord struct {
	name                     string
	folder                   bool
	method, flags            uint16
	size, compressed, offset uint64
}
type zipMember struct {
	Name           string `json:"name"`
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	Size           uint64 `json:"size"`
	CompressedSize uint64 `json:"compressed_size"`
	Method         string `json:"method,omitempty"`
	file           *zip.File
	record         zipRecord
	explicit       bool
}
type browsedZIP struct {
	rangeReader     *zipRangeReader
	reader          *zip.Reader
	directoryOffset uint64
	members         map[string]*zipMember
	count           int
}

// Parse EOCD/ZIP64 and every central record before archive/zip is allowed to
// allocate its File slice. Go intentionally trusts only the low 16 count bits;
// this preflight enforces full counts, byte bounds and names independently.
func zipDirectory(z *zipRangeReader) (uint64, []zipRecord, error) {
	tailLength := min(z.size, int64(65535+22))
	tailOffset := z.size - tailLength
	tail, err := z.remote(tailOffset, tailLength)
	if err != nil {
		return 0, nil, err
	}
	z.segments = append(z.segments, zipSegment{tailOffset, tail})
	end := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == 0x06054b50 && i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) == len(tail) {
			end = i
			break
		}
	}
	if end < 0 {
		return 0, nil, zipInvalid()
	}
	eocd := tail[end:]
	endOffset := uint64(tailOffset + int64(end))
	if binary.LittleEndian.Uint16(eocd[4:]) != 0 || binary.LittleEndian.Uint16(eocd[6:]) != 0 || binary.LittleEndian.Uint16(eocd[8:]) != binary.LittleEndian.Uint16(eocd[10:]) {
		return 0, nil, zipUnsupported()
	}
	count := uint64(binary.LittleEndian.Uint16(eocd[10:]))
	directorySize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	directoryOffset := uint64(binary.LittleEndian.Uint32(eocd[16:]))
	directoryEnd := endOffset
	if count == 65535 || directorySize == 0xffffffff || directoryOffset == 0xffffffff {
		if endOffset < 20 {
			return 0, nil, zipInvalid()
		}
		locator := make([]byte, 20)
		if _, err := z.ReadAt(locator, int64(endOffset)-20); err != nil {
			return 0, nil, err
		}
		if binary.LittleEndian.Uint32(locator) != 0x07064b50 || binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
			return 0, nil, zipUnsupported()
		}
		offset := binary.LittleEndian.Uint64(locator[8:])
		if offset > endOffset-20 || endOffset-20-offset < 56 {
			return 0, nil, zipInvalid()
		}
		zip64 := make([]byte, 56)
		if _, err := z.ReadAt(zip64, int64(offset)); err != nil {
			return 0, nil, err
		}
		recordSize := binary.LittleEndian.Uint64(zip64[4:])
		if binary.LittleEndian.Uint32(zip64) != 0x06064b50 || recordSize < 44 || recordSize > 1024 || offset+12+recordSize != endOffset-20 {
			return 0, nil, zipInvalid()
		}
		if binary.LittleEndian.Uint32(zip64[16:]) != 0 || binary.LittleEndian.Uint32(zip64[20:]) != 0 || binary.LittleEndian.Uint64(zip64[24:]) != binary.LittleEndian.Uint64(zip64[32:]) {
			return 0, nil, zipUnsupported()
		}
		count = binary.LittleEndian.Uint64(zip64[32:])
		directorySize = binary.LittleEndian.Uint64(zip64[40:])
		directoryOffset = binary.LittleEndian.Uint64(zip64[48:])
		directoryEnd = offset
	}
	if count > zipBrowseEntryLimit || directorySize > zipBrowseDirectoryLimit {
		return 0, nil, zipLimit()
	}
	if directoryOffset > directoryEnd || directorySize != directoryEnd-directoryOffset || (count == 0) != (directorySize == 0) {
		return 0, nil, zipInvalid()
	}
	if count == 0 {
		return directoryOffset, nil, nil
	}
	if directorySize < count*46 {
		return 0, nil, zipInvalid()
	}
	directory, err := z.remote(int64(directoryOffset), int64(directorySize))
	if err != nil {
		return 0, nil, err
	}
	z.segments = append(z.segments, zipSegment{int64(directoryOffset), directory})
	records := make([]zipRecord, 0, int(count))
	nameBytes := 0
	for cursor := 0; cursor < len(directory); {
		if err := z.ctx.Err(); err != nil {
			return 0, nil, err
		}
		if len(records) >= int(count) || len(directory)-cursor < 46 {
			return 0, nil, zipInvalid()
		}
		header := directory[cursor : cursor+46]
		if binary.LittleEndian.Uint32(header) != 0x02014b50 {
			return 0, nil, zipInvalid()
		}
		nameLength := int(binary.LittleEndian.Uint16(header[28:]))
		extraLength := int(binary.LittleEndian.Uint16(header[30:]))
		commentLength := int(binary.LittleEndian.Uint16(header[32:]))
		length := 46 + nameLength + extraLength + commentLength
		if length > len(directory)-cursor || nameLength == 0 {
			return 0, nil, zipInvalid()
		}
		nameBytes += nameLength
		if nameBytes > zipBrowseNameLimit || nameLength > 4096 {
			return 0, nil, zipLimit()
		}
		record := zipRecord{name: string(directory[cursor+46 : cursor+46+nameLength]), method: binary.LittleEndian.Uint16(header[10:]), flags: binary.LittleEndian.Uint16(header[8:]), size: uint64(binary.LittleEndian.Uint32(header[24:])), compressed: uint64(binary.LittleEndian.Uint32(header[20:])), offset: uint64(binary.LittleEndian.Uint32(header[42:]))}
		extra := directory[cursor+46+nameLength : cursor+46+nameLength+extraLength]
		if err := zip64Record(&record, extra); err != nil {
			return 0, nil, err
		}
		if binary.LittleEndian.Uint16(header[34:]) != 0 || record.flags&0x2041 != 0 || (record.method != zip.Store && record.method != zip.Deflate) {
			return 0, nil, zipUnsupported()
		}
		if !utf8.ValidString(record.name) || strings.Contains(record.name, ":") {
			return 0, nil, zipInvalid()
		}
		record.folder = strings.HasSuffix(record.name, "/")
		canonical := strings.TrimSuffix(record.name, "/")
		parts, err := storage.SplitRemotePath("/" + canonical)
		if err != nil || len(parts) == 0 || len(parts) > 64 {
			return 0, nil, zipInvalid()
		}
		joined, _ := storage.JoinRemotePath(parts, false)
		if joined != "/"+canonical {
			return 0, nil, zipInvalid()
		}
		for _, part := range parts {
			if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
				return 0, nil, zipInvalid()
			}
		}
		if header[5] == 3 || header[5] == 19 {
			kind := binary.LittleEndian.Uint32(header[38:]) >> 16 & 0170000
			if kind != 0 && kind != 0100000 && kind != 0040000 {
				return 0, nil, zipUnsupported()
			}
			if kind == 0040000 && !record.folder {
				return 0, nil, zipInvalid()
			}
		}
		if binary.LittleEndian.Uint32(header[38:])&0x10 != 0 && !record.folder {
			return 0, nil, zipInvalid()
		}
		if record.offset > directoryOffset || directoryOffset-record.offset < 30 || record.compressed > directoryOffset-record.offset-30 {
			return 0, nil, zipInvalid()
		}
		if record.folder && record.size != 0 {
			return 0, nil, zipInvalid()
		}
		records = append(records, record)
		cursor += length
	}
	if len(records) != int(count) {
		return 0, nil, zipInvalid()
	}
	return directoryOffset, records, nil
}

func zip64Record(record *zipRecord, extra []byte) error {
	needed := record.size == 0xffffffff || record.compressed == 0xffffffff || record.offset == 0xffffffff
	seen := false
	for len(extra) > 0 {
		if len(extra) < 4 {
			return zipInvalid()
		}
		tag, n := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		extra = extra[4:]
		if n > len(extra) {
			return zipInvalid()
		}
		value := extra[:n]
		extra = extra[n:]
		if tag != 1 {
			continue
		}
		if seen {
			return zipInvalid()
		}
		seen = true
		for _, field := range []*uint64{&record.size, &record.compressed, &record.offset} {
			if *field == 0xffffffff {
				if len(value) < 8 {
					return zipInvalid()
				}
				*field = binary.LittleEndian.Uint64(value)
				value = value[8:]
			}
		}
	}
	if needed && !seen {
		return zipInvalid()
	}
	return nil
}

func (d *RESTDispatcher) openZIP(ctx context.Context, source string) (*browsedZIP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.invalidateTextMetadata()
	entry, err := d.downloads.Metadata(source)
	if err != nil {
		return nil, err
	}
	if entry.Kind != model.KindFile || !strings.EqualFold(path.Ext(entry.Name), ".zip") {
		return nil, model.NewStorageError(model.KindUnsupportedOperation, "select a ZIP file")
	}
	if entry.Size == nil || *entry.Size < 22 {
		return nil, zipInvalid()
	}
	if *entry.Size > zipBrowseArchiveLimit {
		return nil, zipLimit()
	}
	identity, err := d.textIdentity()
	if err != nil {
		return nil, zipChanged()
	}
	remote := &zipRangeReader{d: d, ctx: ctx, path: source, entry: entry, identity: identity, size: *entry.Size}
	directory, records, err := zipDirectory(remote)
	if err != nil {
		return nil, err
	}
	members := map[string]*zipMember{"/": {Path: "/", Kind: "folder"}}
	folded := map[string]string{"/": "/"}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parts := strings.Split(strings.TrimSuffix(record.name, "/"), "/")
		current := ""
		for i, name := range parts {
			current += "/" + name
			folder := i < len(parts)-1 || record.folder
			key := strings.ToLower(current)
			if previous, ok := folded[key]; ok && previous != current {
				return nil, model.NewStorageError(model.KindAmbiguousPath, "ZIP contains ambiguous names")
			}
			folded[key] = current
			node, exists := members[current]
			if exists {
				if node.Kind != "folder" || !folder || (i == len(parts)-1 && node.explicit) {
					return nil, model.NewStorageError(model.KindAmbiguousPath, "ZIP contains duplicate or conflicting names")
				}
			} else {
				if len(members) > zipBrowseEntryLimit {
					return nil, zipLimit()
				}
				kind := "file"
				if folder {
					kind = "folder"
				}
				node = &zipMember{Name: name, Path: current, Kind: kind}
				members[current] = node
			}
			if i == len(parts)-1 {
				node.explicit = true
				node.record = record
				node.Size = record.size
				node.CompressedSize = record.compressed
				if !folder {
					node.Method = "store"
					if record.method == zip.Deflate {
						node.Method = "deflate"
					}
				}
			}
		}
	}
	reader, err := zip.NewReader(remote, remote.size)
	if err != nil {
		return nil, zipInvalid()
	}
	if len(reader.File) != len(records) {
		return nil, zipInvalid()
	}
	for i, file := range reader.File {
		record := records[i]
		if file.Name != record.name || file.UncompressedSize64 != record.size || file.CompressedSize64 != record.compressed {
			return nil, zipInvalid()
		}
		members["/"+strings.TrimSuffix(file.Name, "/")].file = file
	}
	if err := remote.verify(true); err != nil {
		return nil, err
	}
	return &browsedZIP{rangeReader: remote, reader: reader, directoryOffset: directory, members: members, count: len(records)}, nil
}

func zipMemberPath(query url.Values) (string, error) {
	values, exists := query["entry"]
	if !exists {
		return "/", nil
	}
	if len(values) != 1 || values[0] == "" {
		return "", errBadRequest("entry must contain one ZIP path")
	}
	return canonicalRemotePath(values[0])
}

func (d *RESTDispatcher) doZIPBrowse(w http.ResponseWriter, r *http.Request, route RESTRoute, download bool) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	source, err := queryPath(route.Query)
	if err != nil {
		return err
	}
	source, err = canonicalRemotePath(source)
	if err != nil {
		return err
	}
	memberPath, err := zipMemberPath(route.Query)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), zipBrowseDuration)
	defer cancel()
	release, err := zipBrowsePermit(ctx)
	if err != nil {
		return err
	}
	defer release()
	archive, err := d.openZIP(ctx, source)
	if err != nil {
		return err
	}
	member, ok := archive.members[memberPath]
	if !ok {
		return model.NewStorageError(model.KindEntryNotFound, "ZIP entry not found")
	}
	if download {
		return d.downloadZIPMember(w, r, archive, member)
	}
	if member.Kind != "folder" {
		return model.NewStorageError(model.KindNotFolder, "ZIP entry is not a folder")
	}
	entries := make([]zipMember, 0)
	for name, entry := range archive.members {
		if name != "/" && path.Dir(name) == memberPath {
			entries = append(entries, *entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind == "folder"
		}
		return entries[i].Name < entries[j].Name
	})
	return sendJSON(w, r, http.StatusOK, struct {
		Path         string      `json:"path"`
		Entry        string      `json:"entry"`
		Entries      []zipMember `json:"entries"`
		TotalEntries int         `json:"total_entries"`
	}{source, memberPath, entries, archive.count}, d.limits, nil)
}

func (d *RESTDispatcher) downloadZIPMember(w http.ResponseWriter, r *http.Request, archive *browsedZIP, member *zipMember) error {
	if member.Kind != "file" || member.file == nil {
		return model.NewStorageError(model.KindNotFolder, "select a file inside the ZIP")
	}
	record := member.record
	if record.size > zipBrowseOutputLimit || record.compressed > zipBrowseCompressedLimit || (record.size > 0 && (record.compressed == 0 || record.size > record.compressed*200)) {
		return zipLimit()
	}
	local := make([]byte, 30)
	if _, err := archive.rangeReader.ReadAt(local, int64(record.offset)); err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(local) != 0x04034b50 || binary.LittleEndian.Uint16(local[6:]) != record.flags || binary.LittleEndian.Uint16(local[8:]) != record.method {
		return zipInvalid()
	}
	nameLength, extraLength := uint64(binary.LittleEndian.Uint16(local[26:])), uint64(binary.LittleEndian.Uint16(local[28:]))
	dataOffset := record.offset + 30 + nameLength + extraLength
	if dataOffset > archive.directoryOffset || record.compressed > archive.directoryOffset-dataOffset {
		return zipInvalid()
	}
	name := make([]byte, nameLength)
	if _, err := archive.rangeReader.ReadAt(name, int64(record.offset)+30); err != nil {
		return err
	}
	if string(name) != record.name {
		return zipInvalid()
	}
	actualOffset, err := member.file.DataOffset()
	if err != nil || uint64(actualOffset) != dataOffset {
		return zipInvalid()
	}
	stream, err := member.file.Open()
	if err != nil {
		return zipInvalid()
	}
	defer stream.Close()
	checksum := crc32.NewIEEE()
	checked := io.TeeReader(stream, checksum)
	// Keep the final output chunk until EOF and archive/zip's CRC check. Do
	// not advertise a Content-Length: errors after earlier chunks abort the
	// HTTP stream, so clients cannot accept a complete-looking corrupt file.
	buffer := make([]byte, 64<<10)
	first, err := zipReadChunk(checked, buffer)
	if err != nil && err != io.EOF {
		return zipInvalid()
	}
	if err == io.EOF && (uint64(len(first)) != record.size || checksum.Sum32() != member.file.CRC32) {
		return zipInvalid()
	}
	if err := archive.rangeReader.verify(false); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="download"; filename*=UTF-8''`+pythonQuote(member.Name))
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	var written uint64
	nextBuffer := make([]byte, 64<<10)
	for {
		if archive.rangeReader.ctx.Err() != nil {
			panic(http.ErrAbortHandler)
		}
		if written+uint64(len(first)) > record.size {
			panic(http.ErrAbortHandler)
		}
		if err == io.EOF {
			if written+uint64(len(first)) != record.size || checksum.Sum32() != member.file.CRC32 {
				panic(http.ErrAbortHandler)
			}
			if _, writeErr := w.Write(first); writeErr != nil {
				panic(http.ErrAbortHandler)
			}
			return nil
		}
		next, nextErr := zipReadChunk(checked, nextBuffer)
		if nextErr != nil && nextErr != io.EOF {
			panic(http.ErrAbortHandler)
		}
		if nextErr == io.EOF && (written+uint64(len(first)+len(next)) != record.size || checksum.Sum32() != member.file.CRC32) {
			panic(http.ErrAbortHandler)
		}
		if _, writeErr := w.Write(first); writeErr != nil {
			panic(http.ErrAbortHandler)
		}
		written += uint64(len(first))
		first, err = next, nextErr
		buffer, nextBuffer = nextBuffer, buffer
	}
}

func zipReadChunk(reader io.Reader, buffer []byte) ([]byte, error) {
	n, empty := 0, 0
	for n < len(buffer) {
		count, err := reader.Read(buffer[n:])
		n += count
		if err != nil {
			return buffer[:n], err
		}
		if count == 0 {
			empty++
			if empty >= 100 {
				return buffer[:n], io.ErrNoProgress
			}
		} else {
			empty = 0
		}
	}
	return buffer[:n], nil
}

var _ io.ReaderAt = (*zipRangeReader)(nil)
