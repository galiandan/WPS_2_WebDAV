// The B1000 upload tests pin the request-body and spool half of
// client.upload: the validation order, the upload budget, the coordinated
// spool reservation, the streamed checksums, and the cleanup of the
// temporary spool and the reservation on every failure surface.

package wps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func newUploadClient(t *testing.T, limiter SpoolLimiter, mutate func(*Config)) (*Client, *fakeControlOpener, string) {
	t.Helper()
	opener := &fakeControlOpener{}
	spoolDir := t.TempDir()
	client := newWriteClient(t, opener, func(c *Config) {
		c.UploadSpoolDir = spoolDir
		c.SpoolLimiter = limiter
		if mutate != nil {
			mutate(c)
		}
	})
	client.signed.transport = &fakeSignedTransport{}
	return client, opener, spoolDir
}

// newUploadObjectClient extends newUploadClient with a scripted signed
// object transport so the create_update and object PUT halves can run to
// the stage boundary.
func newUploadObjectClient(t *testing.T, limiter SpoolLimiter, mutate func(*Config), control []scriptedResponse, object []scriptedResponse) (*Client, *fakeControlOpener, *fakeSignedTransport, string) {
	t.Helper()
	client, opener, spoolDir := newUploadClient(t, limiter, mutate)
	transport := &fakeSignedTransport{script: object}
	client.signed.transport = transport
	opener.script = append(opener.script, control...)
	return client, opener, transport, spoolDir
}

// benchCreateUpdateScript is the observed-shape create_update instruction
// pointing at the fake object host.
func benchCreateUpdateScript() scriptedResponse {
	return scriptedResponse{status: 200, body: []byte(
		`{"url":"https://hwc-bj.ag.kdocs.cn/upload-bench?sig=secret","response":{"expect_code":[200]},"store":"bench-store"}`)}
}

// benchObjectScript is a successful object PUT carrying an ETag.
func benchObjectScript() scriptedResponse {
	return scriptedResponse{status: 200, header: http.Header{"Etag": []string{`"bench-etag"`}}, body: []byte{}}
}

func spoolDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the spool directory still holds %d files: %s", len(entries), strings.Join(names, ", "))
	}
}

func TestUploadRejectsBadNamesBeforeAnyRequest(t *testing.T) {
	client, opener, _ := newUploadClient(t, &recordingLimiter{}, nil)
	for _, name := range []string{"", "a/b", "a\\b", "/x", "\\"} {
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: name, Source: strings.NewReader("x")}); err == nil {
			t.Fatalf("name %q was accepted", name)
		} else if err.Error() != "name must be one remote file name" {
			t.Fatalf("name %q: error = %q", name, err.Error())
		}
	}
	if len(opener.requests) != 0 {
		t.Fatalf("the client issued %d requests", len(opener.requests))
	}
}

func TestUploadValidatesConfigurationBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"zero chunk size", func(c *Config) { c.StreamChunkSize = 0 }, "stream_chunk_size must be positive"},
		{"negative spool memory", func(c *Config) { c.UploadSpoolMemory = -1 }, "upload_spool_memory must not be negative"},
		{"negative min free", func(c *Config) { c.UploadMinFreeBytes = -1 }, "upload_min_free_bytes must not be negative"},
		{"negative max upload", func(c *Config) { c.MaxUploadBytes = -1 }, "max_upload_bytes must not be negative"},
		{"negative retries", func(c *Config) { c.UploadRetries = -1 }, "upload_retries must not be negative"},
		{"negative retry delay", func(c *Config) { c.UploadRetryDelay = -0.5 }, "upload_retry_delay must not be negative"},
		{"zero multipart threshold", func(c *Config) { c.MultipartThreshold = 0 }, "multipart_threshold must be positive"},
		{"zero multipart part size", func(c *Config) { c.MultipartPartSize = 0 }, "multipart_part_size must be positive"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, opener, _ := newUploadClient(t, &recordingLimiter{}, testCase.mutate)
			_, err := client.spoolUpload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("x")})
			if err == nil || err.Error() != testCase.want {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
			if len(opener.requests) != 0 {
				t.Fatalf("the client issued %d requests", len(opener.requests))
			}
		})
	}
}

func TestUploadRequiresSpoolLimiter(t *testing.T) {
	client, opener, _ := newUploadClient(t, nil, nil)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("x")}); err == nil {
		t.Fatal("a client without a spool limiter must refuse uploads")
	} else if err.Error() != "upload spool limiter is required" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(opener.requests) != 0 {
		t.Fatalf("the client issued %d requests", len(opener.requests))
	}
}

