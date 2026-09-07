// The B1100-B1104 multipart tests pin the block session flow: the
// checkpoint format and its refusal rules, the block initialization and
// part-size arithmetic, the per-part instruction validation and signed
// PUTs, the bounded session rebuild, and the merge plus registration with
// the checkpoint removed only after registration succeeded.

package wps

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// multipartBodyFixture is the shared small upload: "body" spliced into
// "bo" and "dy" by a 2-byte part size.
const multipartBodyFixture = "body"

const (
	md5Part1Hex    = "ad7532d5b3860a408fbe01f9455dca36"
	md5Part1Base64 = "rXUy1bOGCkCPvgH5RV3KNg=="
	md5Part2Hex    = "8e7dd5d3e76aa952e21999a5537dcffb"
	md5Part2Base64 = "jn3V0+dqqVLiGZmlU33P+w=="
)

func newMultipartClient(t *testing.T, mutate func(*Config), control []scriptedResponse, object []scriptedResponse) (*Client, *fakeControlOpener, *fakeSignedTransport, string) {
	t.Helper()
	client, opener, transport, spoolDir := newUploadObjectClient(t, &recordingLimiter{}, func(c *Config) {
		c.UploadRetryDelay = 0
		if mutate != nil {
			mutate(c)
		}
	}, control, object)
	return client, opener, transport, spoolDir
}

// smallMultipartConfig routes the tiny fixture through the multipart path.
func smallMultipartConfig(resumeDir string) func(*Config) {
	return func(c *Config) {
		c.MultipartThreshold = 2
		c.MultipartPartSize = 2
		c.UploadResumeDir = resumeDir
	}
}

// multipartInitScript is the observed-shape block init response.
func multipartInitScript(uploadID string) scriptedResponse {
	return scriptedResponse{status: 200, body: []byte(
		`{"result":"ok","upload_id":"` + uploadID + `","key":"bench-multipart-key","store":"bench-store",` +
			`"limit":{"min_part_size":1,"max_part_size":67108864,"max_parts":10000}}`)}
}

// multipartInitScriptWithLimit overrides the limit object verbatim.
func multipartInitScriptWithLimit(limit string) scriptedResponse {
	return scriptedResponse{status: 200, body: []byte(
		`{"result":"ok","upload_id":"u1","key":"bench-multipart-key","store":"bench-store","limit":` + limit + `}`)}
}

// multipartPartScript is one part PUT instruction whose Content-MD5 is
// precomputed for the scripted part bytes.
func multipartPartScript(contentMD5 string, partIndex int) scriptedResponse {
	return scriptedResponse{status: 200, body: []byte(
		`{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/parts/` + strconv.Itoa(partIndex) + `","method":"PUT",` +
			`"request":{"body_type":"file","headers":{"Content-MD5":"` + contentMD5 + `","Content-Type":"application/octet-stream"}},` +
			`"response":{"expect_code":[200]}}`)}
}

func multipartPartEtagScript(etag string) scriptedResponse {
	return scriptedResponse{status: 200, header: http.Header{"Etag": []string{etag}}, body: []byte{}}
}

func multipartMergeScript() scriptedResponse {
	return scriptedResponse{status: 200, body: []byte(
		`{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/merge-bench","method":"POST",` +
			`"request":{"body_type":"data","body_data":"<CompleteMultipartUpload/>","headers":{"Content-Type":"application/xml"}},` +
			`"response":{"expect_code":[200]}}`)}
}

func multipartMergedXMLScript(etag string) scriptedResponse {
	return scriptedResponse{status: 200, body: []byte(
		`<CompleteMultipartUploadResult><ETag>` + etag + `</ETag></CompleteMultipartUploadResult>`)}
}

// twoPartScripts is the full successful script pair for the "body"
// fixture: pre_check, init, two part instructions, merge, register plus
// the two signed PUTs and the signed merge POST.
func twoPartScripts() ([]scriptedResponse, []scriptedResponse) {
	control := []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
		multipartInitScript("u1"),
		multipartPartScript(md5Part1Base64, 1),
		multipartPartScript(md5Part2Base64, 2),
		multipartMergeScript(),
		{status: 200, body: []byte(registerEntryPayload)},
	}
	object := []scriptedResponse{
		multipartPartEtagScript(`"etag-1"`),
		multipartPartEtagScript(`"etag-2"`),
		multipartMergedXMLScript(`"bench-merged-etag"`),
	}
	return control, object
}

// twoPartScriptsObject returns just the signed script of twoPartScripts so
// failure faces can vary only the control tail.
func twoPartScriptsObject() []scriptedResponse {
	_, object := twoPartScripts()
	return object
}

// appendRegisterScript swaps the final register response of the two-part
// control script.
func appendRegisterScript(registerBody string) []scriptedResponse {
	control, _ := twoPartScripts()
	return append(control[:5], scriptedResponse{status: 200, body: []byte(registerBody)})
}

// contentMultipartIdentity mirrors the resume identity: the string forms
// of the group and parent ids, the name, the size, and the sha1.
func contentMultipartIdentity(content string) string {
	sum := sha1.Sum([]byte(content))
	return "group-1:3:file:" + strconv.Itoa(len(content)) + ":" + hex.EncodeToString(sum[:])
}

func smallMultipartIdentity() string {
	return contentMultipartIdentity(multipartBodyFixture)
}

// checkpointResumePath derives the expected checkpoint file name: only the
// sha256 of the identity, never the upload name.
func checkpointResumePath(resumeDir string, identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return filepath.Join(resumeDir, hex.EncodeToString(digest[:])+".json")
}

