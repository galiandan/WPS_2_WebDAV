//go:build unix

package securefile

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenPrivateRegularSupportsBoundedBinaryFiles(t *testing.T) {
	path := writePrivate(t, privateDir(t), "artifact.zip", "PK\xff\x00")
	file, err := OpenPrivateRegular(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	body, err := io.ReadAll(file)
	if err != nil || string(body) != "PK\xff\x00" {
		t.Fatalf("binary file changed: %q, %v", body, err)
	}
	if rejected, err := OpenPrivateRegular(path, 3); rejected != nil || CodeOf(err) != CodePostOpenUnsafe {
		if rejected != nil {
			rejected.Close()
		}
		t.Fatalf("oversized artifact accepted: %v", err)
	}
}

func TestOpenPrivateRegularRejectsUnsafePaths(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "artifact.zip", "archive")
	link := filepath.Join(dir, "link.zip")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if file, err := OpenPrivateRegular(link, 100); file != nil || CodeOf(err) != CodeNotRegular {
		if file != nil {
			file.Close()
		}
		t.Fatalf("symlink accepted: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if file, err := OpenPrivateRegular(path, 100); file != nil || CodeOf(err) != CodeFileUnsafe {
		if file != nil {
			file.Close()
		}
		t.Fatalf("public file accepted: %v", err)
	}
	parentLink := filepath.Join(privateDir(t), "parent")
	if err := os.Symlink(dir, parentLink); err != nil {
		t.Fatal(err)
	}
	if file, err := OpenPrivateRegular(filepath.Join(parentLink, "artifact.zip"), 100); file != nil || CodeOf(err) != CodeParentSymlink {
		if file != nil {
			file.Close()
		}
		t.Fatalf("symlink parent accepted: %v", err)
	}
}

func TestOpenSecureRejectsSymlinkSwappedAfterPrecheck(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "artifact.zip", "archive")
	target := writePrivate(t, dir, "other", "private bytes")
	if err := checkStatePath(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	file, err := openSecure(path)
	if file != nil {
		file.Close()
		t.Fatal("followed a symlink swapped after precheck")
	}
	wantCode(t, err, CodeOpenFailed)
}

func TestOpenSecureDoesNotBlockOnFIFOSwappedAfterPrecheck(t *testing.T) {
	dir := privateDir(t)
	path := writePrivate(t, dir, "artifact.zip", "archive")
	if err := checkStatePath(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := openSecure(path)
		if err == nil {
			_, err = checkAfterOpen(file, 100)
			file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		wantCode(t, err, CodeNotRegular)
	case <-time.After(time.Second):
		// Unblock an incorrect blocking implementation before failing.
		if file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			file.Close()
		}
		t.Fatal("opening a swapped FIFO blocked before type validation")
	}
}
