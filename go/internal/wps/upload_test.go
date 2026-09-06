// The B1000 upload tests pin the request-body and spool half of
// client.upload: the validation order, the upload budget, the coordinated
// spool reservation, the streamed checksums, and the cleanup of the
// temporary spool and the reservation on every failure surface.

package wps

import (
	"errors"
	"io"
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
	return client, opener, spoolDir
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
	client, _, spoolDir := newUploadClient(t, limiter, func(c *Config) { c.StreamChunkSize = 4 })
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
	if _, err := client.Upload(UploadRequest{ParentID: "3", Name: "file", Source: strings.NewReader("nine bytes")}); err == nil {
		t.Fatal("the stage boundary must refuse the upload")
	} else if err.Error() != "upload is not implemented in this stage" {
		t.Fatalf("error = %q", err.Error())
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