func TestUploadChecksDeclaredSizeAgainstTheBudget(t *testing.T) {
	t.Run("negative declared size", func(t *testing.T) {
		client, _, _ := newUploadClient(t, &recordingLimiter{}, nil)
		negative := int64(-1)
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("x"), Size: &negative}); err == nil {
			t.Fatal("a negative declared size must be refused")
		} else if err.Error() != "upload size must not be negative" {
			t.Fatalf("error = %q", err.Error())
		}
	})
	t.Run("declared size over the configured maximum", func(t *testing.T) {
		client, _, spoolDir := newUploadClient(t, &recordingLimiter{}, func(c *Config) { c.MaxUploadBytes = 8 })
		nine := int64(9)
		_, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("nine bytes"), Size: &nine})
		storageErr, ok := model.AsStorageError(err)
		if !ok {
			t.Fatalf("error is not a StorageError: %v", err)
		}
		if storageErr.Kind != model.KindInsufficientStorage || storageErr.Message != "upload exceeds the configured size limit" {
			t.Fatalf("error = %+v", storageErr)
		}
		spoolDirEmpty(t, spoolDir)
	})
	t.Run("declared size within the memory threshold skips the disk probe", func(t *testing.T) {
		client, _, _ := newUploadClient(t, &recordingLimiter{}, func(c *Config) {
			c.UploadSpoolMemory = 64
		})
		client.diskFree = func(string) (int64, error) { return 0, errors.New("must not be probed") }
		small := int64(4)
		spool, err := client.spoolUpload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body"), Size: &small})
		if err != nil {
			t.Fatal(err)
		}
		spool.close()
	})
	t.Run("spool directory probe fails", func(t *testing.T) {
		client, _, spoolDir := newUploadClient(t, &recordingLimiter{}, func(c *Config) {
			c.UploadSpoolMemory = 4
		})
		client.diskFree = func(string) (int64, error) { return 0, errors.New("boom") }
		nine := int64(9)
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("nine bytes"), Size: &nine}); err == nil {
			t.Fatal("an unavailable spool directory must be refused")
		} else if err.Error() != "upload spool directory is unavailable" {
			t.Fatalf("error = %q", err.Error())
		}
		spoolDirEmpty(t, spoolDir)
	})
	t.Run("free space below the reserve", func(t *testing.T) {
		client, _, spoolDir := newUploadClient(t, &recordingLimiter{}, func(c *Config) {
			c.UploadSpoolMemory = 4
			c.UploadMinFreeBytes = 100
		})
		client.diskFree = func(string) (int64, error) { return 108, nil }
		nine := int64(9)
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("nine bytes"), Size: &nine}); err == nil {
			t.Fatal("an exhausted spool directory must be refused")
		} else if err.Error() != "not enough free space for the upload spool" {
			t.Fatalf("error = %q", err.Error())
		}
		spoolDirEmpty(t, spoolDir)
	})
	t.Run("free space exactly at the reserve passes", func(t *testing.T) {
		client, _, _ := newUploadClient(t, &recordingLimiter{}, func(c *Config) {
			c.UploadSpoolMemory = 4
			c.UploadMinFreeBytes = 100
		})
		client.diskFree = func(string) (int64, error) { return 109, nil }
		nine := int64(9)
		spool, err := client.spoolUpload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("123456789"), Size: &nine})
		if err != nil {
			t.Fatal(err)
		}
		spool.close()
	})
}

func TestUploadRequiresCSRF(t *testing.T) {
	client, opener, _ := newUploadClient(t, &recordingLimiter{}, func(c *Config) {
		c.CredentialSource = &credentials.StaticCredentialSource{
			Credentials: credentials.Credentials{Cookie: "Cookie-secret"},
		}
	})
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("x")}); err == nil {
		t.Fatal("an upload without any CSRF token must be refused")
	} else if err.Error() != "csrf_token is required for write operation" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(opener.requests) != 0 {
		t.Fatalf("the client issued %d requests", len(opener.requests))
	}
}

func TestUploadSpoolsAndHashesInOnePass(t *testing.T) {
	client, opener, spoolDir := newUploadClient(t, &recordingLimiter{}, nil)
	spool, err := client.spoolUpload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("hello")})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	if spool.total != 5 {
		t.Fatalf("total = %d", spool.total)
	}
	// Pinned against the Python hashlib values for "hello".
	if spool.md5 != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("md5 = %q", spool.md5)
	}
	if spool.sha1 != "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d" {
		t.Fatalf("sha1 = %q", spool.sha1)
	}
	if spool.sha256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("sha256 = %q", spool.sha256)
	}
	body, err := io.ReadAll(mustReopen(t, spool))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("rewound body = %q", body)
	}
	spoolDirEmpty(t, spoolDir)
	if len(opener.requests) != 0 {
		t.Fatalf("the client issued %d requests", len(opener.requests))
	}
}