// readCheckpointFile asserts the resume dir holds exactly one file (no
// temp leftovers) and returns its bytes.
func readCheckpointFile(t *testing.T, resumeDir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(resumeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("resume dir holds %v, want exactly the checkpoint", names)
	}
	raw, err := os.ReadFile(filepath.Join(resumeDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// newResumeDir builds the private resume directory the securefile write
// discipline requires (the plan mandates 0600 atomic checkpoints, and the
// parent must be private too).
func newResumeDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "resume")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func requireCheckpointGone(t *testing.T, resumeDir string, identity string) {
	t.Helper()
	if _, err := os.Stat(checkpointResumePath(resumeDir, identity)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("checkpoint must be removed after the successful upload, stat err = %v", err)
	}
}

func TestMultipartSendsExactInitBody(t *testing.T) {
	client, opener, transport, _ := newMultipartClient(t, smallMultipartConfig(""),
		// The init response misses the key field, so the flow stops there;
		// the body pin does not need the rest of the chain.
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			{status: 200, body: []byte(`{"result":"ok","upload_id":"u1","store":"bench-store","limit":{"min_part_size":1,"max_part_size":67108864,"max_parts":10000}}`)},
		},
		nil)
	_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err == nil || err.Error() != "WPS operation failed: multipart initialization response is incomplete" {
		t.Fatalf("error = %v", err)
	}
	if len(opener.bodies) != 2 {
		t.Fatalf("control requests = %d", len(opener.bodies))
	}
	want := `{"with_rapid":true,"hash":"02083f4579e08a612425c0c1a17ee47add783b94","size":4,` +
		`"group_id":"group-1","name":"file","parent_id":"3","tried_store":[],` +
		`"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[1]) != want {
		t.Fatalf("init body = %s, want %s", opener.bodies[1], want)
	}
	if len(transport.requests) != 0 {
		t.Fatal("the incomplete init must not reach the signed transport")
	}
}

func TestMultipartSendsExactPartBodiesMergeAndRegister(t *testing.T) {
	control, object := twoPartScripts()
	client, opener, transport, _ := newMultipartClient(t, smallMultipartConfig(""), control, object)
	entry, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if entry.ID != "9" {
		t.Fatalf("entry = %+v", entry)
	}
	wantPart1 := `{"key":"bench-multipart-key","md5":"` + md5Part1Hex + `","part_number":1,"part_size":2,` +
		`"req_by_internal":false,"store":"bench-store","upload_id":"u1","csrfmiddlewaretoken":"csrf-secret"}`
	wantPart2 := `{"key":"bench-multipart-key","md5":"` + md5Part2Hex + `","part_number":2,"part_size":2,` +
		`"req_by_internal":false,"store":"bench-store","upload_id":"u1","csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[2]) != wantPart1 || string(opener.bodies[3]) != wantPart2 {
		t.Fatalf("part bodies = %s, %s", opener.bodies[2], opener.bodies[3])
	}
	wantMerge := `{"key":"bench-multipart-key","req_by_internal":false,"store":"bench-store",` +
		`"part_infos":[{"etag":"etag-1","part_number":1},{"etag":"etag-2","part_number":2}],` +
		`"upload_id":"u1","csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[4]) != wantMerge {
		t.Fatalf("merge body = %s, want %s", opener.bodies[4], wantMerge)
	}
	wantRegister := `{"key":"bench-multipart-key","groupid":"group-1","parentid":"3",` +
		`"name":"file","parent_path":[],"sha1":"02083f4579e08a612425c0c1a17ee47add783b94","size":4,` +
		`"store":"bench-store","etag":"bench-merged-etag","isUpNewVer":false,"apiErrorInfo":null,` +
		`"csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[5]) != wantRegister {
		t.Fatalf("register body = %s, want %s", opener.bodies[5], wantRegister)
	}
	// The signed part PUTs carry the instructed Content-MD5 and never any
	// credential surface.
	wantMD5 := []string{md5Part1Base64, md5Part2Base64}
	for index, request := range transport.requests[:2] {
		if got := request.Header.Get("Content-MD5"); got != wantMD5[index] {
			t.Fatalf("part %d Content-MD5 = %q, want %q", index+1, got, wantMD5[index])
		}
		if got := request.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Fatalf("part %d Content-Type = %q", index+1, got)
		}
		if request.ContentLength != 2 {
			t.Fatalf("part %d Content-Length = %d, want 2", index+1, request.ContentLength)
		}
		for _, forbidden := range []string{"Cookie", "Authorization"} {
			if len(request.Header.Values(forbidden)) != 0 {
				t.Fatalf("part %d carries a %s header", index+1, forbidden)
			}
		}
	}
	if got := transport.requests[2].Header.Get("Content-Type"); got != "application/xml" {
		t.Fatalf("merge Content-Type = %q", got)
	}
}

func TestMultipartRefusesOverwriteAtThresholdAfterPreCheck(t *testing.T) {
	client, opener, transport, _ := newMultipartClient(t, smallMultipartConfig(""),
		[]scriptedResponse{{status: 200, body: []byte(`{"result":"ok"}`)}}, nil)
	_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture), Overwrite: true})
	if err == nil || err.Error() != "multipart overwrite is disabled until independently verified" {
		t.Fatalf("error = %v", err)
	}
	storageErr, ok := model.AsStorageError(err)
	if !ok || storageErr.Kind != model.KindUnsupportedOperation {
		t.Fatalf("error kind = %v", err)
	}
	// The refusal point is after pre_check and before any block request.
	if len(opener.requests) != 1 {
		t.Fatalf("control requests = %d, want only the pre_check", len(opener.requests))
	}
	if len(transport.requests) != 0 {
		t.Fatalf("signed requests = %d", len(transport.requests))
	}
}