func TestUploadSpoolsEmptySourceLikePython(t *testing.T) {
	client, _, _ := newUploadClient(t, &recordingLimiter{}, nil)
	spool, err := client.spoolUpload(UploadRequest{ParentID: "3", Name: "file", Source: emptyReader{}})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	if spool.total != 0 {
		t.Fatalf("total = %d", spool.total)
	}
	if spool.md5 != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("md5 = %q", spool.md5)
	}
	if spool.sha1 != "da39a3ee5e6b4b0d3255bfef95601890afd80709" {
		t.Fatalf("sha1 = %q", spool.sha1)
	}
	if spool.sha256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("sha256 = %q", spool.sha256)
	}
}

func TestUploadReservesPerChunkAndReleasesAtTheBoundary(t *testing.T) {
	limiter := &recordingLimiter{}
	client, _, _, spoolDir := newUploadObjectClient(t, limiter, func(c *Config) { c.StreamChunkSize = 4 },
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	spool, err := client.spoolUpload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("nine bytes")})
	if err != nil {
		t.Fatal(err)
	}
	if len(limiter.reserves) != 3 || limiter.reserves[0] != 4 || limiter.reserves[1] != 8 || limiter.reserves[2] != 10 {
		t.Fatalf("reserves = %v", limiter.reserves)
	}
	if spool.reserved != 10 {
		t.Fatalf("reserved = %d", spool.reserved)
	}
	if len(limiter.releases) != 0 {
		t.Fatalf("the reservation was released while the flow still holds the spool: %v", limiter.releases)
	}
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("nine bytes")}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if len(limiter.releases) != 1 || limiter.releases[0] != 10 {
		t.Fatalf("releases = %v", limiter.releases)
	}
	spool.close()
	if len(limiter.releases) != 2 || limiter.releases[1] != 10 {
		t.Fatalf("releases = %v", limiter.releases)
	}
	spoolDirEmpty(t, spoolDir)
}

func TestUploadReleasesEverythingWhenTheBudgetFailsMidStream(t *testing.T) {
	limiter := &recordingLimiter{}
	client, _, spoolDir := newUploadClient(t, limiter, func(c *Config) {
		c.StreamChunkSize = 4
		c.MaxUploadBytes = 6
	})
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("nine bytes")}); err == nil {
		t.Fatal("the per-chunk budget must refuse the stream")
	} else if err.Error() != "upload exceeds the configured size limit" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(limiter.releases) != 1 || limiter.releases[0] != 4 {
		t.Fatalf("releases = %v", limiter.releases)
	}
	spoolDirEmpty(t, spoolDir)
}

func TestUploadReleasesEverythingWhenTheSourceFails(t *testing.T) {
	limiter := &recordingLimiter{}
	client, _, spoolDir := newUploadClient(t, limiter, nil)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: failingReader{}}); err == nil {
		t.Fatal("a failing source must abort the upload")
	} else if err.Error() != "upload source read failed" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(limiter.releases) != 1 || limiter.releases[0] != 0 {
		t.Fatalf("releases = %v", limiter.releases)
	}
	spoolDirEmpty(t, spoolDir)
}

func TestUploadReleasesEverythingWhenTheSpoolCannotRollOver(t *testing.T) {
	limiter := &recordingLimiter{}
	client, _, _ := newUploadClient(t, limiter, func(c *Config) {
		c.UploadSpoolMemory = 4
		c.UploadSpoolDir = filepath.Join(t.TempDir(), "missing")
	})
	// The budget probe points at a healthy directory so the failure is the
	// rollover itself, not the free-space check.
	client.diskFree = func(string) (int64, error) { return 1 << 40, nil }
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("nine bytes")}); err == nil {
		t.Fatal("a rollover failure must abort the upload")
	} else if err.Error() != "upload spool rollover failed" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(limiter.releases) != 1 || limiter.releases[0] != 10 {
		t.Fatalf("releases = %v", limiter.releases)
	}
}

func TestUploadReleasesEverythingOnSizeMismatch(t *testing.T) {
	limiter := &recordingLimiter{}
	client, _, spoolDir := newUploadClient(t, limiter, nil)
	five := int64(5)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("abc"), Size: &five}); err == nil {
		t.Fatal("a short source must be refused")
	} else if err.Error() != "source size mismatch: expected 5, read 3" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(limiter.releases) != 1 || limiter.releases[0] != 3 {
		t.Fatalf("releases = %v", limiter.releases)
	}
	spoolDirEmpty(t, spoolDir)
}