func TestMultipartPartSizeAdaptsToUpstreamLimits(t *testing.T) {
	content := bytes.Repeat([]byte{0xAB}, 10240)
	md5At := func(offset int64, length int64) (string, string) {
		sum := md5.Sum(content[offset : offset+length])
		return hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:])
	}

	t.Run("minimum part size", func(t *testing.T) {
		control := []scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScriptWithLimit(`{"min_part_size":4096,"max_part_size":67108864,"max_parts":10000}`),
		}
		object := []scriptedResponse{}
		offsets := []int64{0, 4096, 8192}
		lengths := []int64{4096, 4096, 2048}
		for index := range offsets {
			_, md5Base64 := md5At(offsets[index], lengths[index])
			control = append(control, multipartPartScript(md5Base64, index+1))
			object = append(object, multipartPartEtagScript(fmt.Sprintf(`"etag-%d"`, index+1)))
		}
		control = append(control, multipartMergeScript(), scriptedResponse{status: 200, body: []byte(registerEntryPayload)})
		object = append(object, multipartMergedXMLScript(`"merged"`))
		client, opener, _, _ := newMultipartClient(t, func(c *Config) {
			c.MultipartThreshold = 2
			c.MultipartPartSize = 1024
		}, control, object)
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: bytes.NewReader(content)}); err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
		for index := range offsets {
			body := string(opener.bodies[2+index])
			if !strings.Contains(body, `"part_size":`+strconv.FormatInt(lengths[index], 10)) {
				t.Fatalf("part %d body = %s, want part_size %d", index+1, body, lengths[index])
			}
			if !strings.Contains(body, `"part_number":`+strconv.Itoa(index+1)) {
				t.Fatalf("part %d body = %s", index+1, body)
			}
		}
	})

	t.Run("max parts forces a larger part size", func(t *testing.T) {
		control := []scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScriptWithLimit(`{"min_part_size":1,"max_part_size":67108864,"max_parts":2}`),
		}
		for index, offset := range []int64{0, 5120} {
			_, md5Base64 := md5At(offset, 5120)
			control = append(control, multipartPartScript(md5Base64, index+1))
		}
		control = append(control, multipartMergeScript(), scriptedResponse{status: 200, body: []byte(registerEntryPayload)})
		object := []scriptedResponse{
			multipartPartEtagScript(`"etag-1"`), multipartPartEtagScript(`"etag-2"`),
			multipartMergedXMLScript(`"merged"`),
		}
		client, opener, _, _ := newMultipartClient(t, func(c *Config) {
			c.MultipartThreshold = 2
			c.MultipartPartSize = 1024
		}, control, object)
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: bytes.NewReader(content)}); err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
		for index := range []int{0, 1} {
			if !strings.Contains(string(opener.bodies[2+index]), `"part_size":5120`) {
				t.Fatalf("part %d body = %s, want part_size 5120", index+1, opener.bodies[2+index])
			}
		}
	})

	t.Run("file exceeds multipart size limits", func(t *testing.T) {
		client, opener, transport, _ := newMultipartClient(t, func(c *Config) {
			c.MultipartThreshold = 2
			c.MultipartPartSize = 1024
		}, []scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScriptWithLimit(`{"min_part_size":1,"max_part_size":4096,"max_parts":2}`),
		}, nil)
		_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: bytes.NewReader(content)})
		if err == nil || err.Error() != "WPS operation failed: file exceeds multipart size limits" {
			t.Fatalf("error = %v", err)
		}
		if len(transport.requests) != 0 {
			t.Fatalf("signed requests = %d", len(transport.requests))
		}
		if len(opener.requests) != 2 {
			t.Fatalf("control requests = %d, want pre_check and init only", len(opener.requests))
		}
	})

	t.Run("part above the memory ceiling is refused", func(t *testing.T) {
		client, _, _, _ := newMultipartClient(t, func(c *Config) {
			c.MultipartThreshold = 2
			c.MultipartPartSize = 100 << 20
		}, []scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScriptWithLimit(`{"min_part_size":1,"max_part_size":134217728,"max_parts":10000}`),
		}, nil)
		_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("bbbb")})
		if err == nil || err.Error() != "multipart part exceeds the memory safety limit" {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid limits", func(t *testing.T) {
		faces := []struct {
			name  string
			limit string
			want  string
		}{
			{"zero minimum", `{"min_part_size":0,"max_part_size":67108864,"max_parts":10000}`, "WPS operation failed: invalid multipart limits"},
			{"max below min", `{"min_part_size":4096,"max_part_size":1024,"max_parts":10000}`, "WPS operation failed: invalid multipart limits"},
			{"zero parts", `{"min_part_size":1,"max_part_size":67108864,"max_parts":0}`, "WPS operation failed: invalid multipart limits"},
			{"missing key", `{"min_part_size":1,"max_part_size":67108864}`, "WPS operation failed: parse multipart limits"},
		}
		for _, face := range faces {
			t.Run(face.name, func(t *testing.T) {
				client, _, transport, _ := newMultipartClient(t, func(c *Config) {
					c.MultipartThreshold = 2
					c.MultipartPartSize = 1024
				}, []scriptedResponse{
					{status: 200, body: []byte(`{"result":"ok"}`)},
					multipartInitScriptWithLimit(face.limit),
				}, nil)
				_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
				if err == nil || err.Error() != face.want {
					t.Fatalf("error = %v, want %q", err, face.want)
				}
				if len(transport.requests) != 0 {
					t.Fatalf("signed requests = %d", len(transport.requests))
				}
			})
		}
	})
}

func TestMultipartPartInstructionValidation(t *testing.T) {
	faces := []struct {
		name        string
		instruction string
		want        string
	}{
		{"bad result", `{"result":"nope"}`, "WPS operation failed: get multipart part URL"},
		{"wrong method", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"POST"}`, "WPS operation failed: invalid multipart part instruction"},
		{"url not a string", `{"result":"ok","url":5,"method":"PUT"}`, "WPS operation failed: invalid multipart part instruction"},
		{"body type", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT","request":{"body_type":"json"}}`, "WPS operation failed: invalid multipart part request instruction"},
		{"request missing", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT"}`, "WPS operation failed: invalid multipart part request instruction"},
		{"response not a map", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT","request":{"body_type":"file"},"response":"x"}`, "WPS operation failed: invalid multipart part response instruction"},
		{"other expect code", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT","request":{"body_type":"file"},"response":{"expect_code":[500]}}`, "WPS operation failed: unsupported multipart part status"},
		{"null expect code", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT","request":{"body_type":"file"},"response":{"expect_code":null}}`, "WPS operation failed: unsupported multipart part status"},
		{"empty expect code", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT","request":{"body_type":"file"},"response":{"expect_code":[]}}`, "WPS operation failed: unsupported multipart part status"},
		{"headers missing", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT","request":{"body_type":"file"},"response":{"expect_code":[200]}}`, "WPS operation failed: multipart part headers missing"},
		{"content type mismatch", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT","request":{"body_type":"file","headers":{"Content-MD5":"` + md5Part1Base64 + `","Content-Type":"text/plain"}},"response":{"expect_code":[200]}}`, "WPS operation failed: multipart part content type does not match instruction"},
	}
	for _, face := range faces {
		t.Run(face.name, func(t *testing.T) {
			client, _, transport, _ := newMultipartClient(t, func(c *Config) {
				smallMultipartConfig("")(c)
				c.UploadRetries = 0
			}, []scriptedResponse{
				{status: 200, body: []byte(`{"result":"ok"}`)},
				multipartInitScript("u1"),
				{status: 200, body: []byte(face.instruction)},
			}, nil)
			_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
			if err == nil || err.Error() != face.want {
				t.Fatalf("error = %v, want %q", err, face.want)
			}
			if len(transport.requests) != 0 {
				t.Fatalf("signed requests = %d, want none", len(transport.requests))
			}
		})
	}
	t.Run("instruction MD5 is forwarded without local comparison", func(t *testing.T) {
		control := []scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScript("u1"),
			{status: 200, body: []byte(`{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/p","method":"PUT","request":{"body_type":"file","headers":{"content-md5":"bGll","content-type":"application/octet-stream"}},"response":{"expect_code":[200]}}`)},
			multipartPartScript(md5Part2Base64, 2),
			multipartMergeScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		}
		object := []scriptedResponse{
			multipartPartEtagScript(`"etag-1"`), multipartPartEtagScript(`"etag-2"`),
			multipartMergedXMLScript(`"merged"`),
		}
		client, _, transport, _ := newMultipartClient(t, smallMultipartConfig(""), control, object)
		entry, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
		if err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
		if entry.ID != "9" || len(transport.requests) != 3 {
			t.Fatalf("entry = %+v, signed requests = %d", entry, len(transport.requests))
		}
	})
}

func TestMultipartPartEtagIsNormalizedAndMissingEtagFails(t *testing.T) {
	t.Run("quotes stripped", func(t *testing.T) {
		control, object := twoPartScripts()
		client, opener, _, _ := newMultipartClient(t, smallMultipartConfig(""), control, object)
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)}); err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
		if !strings.Contains(string(opener.bodies[4]), `"etag":"etag-1"`) {
			t.Fatalf("merge body keeps the raw quoted etag: %s", opener.bodies[4])
		}
	})
	t.Run("missing etag", func(t *testing.T) {
		client, _, transport, _ := newMultipartClient(t, func(c *Config) {
			smallMultipartConfig("")(c)
			c.UploadRetries = 0
		}, []scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScript("u1"),
			multipartPartScript(md5Part1Base64, 1),
		}, []scriptedResponse{{status: 200, body: []byte{}}})
		_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
		if err == nil || err.Error() != "WPS operation failed: multipart part response missing ETag" {
			t.Fatalf("error = %v", err)
		}
		if len(transport.requests) != 1 {
			t.Fatalf("signed requests = %d, want the single unconfirmed PUT", len(transport.requests))
		}
	})
}

func TestMultipartPartRetryRepeatsTheSamePart(t *testing.T) {
	control := []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
		multipartInitScript("u1"),
		multipartPartScript(md5Part1Base64, 1),
		multipartPartScript(md5Part2Base64, 2),
		multipartPartScript(md5Part2Base64, 2),
		multipartMergeScript(),
		{status: 200, body: []byte(registerEntryPayload)},
	}
	object := []scriptedResponse{
		multipartPartEtagScript(`"etag-1"`),
		{status: 500, body: []byte("boom")},
		multipartPartEtagScript(`"etag-2"`),
		multipartMergedXMLScript(`"merged"`),
	}
	client, opener, _, _ := newMultipartClient(t, func(c *Config) {
		smallMultipartConfig("")(c)
		c.UploadRetries = 1
	}, control, object)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if string(opener.bodies[3]) != string(opener.bodies[4]) {
		t.Fatalf("the retried part must re-send identical framing: %s vs %s", opener.bodies[3], opener.bodies[4])
	}
	if !strings.Contains(string(opener.bodies[5]), `"etag":"etag-1","part_number":1},{"etag":"etag-2","part_number":2}]`) {
		t.Fatalf("merge body = %s", opener.bodies[5])
	}
}

func TestMultipartPartFailureKeepsConfirmedPartsOnly(t *testing.T) {
	resumeDir := newResumeDir(t)
	client, opener, transport, _ := newMultipartClient(t, func(c *Config) {
		smallMultipartConfig(resumeDir)(c)
		c.UploadRetries = 0
	},
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScript("u1"),
			multipartPartScript(md5Part1Base64, 1),
			multipartPartScript(md5Part2Base64, 2),
		},
		[]scriptedResponse{
			multipartPartEtagScript(`"etag-1"`),
			{status: 500, body: []byte("boom")},
		})
	_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err == nil || err.Error() != "WPS operation failed: multipart part upload (HTTP 500)" {
		t.Fatalf("error = %v", err)
	}
	if len(opener.requests) != 4 {
		t.Fatalf("control requests = %d, want pre_check, init and two instructions", len(opener.requests))
	}
	if len(transport.requests) != 2 {
		t.Fatalf("signed requests = %d", len(transport.requests))
	}
	checkpoint := readCheckpointFile(t, resumeDir)
	var decoded map[string]any
	if err := json.Unmarshal(checkpoint, &decoded); err != nil {
		t.Fatal(err)
	}
	parts, _ := decoded["parts"].(map[string]any)
	if len(parts) != 1 || parts["1"] != "etag-1" {
		t.Fatalf("checkpoint parts = %v", decoded["parts"])
	}
}

func TestMultipartDisconnectIsRetriedWithoutSkippingThePart(t *testing.T) {
	sum := md5.Sum([]byte(multipartBodyFixture))
	instructionMD5 := base64.StdEncoding.EncodeToString(sum[:])
	control := []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
		multipartInitScript("u1"),
		multipartPartScript(instructionMD5, 1),
		multipartPartScript(instructionMD5, 1),
		multipartMergeScript(),
		{status: 200, body: []byte(registerEntryPayload)},
	}
	object := []scriptedResponse{
		multipartPartEtagScript(`"etag-1"`),
		multipartMergedXMLScript(`"merged"`),
	}
	client, opener, transport, _ := newMultipartClient(t, func(c *Config) {
		smallMultipartConfig("")(c)
		c.MultipartPartSize = 4
		c.UploadRetries = 1
	}, control, object)
	transport.failures = []error{errors.New("connection reset by peer")}
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if len(transport.requests) != 3 {
		t.Fatalf("signed requests = %d, want the failed PUT, the retried PUT and the merge", len(transport.requests))
	}
	if !strings.Contains(string(opener.bodies[4]), `"etag":"etag-1","part_number":1}]`) {
		t.Fatalf("merge body = %s", opener.bodies[4])
	}
}

func TestMultipartSessionResetRebuildsAndRestartsFromPartOne(t *testing.T) {
	resumeDir := newResumeDir(t)
	control := []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
		multipartInitScript("u1"),
		{status: 404, body: []byte(`{"error":"session gone"}`)},
		multipartInitScript("u2"),
		multipartPartScript(md5Part1Base64, 1),
		multipartPartScript(md5Part2Base64, 2),
		multipartMergeScript(),
		{status: 200, body: []byte(registerEntryPayload)},
	}
	object := []scriptedResponse{
		multipartPartEtagScript(`"etag-1"`),
		multipartPartEtagScript(`"etag-2"`),
		multipartMergedXMLScript(`"merged"`),
	}
	client, opener, _, _ := newMultipartClient(t, smallMultipartConfig(resumeDir), control, object)
	entry, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if entry.ID != "9" {
		t.Fatalf("entry = %+v", entry)
	}
	if string(opener.bodies[3]) != string(opener.bodies[1]) {
		t.Fatalf("reinit body %s differs from the original init %s", opener.bodies[3], opener.bodies[1])
	}
	if !strings.Contains(string(opener.bodies[2]), `"upload_id":"u1"`) {
		t.Fatalf("the failed part must reference the dead session: %s", opener.bodies[2])
	}
	if !strings.Contains(string(opener.bodies[4]), `"upload_id":"u2"`) ||
		!strings.Contains(string(opener.bodies[4]), `"part_number":1`) {
		t.Fatalf("the rebuild must restart from part one under the new session: %s", opener.bodies[4])
	}
	if !strings.Contains(string(opener.bodies[6]), `"upload_id":"u2"`) {
		t.Fatalf("merge must use the rebuilt session: %s", opener.bodies[6])
	}
	requireCheckpointGone(t, resumeDir, smallMultipartIdentity())
}