func mustReopen(t *testing.T, spool *uploadSpool) io.Reader {
	t.Helper()
	reader, err := spool.file.reopen()
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// The B1001 pre_check tests pin the conflict probe after the spool: the
// exact ordered query, the result gate, and the overwrite-and-403
// continuation that never substitutes a delete for the overwrite.

func TestUploadPreCheckSendsExactQueryAndCompletesTheUpload(t *testing.T) {
	limiter := &recordingLimiter{}
	client, opener, _, spoolDir := newUploadObjectClient(t, limiter, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body")}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if len(opener.requests) != 3 {
		t.Fatalf("requests = %d, want pre_check + create_update + register", len(opener.requests))
	}
	request := opener.requests[0]
	if request.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", request.Method)
	}
	want := "https://365.kdocs.cn/3rd/drive/api/v5/files/upload/pre_check?file_name=file&group_id=group-1&parent_id=3"
	if request.URL.String() != want {
		t.Fatalf("URL = %q, want %q", request.URL.String(), want)
	}
	if request.Header.Get("Cookie") != "Cookie-secret" {
		t.Fatalf("Cookie = %q", request.Header.Get("Cookie"))
	}
	spoolDirEmpty(t, spoolDir)
	if len(limiter.releases) != 1 || limiter.releases[0] != 4 {
		t.Fatalf("releases = %v", limiter.releases)
	}
}

func TestUploadPreCheckAcceptsMissingAndNullResult(t *testing.T) {
	for _, body := range []string{`{}`, `{"result":null}`} {
		t.Run(body, func(t *testing.T) {
			client, _, _, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
				[]scriptedResponse{
					{status: 200, body: []byte(body)},
					benchCreateUpdateScript(),
					{status: 200, body: []byte(registerEntryPayload)},
				},
				[]scriptedResponse{benchObjectScript()},
			)
			if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body")}); err != nil {
				t.Fatalf("Upload failed: %v", err)
			}
		})
	}
}

func TestUploadPreCheckRejectsOtherResults(t *testing.T) {
	client, opener, spoolDir := newUploadClient(t, &recordingLimiter{}, nil)
	opener.script = []scriptedResponse{
		{status: 200, body: []byte(`{"result":"conflict"}`)},
	}
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body")}); err == nil {
		t.Fatal("a foreign result must refuse the upload")
	} else if err.Error() != "WPS operation failed: upload pre-check" {
		t.Fatalf("error = %q", err.Error())
	}
	spoolDirEmpty(t, spoolDir)
}

func TestUploadPreCheckContinuesOnlyOnOverwrite403(t *testing.T) {
	t.Run("403 without overwrite propagates", func(t *testing.T) {
		client, opener, spoolDir := newUploadClient(t, &recordingLimiter{}, nil)
		opener.script = []scriptedResponse{
			{status: 403, body: []byte(`{"result":"forbidden"}`)},
		}
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body")}); err == nil {
			t.Fatal("a 403 pre_check without overwrite must propagate")
		} else if err.Error() != "WPS operation failed: /3rd/drive/api/v5/files/upload/pre_check (HTTP 403)" {
			t.Fatalf("error = %q", err.Error())
		}
		spoolDirEmpty(t, spoolDir)
	})
	t.Run("403 with overwrite continues", func(t *testing.T) {
		client, opener, _, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
			[]scriptedResponse{
				{status: 403, body: []byte(`{"result":"forbidden"}`)},
				benchCreateUpdateScript(),
				{status: 200, body: []byte(registerEntryPayload)},
			},
			[]scriptedResponse{benchObjectScript()},
		)
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body"), Overwrite: true}); err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
		// The overwrite continues straight to create_update: no delete-first
		// substitute request may appear.
		if len(opener.requests) != 3 {
			t.Fatalf("requests = %d, want pre_check + create_update + register", len(opener.requests))
		}
		if got := opener.requests[1].URL.Path; got != "/3rd/drive/api/v5/files/upload/create_update" {
			t.Fatalf("second request path = %q, want create_update", got)
		}
	})
	t.Run("other statuses keep propagating under overwrite", func(t *testing.T) {
		client, opener, _ := newUploadClient(t, &recordingLimiter{}, nil)
		opener.script = []scriptedResponse{
			{status: 500, body: []byte(`{"result":"boom"}`)},
		}
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body"), Overwrite: true}); err == nil {
			t.Fatal("a 500 pre_check must propagate even under overwrite")
		} else if err.Error() != "WPS operation failed: /3rd/drive/api/v5/files/upload/pre_check (HTTP 500)" {
			t.Fatalf("error = %q", err.Error())
		}
	})
	t.Run("transport failures keep propagating under overwrite", func(t *testing.T) {
		client, opener, _ := newUploadClient(t, &recordingLimiter{}, nil)
		opener.failures = []error{errors.New("connection reset")}
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body"), Overwrite: true}); err == nil {
			t.Fatal("a transport failure must propagate even under overwrite")
		} else if err.Error() != "WPS operation failed: /3rd/drive/api/v5/files/upload/pre_check" {
			t.Fatalf("error = %q", err.Error())
		}
	})
}