func TestMultipartSessionResetDropsPartInfosOfTheDeadSession(t *testing.T) {
	resumeDir := newResumeDir(t)
	control := []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
		multipartInitScript("u1"),
		multipartPartScript(md5Part1Base64, 1),
		// Part two finds the session gone, so the rebuild restarts the
		// whole part loop under the fresh session.
		{status: 404, body: []byte(`{"error":"session gone"}`)},
		multipartInitScript("u2"),
		multipartPartScript(md5Part1Base64, 1),
		multipartPartScript(md5Part2Base64, 2),
		multipartMergeScript(),
		{status: 200, body: []byte(registerEntryPayload)},
	}
	object := []scriptedResponse{
		multipartPartEtagScript(`"etag-1"`),
		multipartPartEtagScript(`"etag-1b"`),
		multipartPartEtagScript(`"etag-2"`),
		multipartMergedXMLScript(`"merged"`),
	}
	client, opener, _, _ := newMultipartClient(t, smallMultipartConfig(resumeDir), control, object)
	entry, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if entry.ID != "9" {
		t.Fatalf("entry = %+v", entry)
	}
	// The merge must carry only the rebuilt session's parts: the etag of
	// the dead upload_id would duplicate part number one.
	wantMerge := `{"key":"bench-multipart-key","req_by_internal":false,"store":"bench-store",` +
		`"part_infos":[{"etag":"etag-1b","part_number":1},{"etag":"etag-2","part_number":2}],` +
		`"upload_id":"u2","csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[7]) != wantMerge {
		t.Fatalf("merge body = %s, want %s", opener.bodies[7], wantMerge)
	}
	requireCheckpointGone(t, resumeDir, smallMultipartIdentity())
}

func TestMultipartSessionResetNeedsAResumeDir(t *testing.T) {
	client, opener, transport, _ := newMultipartClient(t, func(c *Config) {
		smallMultipartConfig("")(c)
		c.UploadRetries = 0
	}, []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
		multipartInitScript("u1"),
		{status: 404, body: []byte(`{"error":"session gone"}`)},
	}, nil)
	_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err == nil || err.Error() != "WPS operation failed: /3rd/drive/api/v5/files/upload/block (HTTP 404)" {
		t.Fatalf("error = %v", err)
	}
	if len(opener.requests) != 3 {
		t.Fatalf("control requests = %d, want pre_check, init and one instruction", len(opener.requests))
	}
	if len(transport.requests) != 0 {
		t.Fatalf("signed requests = %d", len(transport.requests))
	}
}

func TestMultipartSessionRebuildsAreBounded(t *testing.T) {
	resumeDir := newResumeDir(t)
	control := []scriptedResponse{{status: 200, body: []byte(`{"result":"ok"}`)}}
	for index := 0; index < 4; index++ {
		control = append(control,
			multipartInitScript(fmt.Sprintf("u%d", index+1)),
			scriptedResponse{status: 404, body: []byte(`{"error":"session gone"}`)})
	}
	client, opener, _, _ := newMultipartClient(t, smallMultipartConfig(resumeDir), control, nil)
	done := make(chan error, 1)
	go func() {
		_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "WPS operation failed: /3rd/drive/api/v5/files/upload/block (HTTP 404)" {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the rebuild loop must be bounded, the upload is still running")
	}
	inits := 0
	for _, request := range opener.requests {
		if request.Method == http.MethodPost && request.URL.Path == "/3rd/drive/api/v5/files/upload/block" {
			inits++
		}
	}
	if inits != 4 {
		t.Fatalf("init requests = %d, want the original plus three rebuilds", inits)
	}
}

func TestMultipartMergeAndRegisterFailuresKeepTheCheckpoint(t *testing.T) {
	faces := []struct {
		name    string
		control []scriptedResponse
		object  []scriptedResponse
		want    string
	}{
		{
			"merge result refused",
			[]scriptedResponse{
				{status: 200, body: []byte(`{"result":"ok"}`)},
				multipartInitScript("u1"),
				multipartPartScript(md5Part1Base64, 1),
				multipartPartScript(md5Part2Base64, 2),
				{status: 200, body: []byte(`{"result":"no"}`)},
			},
			[]scriptedResponse{multipartPartEtagScript(`"etag-1"`), multipartPartEtagScript(`"etag-2"`)},
			"WPS operation failed: prepare multipart merge",
		},
		{
			"merge object put fails",
			[]scriptedResponse{
				{status: 200, body: []byte(`{"result":"ok"}`)},
				multipartInitScript("u1"),
				multipartPartScript(md5Part1Base64, 1),
				multipartPartScript(md5Part2Base64, 2),
				multipartMergeScript(),
			},
			[]scriptedResponse{
				multipartPartEtagScript(`"etag-1"`), multipartPartEtagScript(`"etag-2"`),
				{status: 500, body: []byte("boom")},
			},
			"WPS operation failed: multipart merge (HTTP 500)",
		},
		{
			"register result refused",
			appendRegisterScript(`{"result":"bad"}`),
			twoPartScriptsObject(),
			"WPS operation failed: register multipart file",
		},
		{
			"register entry unparseable",
			appendRegisterScript(`{"result":"ok"}`),
			twoPartScriptsObject(),
			"WPS operation failed: normalize file metadata",
		},
	}
	for _, face := range faces {
		t.Run(face.name, func(t *testing.T) {
			resumeDir := newResumeDir(t)
			client, _, _, _ := newMultipartClient(t, smallMultipartConfig(resumeDir), face.control, face.object)
			_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
			if err == nil || err.Error() != face.want {
				t.Fatalf("error = %v, want %q", err, face.want)
			}
			checkpoint := readCheckpointFile(t, resumeDir)
			var decoded map[string]any
			if err := json.Unmarshal(checkpoint, &decoded); err != nil {
				t.Fatal(err)
			}
			parts, _ := decoded["parts"].(map[string]any)
			if len(parts) != 2 {
				t.Fatalf("checkpoint parts = %v, want both confirmed parts kept", decoded["parts"])
			}
		})
	}
}

func TestMultipartMergeInstructionValidation(t *testing.T) {
	faces := []struct {
		name  string
		merge string
		want  string
	}{
		{"bad result", `{"result":"no"}`, "WPS operation failed: prepare multipart merge"},
		{"wrong method", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"GET"}`, "WPS operation failed: invalid multipart merge instruction"},
		{"url missing", `{"result":"ok","method":"POST"}`, "WPS operation failed: invalid multipart merge instruction"},
		{"body type", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"POST","request":{"body_type":"json"}}`, "WPS operation failed: invalid multipart merge request instruction"},
		{"request missing", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"POST"}`, "WPS operation failed: invalid multipart merge request instruction"},
		{"body data not a string", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"POST","request":{"body_type":"data","body_data":5,"headers":{"Content-Type":"application/xml"}}}`, "WPS operation failed: multipart merge body is missing"},
		{"headers missing", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"POST","request":{"body_type":"data","body_data":"<x/>"}}`, "WPS operation failed: multipart merge body is missing"},
		{"content type", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"POST","request":{"body_type":"data","body_data":"<x/>","headers":{"Content-Type":"application/json"}}}`, "WPS operation failed: unsupported multipart merge content type"},
		{"response missing", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"POST","request":{"body_type":"data","body_data":"<x/>","headers":{"Content-Type":"application/xml"}}}`, "WPS operation failed: invalid multipart merge response instruction"},
		{"other expect code", `{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"POST","request":{"body_type":"data","body_data":"<x/>","headers":{"Content-Type":"application/xml"}},"response":{"expect_code":[500]}}`, "WPS operation failed: unsupported multipart merge status"},
	}
	for _, face := range faces {
		t.Run(face.name, func(t *testing.T) {
			client, _, transport, _ := newMultipartClient(t, smallMultipartConfig(""),
				[]scriptedResponse{
					{status: 200, body: []byte(`{"result":"ok"}`)},
					multipartInitScript("u1"),
					multipartPartScript(md5Part1Base64, 1),
					multipartPartScript(md5Part2Base64, 2),
					{status: 200, body: []byte(face.merge)},
				},
				[]scriptedResponse{multipartPartEtagScript(`"etag-1"`), multipartPartEtagScript(`"etag-2"`)})
			_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
			if err == nil || err.Error() != face.want {
				t.Fatalf("error = %v, want %q", err, face.want)
			}
			if len(transport.requests) != 2 {
				t.Fatalf("signed requests = %d, want only the two part PUTs", len(transport.requests))
			}
		})
	}
	t.Run("lowercase content type accepted", func(t *testing.T) {
		client, _, transport, _ := newMultipartClient(t, smallMultipartConfig(""),
			[]scriptedResponse{
				{status: 200, body: []byte(`{"result":"ok"}`)},
				multipartInitScript("u1"),
				multipartPartScript(md5Part1Base64, 1),
				multipartPartScript(md5Part2Base64, 2),
				{status: 200, body: []byte(`{"result":"ok","url":"https://hwc-bj.ag.kdocs.cn/m","method":"POST","request":{"body_type":"data","body_data":"<CompleteMultipartUpload/>","headers":{"content-type":"application/xml"}},"response":{"expect_code":[200]}}`)},
				{status: 200, body: []byte(registerEntryPayload)},
			},
			[]scriptedResponse{
				multipartPartEtagScript(`"etag-1"`), multipartPartEtagScript(`"etag-2"`),
				multipartMergedXMLScript(`"merged"`),
			})
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)}); err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
		if got := transport.requests[2].Header.Get("Content-Type"); got != "application/xml" {
			t.Fatalf("merge Content-Type = %q", got)
		}
	})
}

func TestMultipartEtagParsing(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain", `<r><ETag>"a"</ETag></r>`, "a"},
		{"namespaced", `<CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ETag>"ns"</ETag></CompleteMultipartUploadResult>`, "ns"},
		{"first wins", `<r><ETag>a</ETag><ETag>b</ETag></r>`, "a"},
		{"whitespace only text", `<r><ETag>  </ETag></r>`, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			etag, err := multipartEtag([]byte(testCase.body))
			if err != nil {
				t.Fatalf("multipartEtag failed: %v", err)
			}
			if etag != testCase.want {
				t.Fatalf("etag = %q, want %q", etag, testCase.want)
			}
		})
	}
	failures := []struct {
		name string
		body string
		want string
	}{
		{"doctype", `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><r><ETag>a</ETag></r>`, "WPS operation failed: parse multipart merge response"},
		{"entity", `<!ENTITY xxe "no">`, "WPS operation failed: parse multipart merge response"},
		{"no etag element", `<r><Other>a</Other></r>`, "WPS operation failed: multipart merge response missing ETag"},
		{"lowercase etag element", `<r><Etag>a</Etag></r>`, "WPS operation failed: multipart merge response missing ETag"},
		{"empty etag element", `<r><ETag></ETag></r>`, "WPS operation failed: multipart merge response missing ETag"},
		{"child only etag", `<r><ETag><x/></ETag></r>`, "WPS operation failed: multipart merge response missing ETag"},
		{"malformed xml", `<r><ETag>a</ETag>`, "WPS operation failed: parse multipart merge response"},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			_, err := multipartEtag([]byte(failure.body))
			if err == nil || err.Error() != failure.want {
				t.Fatalf("error = %v, want %q", err, failure.want)
			}
		})
	}
}

func TestMultipartCheckpointIsHashNamedAtomicAndCredentialFree(t *testing.T) {
	resumeDir := newResumeDir(t)
	client, _, _, _ := newMultipartClient(t, func(c *Config) {
		smallMultipartConfig(resumeDir)(c)
		c.UploadRetries = 0
	},
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScript("u1"),
			multipartPartScript(md5Part1Base64, 1),
			multipartPartScript(md5Part2Base64, 2),
		},
		[]scriptedResponse{
			multipartPartEtagScript(`"etag-1"`),
			{status: 500, body: []byte("boom")},
		})
	_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err == nil {
		t.Fatal("the upload must fail at part two")
	}
	wantPath := checkpointResumePath(resumeDir, smallMultipartIdentity())
	entries, err := os.ReadDir(resumeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(wantPath) {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("resume dir holds %v, want only %s", names, filepath.Base(wantPath))
	}
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint mode = %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("checkpoint is not JSON: %v", err)
	}
	wantKeys := []string{"identity", "key", "part_size", "parts", "store", "upload_id", "version"}
	if len(decoded) != len(wantKeys) {
		t.Fatalf("checkpoint keys = %v", decoded)
	}
	for _, key := range wantKeys {
		if _, present := decoded[key]; !present {
			t.Fatalf("checkpoint misses key %q: %v", key, decoded)
		}
	}
	text := string(raw)
	for _, forbidden := range []string{"csrf", "Cookie", "cookie", "secret", "https://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("checkpoint leaks %q: %s", forbidden, text)
		}
	}
	if decoded["version"] != float64(1) || decoded["identity"] != smallMultipartIdentity() {
		t.Fatalf("checkpoint identity/version = %v / %v", decoded["identity"], decoded["version"])
	}
	if decoded["part_size"] != float64(2) || decoded["upload_id"] != "u1" {
		t.Fatalf("checkpoint session fields = %v", decoded)
	}
}