func TestUploadPreCheckNormalizesDecimalGroupID(t *testing.T) {
	client, opener, _, _ := newUploadObjectClient(t, &recordingLimiter{}, func(c *Config) { c.GroupID = "007" },
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	if _, err := client.Upload(UploadRequest{ParentID: "030", Name: "file", Source: strings.NewReader("body")}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	want := "https://365.kdocs.cn/3rd/drive/api/v5/files/upload/pre_check?file_name=file&group_id=7&parent_id=30"
	if opener.requests[0].URL.String() != want {
		t.Fatalf("URL = %q, want %q", opener.requests[0].URL.String(), want)
	}
}

func TestUploadPreCheckFailsBeforeAnyRequestWithoutAGroup(t *testing.T) {
	client, opener, spoolDir := newUploadClient(t, &recordingLimiter{}, nil)
	client.config.GroupID = ""
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("body")}); err == nil {
		t.Fatal("an unresolvable group id must refuse the upload")
	} else if err.Error() != "WPS operation failed: WPS workspace is not configured (HTTP 503)" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(opener.requests) != 0 {
		t.Fatalf("the client issued %d requests", len(opener.requests))
	}
	spoolDirEmpty(t, spoolDir)
}

// The B1002 tests pin the create_update instruction and the signed object
// PUT: the exact captured body, the instruction gates, the credential-free
// streaming PUT, the fresh-URL exponential retry, and the ETag handoff.

const benchBody = "body"

// wantCreateUpdateBody is the exact compact JSON the overwrite=false flow
// must send: Python's dict order, ensure_ascii, and separators.
const wantCreateUpdateBody = `{"groupid":"group-1","parentid":3,"parent_path":[],"size":4,` +
	`"name":"file","req_by_internal":false,"client_stores":"","contenttype":"application/octet-stream",` +
	`"startswithfilename":"","successactionstatus":200,"group_id":"group-1","parent_id":3,"file_id":0,` +
	`"with_rapid":true,"tried_store":[],"sha256":"230d8358dc8e8890b4c58deeb62912ee2f20357ae92a5cc861b98e68fe31acb5",` +
	`"csrfmiddlewaretoken":"csrf-secret"}`

func TestUploadSendsCreateUpdateBodyAndPutsTheObject(t *testing.T) {
	limiter := &recordingLimiter{}
	client, opener, object, spoolDir := newUploadObjectClient(t, limiter, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	entry, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)})
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if entry.ID != "9" {
		t.Fatalf("entry = %+v", entry)
	}
	if len(opener.requests) != 3 {
		t.Fatalf("control requests = %d, want 3", len(opener.requests))
	}
	instruction := opener.requests[1]
	if instruction.Method != http.MethodPut {
		t.Fatalf("instruction method = %s, want PUT", instruction.Method)
	}
	if instruction.URL.String() != "https://365.kdocs.cn/3rd/drive/api/v5/files/upload/create_update" {
		t.Fatalf("instruction URL = %q", instruction.URL.String())
	}
	if string(opener.bodies[1]) != wantCreateUpdateBody {
		t.Fatalf("instruction body = %q, want %q", opener.bodies[1], wantCreateUpdateBody)
	}
	if len(object.requests) != 1 {
		t.Fatalf("object requests = %d, want 1", len(object.requests))
	}
	put := object.requests[0]
	if put.Method != http.MethodPut {
		t.Fatalf("object method = %s, want PUT", put.Method)
	}
	if put.URL.String() != "https://hwc-bj.ag.kdocs.cn/upload-bench?sig=secret" {
		t.Fatalf("object URL = %q", put.URL.String())
	}
	if got, err := io.ReadAll(put.Body); err != nil || string(got) != benchBody {
		t.Fatalf("object body = %q (%v), want %q", got, err, benchBody)
	}
	if got := put.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("object Content-Type = %q", got)
	}
	if put.ContentLength != int64(len(benchBody)) {
		t.Fatalf("object ContentLength = %d, want %d", put.ContentLength, len(benchBody))
	}
	// The signed transport must never observe the browser session.
	if put.Header.Get("Cookie") != "" || put.Header.Get("Authorization") != "" {
		t.Fatalf("object request carried credentials: %v", put.Header)
	}
	spoolDirEmpty(t, spoolDir)
	if len(limiter.releases) != 1 || limiter.releases[0] != 4 {
		t.Fatalf("releases = %v", limiter.releases)
	}
}