func TestMultipartRefusesRelativeResumeDirAfterPreCheck(t *testing.T) {
	client, opener, transport, _ := newMultipartClient(t, func(c *Config) {
		c.MultipartThreshold = 2
		c.UploadResumeDir = "relative/dir"
	}, []scriptedResponse{{status: 200, body: []byte(`{"result":"ok"}`)}}, nil)
	_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err == nil || err.Error() != "upload_resume_dir must be absolute" {
		t.Fatalf("error = %v", err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("control requests = %d, want only the pre_check before the refusal", len(opener.requests))
	}
	if len(transport.requests) != 0 {
		t.Fatalf("signed requests = %d", len(transport.requests))
	}
}

func TestMultipartIgnoresForeignAndMalformedCheckpoints(t *testing.T) {
	identity := smallMultipartIdentity()
	faces := []struct {
		name       string
		checkpoint string
		mode       os.FileMode
	}{
		{"wrong identity", `{"version":1,"identity":"other","upload_id":"u0","key":"k","store":"s","part_size":2,"parts":{"1":"e"}}`, 0o600},
		{"wrong version", `{"version":2,"identity":"` + identity + `","upload_id":"u0","key":"k","store":"s","part_size":2,"parts":{"1":"e"}}`, 0o600},
		{"non digit part key", `{"version":1,"identity":"` + identity + `","upload_id":"u0","key":"k","store":"s","part_size":2,"parts":{"a":"e"}}`, 0o600},
		{"non string part value", `{"version":1,"identity":"` + identity + `","upload_id":"u0","key":"k","store":"s","part_size":2,"parts":{"1":5}}`, 0o600},
		{"garbage json", `not json`, 0o600},
		{"insecure permissions", `{"version":1,"identity":"` + identity + `","upload_id":"u0","key":"k","store":"s","part_size":2,"parts":{"1":"e"}}`, 0o644},
		{"empty session fields", `{"version":1,"identity":"` + identity + `","upload_id":"","key":"k","store":"s","part_size":2,"parts":{}}`, 0o600},
	}
	for _, face := range faces {
		t.Run(face.name, func(t *testing.T) {
			resumeDir := newResumeDir(t)
			if err := os.WriteFile(checkpointResumePath(resumeDir, identity), []byte(face.checkpoint), face.mode); err != nil {
				t.Fatal(err)
			}
			control, object := twoPartScripts()
			client, opener, _, _ := newMultipartClient(t, smallMultipartConfig(resumeDir), control, object)
			if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)}); err != nil {
				t.Fatalf("Upload failed: %v", err)
			}
			// A fresh session must have been negotiated: request two is the
			// block init, and the merge only holds parts of this session.
			if !strings.Contains(string(opener.bodies[1]), `"with_rapid"`) {
				t.Fatalf("no block init for a %s checkpoint", face.name)
			}
			if !strings.Contains(string(opener.bodies[4]), `"etag":"etag-1","part_number":1},{"etag":"etag-2","part_number":2}]`) {
				t.Fatalf("merge must only contain parts uploaded in this session: %s", opener.bodies[4])
			}
		})
	}
}

func TestMultipartBrokenPartSizeOnValidCheckpointIsFatal(t *testing.T) {
	resumeDir := newResumeDir(t)
	identity := smallMultipartIdentity()
	checkpoint := `{"version":1,"identity":"` + identity +
		`","upload_id":"u0","key":"k","store":"s","part_size":"garbage","parts":{}}`
	if err := os.WriteFile(checkpointResumePath(resumeDir, identity), []byte(checkpoint), 0o600); err != nil {
		t.Fatal(err)
	}
	client, opener, transport, _ := newMultipartClient(t, smallMultipartConfig(resumeDir),
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScript("u1"),
		}, nil)
	_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
	if err == nil || err.Error() != "WPS operation failed: invalid multipart resume checkpoint" {
		t.Fatalf("error = %v", err)
	}
	if len(opener.requests) != 1 {
		t.Fatalf("control requests = %d, want only the pre_check", len(opener.requests))
	}
	if len(transport.requests) != 0 {
		t.Fatalf("signed requests = %d", len(transport.requests))
	}
}

func TestMultipartResumesAfterRestart(t *testing.T) {
	resumeDir := newResumeDir(t)
	content := "abcdef"
	identity := contentMultipartIdentity(content)
	md5Ab := md5.Sum([]byte("ab"))
	md5Cd := md5.Sum([]byte("cd"))
	md5Ef := md5.Sum([]byte("ef"))

	// First client: parts one and two confirm, part three dies on the
	// signed PUT with no retries left.
	first, _, firstTransport, _ := newMultipartClient(t, func(c *Config) {
		c.MultipartThreshold = 3
		c.MultipartPartSize = 2
		c.UploadResumeDir = resumeDir
		c.UploadRetries = 0
	},
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartInitScript("u1"),
			multipartPartScript(base64.StdEncoding.EncodeToString(md5Ab[:]), 1),
			multipartPartScript(base64.StdEncoding.EncodeToString(md5Cd[:]), 2),
			multipartPartScript(base64.StdEncoding.EncodeToString(md5Ef[:]), 3),
		},
		[]scriptedResponse{
			multipartPartEtagScript(`"etag-1"`),
			multipartPartEtagScript(`"etag-2"`),
			{status: 500, body: []byte("boom")},
		})
	_, err := first.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(content)})
	if err == nil || err.Error() != "WPS operation failed: multipart part upload (HTTP 500)" {
		t.Fatalf("first upload error = %v", err)
	}
	if len(firstTransport.requests) != 3 {
		t.Fatalf("first signed requests = %d", len(firstTransport.requests))
	}

	// Second client, same identity: the checkpoint answers parts one and
	// two, only part three is uploaded, then merge and register reuse the
	// original session.
	second, opener, secondTransport, _ := newMultipartClient(t, func(c *Config) {
		c.MultipartThreshold = 3
		c.MultipartPartSize = 2
		c.UploadResumeDir = resumeDir
	},
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			multipartPartScript(base64.StdEncoding.EncodeToString(md5Ef[:]), 3),
			multipartMergeScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{
			multipartPartEtagScript(`"etag-3"`),
			multipartMergedXMLScript(`"merged"`),
		})
	entry, err := second.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(content)})
	if err != nil {
		t.Fatalf("resumed upload failed: %v", err)
	}
	if entry.ID != "9" {
		t.Fatalf("entry = %+v", entry)
	}
	if len(opener.requests) != 4 {
		t.Fatalf("resumed control requests = %d, want pre_check, one instruction, merge, register", len(opener.requests))
	}
	if !strings.Contains(string(opener.bodies[1]), `"part_number":3`) ||
		!strings.Contains(string(opener.bodies[1]), `"upload_id":"u1"`) {
		t.Fatalf("resumed part must reuse the checkpoint session: %s", opener.bodies[1])
	}
	wantMerge := `{"key":"bench-multipart-key","req_by_internal":false,"store":"bench-store",` +
		`"part_infos":[{"etag":"etag-1","part_number":1},{"etag":"etag-2","part_number":2},` +
		`{"etag":"etag-3","part_number":3}],"upload_id":"u1","csrfmiddlewaretoken":"csrf-secret"}`
	if string(opener.bodies[2]) != wantMerge {
		t.Fatalf("merge body = %s, want %s", opener.bodies[2], wantMerge)
	}
	if len(secondTransport.requests) != 2 {
		t.Fatalf("resumed signed requests = %d, want part three and the merge", len(secondTransport.requests))
	}
	requireCheckpointGone(t, resumeDir, identity)
}

// gatedSignedTransport parks the first signed request until released so a
// test can observe what a concurrent uploader does meanwhile.
type gatedSignedTransport struct {
	fake    *fakeSignedTransport
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedSignedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.fake.RoundTrip(request)
}