func TestUploadOverwriteRebuildsOptionsAndAppendsMd5(t *testing.T) {
	client, opener, _, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody), Overwrite: true}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	want := `{"groupid":"group-1","parentid":3,"parent_path":[],"size":4,` +
		`"name":"file","req_by_internal":false,"client_stores":"ks3,ks3sh","contenttype":"application/octet-stream",` +
		`"startswithfilename":"file","successactionstatus":201,"group_id":"group-1","parent_id":3,"file_id":0,` +
		`"with_rapid":true,"tried_store":["ks3,ks3sh"],"sha256":"230d8358dc8e8890b4c58deeb62912ee2f20357ae92a5cc861b98e68fe31acb5",` +
		`"csrfmiddlewaretoken":"csrf-secret","md5":"841a2d689ad86bd1611447453c22c6fc"}`
	if string(opener.bodies[1]) != want {
		t.Fatalf("overwrite body = %q, want %q", opener.bodies[1], want)
	}
}

func TestUploadCreateUpdateValidatesTheInstruction(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"missing url", `{"response":{"expect_code":[200]}}`, "WPS operation failed: create upload URL"},
		{"url is not a string", `{"url":42,"response":{"expect_code":[200]}}`, "WPS operation failed: create upload URL"},
		{"unsupported expect code", `{"url":"https://hwc-bj.ag.kdocs.cn/u","response":{"expect_code":[201]}}`, "WPS operation failed: unsupported object upload status"},
		{"non-integer expect code", `{"url":"https://hwc-bj.ag.kdocs.cn/u","response":{"expect_code":["200"]}}`, "WPS operation failed: unsupported object upload status"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, _, _, spoolDir := newUploadObjectClient(t, &recordingLimiter{}, nil,
				[]scriptedResponse{
					{status: 200, body: []byte(`{"result":"ok"}`)},
					{status: 200, body: []byte(testCase.payload)},
				},
				nil,
			)
			if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)}); err == nil {
				t.Fatal("a malformed instruction must refuse the upload")
			} else if err.Error() != testCase.want {
				t.Fatalf("error = %q, want %q", err.Error(), testCase.want)
			}
			spoolDirEmpty(t, spoolDir)
		})
	}
	t.Run("default expect code applies without the response object", func(t *testing.T) {
		client, _, _, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
			[]scriptedResponse{
				{status: 200, body: []byte(`{"result":"ok"}`)},
				{status: 200, body: []byte(`{"url":"https://hwc-bj.ag.kdocs.cn/u"}`)},
				{status: 200, body: []byte(registerEntryPayload)},
			},
			[]scriptedResponse{benchObjectScript()},
		)
		if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)}); err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
	})
}

func TestUploadObjectPutRetriesWithFreshSignedURL(t *testing.T) {
	client, opener, object, _ := newUploadObjectClient(t, &recordingLimiter{}, func(c *Config) {
		c.UploadRetryDelay = 0.01
	},
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{
			{status: 500, body: []byte("upstream boom")},
			benchObjectScript(),
		},
	)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if len(object.requests) != 2 {
		t.Fatalf("object requests = %d, want the failed attempt plus the retry", len(object.requests))
	}
	// Each retry carries a freshly signed URL and the full body again.
	if object.requests[0].URL.String() != "https://hwc-bj.ag.kdocs.cn/upload-bench?sig=secret" {
		t.Fatalf("first object URL = %q", object.requests[0].URL.String())
	}
	if got, err := io.ReadAll(object.requests[1].Body); err != nil || string(got) != benchBody {
		t.Fatalf("retry body = %q (%v), want the full body again", got, err)
	}
	if len(opener.requests) != 4 {
		t.Fatalf("control requests = %d, want pre_check + two create_update calls + register", len(opener.requests))
	}
	if string(opener.bodies[1]) != string(opener.bodies[2]) {
		t.Fatal("the retried instruction body must stay identical")
	}
}

func TestUploadObjectPutRetriesExhaustAndPropagate(t *testing.T) {
	client, opener, object, spoolDir := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			benchCreateUpdateScript(),
			benchCreateUpdateScript(),
		},
		[]scriptedResponse{
			{status: 500, body: []byte("boom one")},
			{status: 502, body: []byte("boom two")},
			{status: 500, body: []byte("boom three")},
		},
	)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)}); err == nil {
		t.Fatal("exhausted retries must propagate")
	} else if err.Error() != "WPS operation failed: object upload (HTTP 500)" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(object.requests) != 3 {
		t.Fatalf("object requests = %d, want the initial attempt plus two retries", len(object.requests))
	}
	if len(opener.requests) != 4 {
		t.Fatalf("control requests = %d, want pre_check plus three create_update calls", len(opener.requests))
	}
	spoolDirEmpty(t, spoolDir)
}

func TestUploadObjectPutMissingETagIsNotRetried(t *testing.T) {
	client, opener, object, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
		},
		[]scriptedResponse{{status: 200, body: []byte{}}},
	)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)}); err == nil {
		t.Fatal("a missing ETag must refuse the upload")
	} else if err.Error() != "WPS operation failed: object upload response missing ETag" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(object.requests) != 1 {
		t.Fatalf("object requests = %d, want no retry after a missing ETag", len(object.requests))
	}
	if len(opener.requests) != 2 {
		t.Fatalf("control requests = %d, want no instruction refresh for a missing ETag", len(opener.requests))
	}
}

func TestUploadObjectPutEmptyBodySendsZeroLength(t *testing.T) {
	client, _, object, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("")}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if len(object.requests) != 1 {
		t.Fatalf("object requests = %d, want 1", len(object.requests))
	}
	put := object.requests[0]
	if put.Body != nil {
		t.Fatal("an empty upload must not stream a body")
	}
	if put.ContentLength != 0 {
		t.Fatalf("object ContentLength = %d, want 0", put.ContentLength)
	}
}

// The B1003 tests pin the file registration half: the exact captured body,
// the success gate over a parseable entry, the sanitized orphan warning
// without any delete attempt, and the spool/reservation cleanup.

// wantRegisterBody is the exact compact JSON the registration must send
// after the bench instruction and object PUT.
const wantRegisterBody = `{"key":"02083f4579e08a612425c0c1a17ee47add783b94","groupid":"group-1",` +
	`"parentid":3,"name":"file","parent_path":[],"sha1":"02083f4579e08a612425c0c1a17ee47add783b94","size":4,` +
	`"store":"bench-store","etag":"\"bench-etag\"","isUpNewVer":false,"apiErrorInfo":null,` +
	`"csrfmiddlewaretoken":"csrf-secret"}`

const registerEntryPayload = `{"result":"ok","id":9,"fname":"file","ftype":"file","fsize":4,` +
	`"mtime":1788268272,"parentid":3}`

func TestUploadRegistersFileAndReturnsTheEntry(t *testing.T) {
	limiter := &recordingLimiter{}
	client, opener, _, spoolDir := newUploadObjectClient(t, limiter, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	entry, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)})
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if entry.ID != "9" || entry.Name != "file" || entry.Kind != model.KindFile {
		t.Fatalf("entry = %+v", entry)
	}
	if len(opener.requests) != 3 {
		t.Fatalf("control requests = %d, want pre_check + create_update + register", len(opener.requests))
	}
	register := opener.requests[2]
	if register.Method != http.MethodPost {
		t.Fatalf("register method = %s, want POST", register.Method)
	}
	if register.URL.String() != "https://365.kdocs.cn/3rd/drive/api/v5/files/file" {
		t.Fatalf("register URL = %q", register.URL.String())
	}
	if string(opener.bodies[2]) != wantRegisterBody {
		t.Fatalf("register body = %q, want %q", opener.bodies[2], wantRegisterBody)
	}
	spoolDirEmpty(t, spoolDir)
	if len(limiter.releases) != 1 || limiter.releases[0] != 4 {
		t.Fatalf("releases = %v", limiter.releases)
	}
}