func multipartConcurrencyConfig(resumeDir string) Config {
	config := DefaultConfig("group-1")
	config.CredentialSource = staticSource()
	config.MultipartThreshold = 2
	config.MultipartPartSize = 2
	config.UploadResumeDir = resumeDir
	config.UploadSpoolDir = resumeDir
	config.SpoolLimiter = &recordingLimiter{}
	return config
}

// fullMultipartScript is the control script of one whole two-part upload.
func fullMultipartScript(uploadID string) []scriptedResponse {
	return []scriptedResponse{
		{status: 200, body: []byte(`{"result":"ok"}`)},
		multipartInitScript(uploadID),
		multipartPartScript(md5Part1Base64, 1),
		multipartPartScript(md5Part2Base64, 2),
		multipartMergeScript(),
		{status: 200, body: []byte(registerEntryPayload)},
	}
}

func TestMultipartConcurrentSameIdentityIsSerialized(t *testing.T) {
	resumeDir := newResumeDir(t)
	gated := &gatedSignedTransport{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		fake: &fakeSignedTransport{script: []scriptedResponse{
			multipartPartEtagScript(`"etag-1"`),
			multipartPartEtagScript(`"etag-2"`),
			multipartMergedXMLScript(`"merged"`),
		}},
	}
	first, err := NewClient(multipartConcurrencyConfig(resumeDir),
		WithOpener(&fakeControlOpener{script: fullMultipartScript("u1")}))
	if err != nil {
		t.Fatal(err)
	}
	first.signed.transport = gated
	var firstErr error
	firstDone := make(chan struct{})
	go func() {
		_, firstErr = first.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
		close(firstDone)
	}()
	<-gated.entered

	secondOpener := &fakeControlOpener{script: fullMultipartScript("u1")}
	// The poll loop reads the request count while the upload goroutine
	// runs, so the counter is mutex-guarded (the raw opener is not).
	secondCounter := &countingOpener{inner: secondOpener}
	second, err := NewClient(multipartConcurrencyConfig(resumeDir), WithOpener(secondCounter))
	if err != nil {
		t.Fatal(err)
	}
	second.signed.transport = &fakeSignedTransport{script: []scriptedResponse{
		multipartPartEtagScript(`"etag-1"`),
		multipartPartEtagScript(`"etag-2"`),
		multipartMergedXMLScript(`"merged"`),
	}}
	var secondErr error
	secondDone := make(chan struct{})
	go func() {
		_, secondErr = second.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(multipartBodyFixture)})
		close(secondDone)
	}()

	// While the first upload is parked inside its signed part PUT, the
	// second may only have passed the pre_check: the checkpoint lock holds
	// it before the block init.
	deadline := time.Now().Add(5 * time.Second)
	for secondCounter.total() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stableUntil := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(stableUntil) {
		if secondCounter.total() > 1 {
			t.Fatalf("the second upload issued %d requests while the first held the checkpoint", secondCounter.total())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gated.release)
	select {
	case <-firstDone:
		if firstErr != nil {
			t.Fatalf("first upload failed: %v", firstErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first upload never finished")
	}
	select {
	case <-secondDone:
		if secondErr != nil {
			t.Fatalf("second upload failed: %v", secondErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second upload never finished")
	}
	if secondCounter.total() < 4 {
		t.Fatalf("the second upload issued %d requests after release", secondCounter.total())
	}
}

func TestMultipartUploadDownloadHashesMatchAt100MiB(t *testing.T) {
	const total = 100 << 20
	const partSize = 10 << 20
	content := make([]byte, total)
	for index := range content {
		content[index] = byte(index % 251)
	}
	resumeDir := newResumeDir(t)
	sourceSum := sha256.Sum256(content)

	control := []scriptedResponse{{status: 200, body: []byte(`{"result":"ok"}`)}, multipartInitScript("u1")}
	object := []scriptedResponse{}
	for part := 0; part < total/partSize; part++ {
		sum := md5.Sum(content[part*partSize : (part+1)*partSize])
		control = append(control, multipartPartScript(base64.StdEncoding.EncodeToString(sum[:]), part+1))
		object = append(object, multipartPartEtagScript(fmt.Sprintf(`"etag-%d"`, part+1)))
	}
	control = append(control, multipartMergeScript(),
		scriptedResponse{status: 200, body: []byte(registerEntryPayload)},
		resolveResponse("https://hwc-bj.ag.kdocs.cn/dl-100?sig=secret"))
	object = append(object, multipartMergedXMLScript(`"merged-100"`),
		scriptedResponse{status: 200, body: content})

	client, opener, transport, _ := newMultipartClient(t, func(c *Config) {
		c.UploadResumeDir = resumeDir
	}, control, object)
	entry, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: bytes.NewReader(content)})
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	// Ten part PUTs plus the signed merge; the download GET comes later.
	if len(transport.requests) != 11 {
		t.Fatalf("signed requests = %d, want ten parts and the merge", len(transport.requests))
	}
	for part := 0; part < 10; part++ {
		if transport.requests[part].ContentLength != partSize {
			t.Fatalf("part %d Content-Length = %d, want %d", part+1, transport.requests[part].ContentLength, partSize)
		}
	}
	if !strings.Contains(string(opener.bodies[12]), `"part_number":10}]`) {
		t.Fatalf("merge body = %s", opener.bodies[12])
	}

	// The part bodies reassemble to exactly the source bytes.
	reassembled := sha256.New()
	for part := 0; part < 10; part++ {
		body, err := io.ReadAll(transport.requests[part].Body)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(body)) != partSize {
			t.Fatalf("part %d body = %d bytes", part+1, len(body))
		}
		reassembled.Write(body)
	}
	if !bytes.Equal(reassembled.Sum(nil), sourceSum[:]) {
		t.Fatal("reassembled part bodies do not match the source hash")
	}

	downloaded, err := client.OpenDownload(entry.ID, 0, nil, nil)
	if err != nil {
		t.Fatalf("OpenDownload failed: %v", err)
	}
	defer downloaded.Close()
	downloadSum := sha256.New()
	if _, err := io.Copy(downloadSum, downloaded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloadSum.Sum(nil), sourceSum[:]) {
		t.Fatal("the download does not return the uploaded bytes")
	}
	if len(transport.requests) != 12 {
		t.Fatalf("signed requests after download = %d, want the merge PUTs plus the GET", len(transport.requests))
	}
	requireCheckpointGone(t, resumeDir, contentMultipartIdentity(string(content)))
}

// countingOpener wraps an opener with a mutex-guarded request counter so a
// poll loop can read it while the uploading goroutine is running.
type countingOpener struct {
	inner Opener
	mu    sync.Mutex
	count int
}

func (c *countingOpener) Do(request *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	return c.inner.Do(request)
}

func (c *countingOpener) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}