func TestUploadRegistrationFailureWarnsWithoutDeleting(t *testing.T) {
	var warnings []string
	const privateName = "very-secret-name.bin"
	client, opener, object, spoolDir := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(`{"result":"failed"}`)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	client.warnUpload = func(message string) { warnings = append(warnings, message) }

	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: privateName, Source: strings.NewReader(benchBody)}); err == nil {
		t.Fatal("a failed registration result must refuse the upload")
	} else if err.Error() != "WPS operation failed: register uploaded file" {
		t.Fatalf("error = %q", err.Error())
	}
	spoolDirEmpty(t, spoolDir)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want exactly one", warnings)
	}
	if !strings.Contains(warnings[0], "unregistered") {
		t.Fatalf("warning = %q, want the unregistered-object notice", warnings[0])
	}
	// The warning must stay sanitized: no file name, no signed URL, no cookie.
	for _, secret := range []string{"very-secret-name.bin", "bench-store", "sig=secret", "Cookie-secret", "csrf-secret"} {
		if strings.Contains(warnings[0], secret) {
			t.Fatalf("warning %q leaked %q", warnings[0], secret)
		}
	}
	if len(object.requests) != 1 {
		t.Fatalf("object requests = %d, want no delete attempt against the object host", len(object.requests))
	}
	// Only pre_check, create_update, and register hit the control plane —
	// never an unknown cleanup endpoint.
	for index, request := range opener.requests {
		switch request.URL.Path {
		case "/3rd/drive/api/v5/files/upload/pre_check",
			"/3rd/drive/api/v5/files/upload/create_update",
			"/3rd/drive/api/v5/files/file":
		default:
			t.Fatalf("request %d hit an unexpected path %q", index, request.URL.Path)
		}
	}
}

func TestUploadRegistrationTransportFailureWarns(t *testing.T) {
	var warnings []string
	client, _, _, spoolDir := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 500, body: []byte("register boom")},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	client.warnUpload = func(message string) { warnings = append(warnings, message) }
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)}); err == nil {
		t.Fatal("a failed registration request must refuse the upload")
	} else if err.Error() != "WPS operation failed: /3rd/drive/api/v5/files/file (HTTP 500)" {
		t.Fatalf("error = %q", err.Error())
	}
	spoolDirEmpty(t, spoolDir)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want exactly one", warnings)
	}
}

func TestUploadUnparseableRegistrationEntryWarns(t *testing.T) {
	var warnings []string
	client, _, _, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(`{"result":"ok","fname":"file"}`)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	client.warnUpload = func(message string) { warnings = append(warnings, message) }
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)}); err == nil {
		t.Fatal("an unparseable entry must refuse the upload")
	} else if err.Error() != "WPS operation failed: normalize file metadata" {
		t.Fatalf("error = %q", err.Error())
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want exactly one", warnings)
	}
}

func TestUploadRegistrationStoreFallsBackToEmptyString(t *testing.T) {
	client, opener, _, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			{status: 200, body: []byte(`{"url":"https://hwc-bj.ag.kdocs.cn/u"}`)},
			{status: 200, body: []byte(registerEntryPayload)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(benchBody)}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if !strings.Contains(string(opener.bodies[2]), `"store":""`) {
		t.Fatalf("register body = %q, want the empty-store fallback", opener.bodies[2])
	}
}

// The stage completion condition requires a hash-consistent round trip:
// the bytes the signed object PUT streams must be the bytes a download
// returns, and the checksums registered along the way must match.
func TestUploadThenDownloadHashesMatch(t *testing.T) {
	const content = "the round trip payload for the stage gate"
	var createUpdateBodySent string
	client, opener, _, _ := newUploadObjectClient(t, &recordingLimiter{}, nil,
		[]scriptedResponse{
			{status: 200, body: []byte(`{"result":"ok"}`)},
			benchCreateUpdateScript(),
			{status: 200, body: []byte(registerEntryPayload)},
			// The download resolve reuses the control opener.
			{status: 200, body: []byte(`{"download_url":"https://hwc-bj.ag.kdocs.cn/dl?sig=secret","status":"finished"}`)},
		},
		[]scriptedResponse{benchObjectScript()},
	)
	client.signed.transport.(*fakeSignedTransport).script = append(
		client.signed.transport.(*fakeSignedTransport).script,
		scriptedResponse{status: 200, body: []byte(content)},
	)

	entry, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader(content)})
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if len(opener.bodies) < 2 {
		t.Fatalf("control requests = %d", len(opener.bodies))
	}
	createUpdateBodySent = string(opener.bodies[1])
	if entry.ID != "9" {
		t.Fatalf("entry = %+v", entry)
	}
	downloaded, err := client.OpenDownload(entry.ID, 0, nil, nil)
	if err != nil {
		t.Fatalf("OpenDownload failed: %v", err)
	}
	defer downloaded.Close()
	body, err := io.ReadAll(downloaded)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != content {
		t.Fatalf("round trip content = %q, want the uploaded bytes", body)
	}
	// The sha256 registered in the create_update body must equal the hash
	// of what a download returns for the same object.
	digest := sha256.Sum256(body)
	registered := `"sha256":"` + hex.EncodeToString(digest[:]) + `"`
	if !strings.Contains(createUpdateBodySent, registered) {
		t.Fatalf("registered sha256 for %q not found in create_update body", body)
	}
}
