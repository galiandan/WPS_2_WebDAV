package storage

import (
	"context"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func TestBoundBatchRefusesReplacedSourceBeforeMutating(t *testing.T) {
	for _, operation := range []string{"delete", "move", "copy"} {
		t.Run(operation, func(t *testing.T) {
			client := newFakeClient()
			writer := &copyWriter{fakeClient: client}
			s := newTestStorage(t, client, func(c *StorageConfig) { c.Writer = writer })
			if _, err := s.Metadata("/top.txt"); err != nil {
				t.Fatal(err)
			}
			client.children["root"][1].ID = "replacement"
			err := s.ApplyBoundBatch(context.Background(), operation, "/top.txt", "/docs/top.txt", "top", "docs")
			if err != errBoundSourceChanged {
				t.Fatalf("err=%v", err)
			}
			if len(client.deleteCalls)+len(client.moveCalls)+len(writer.calls)+len(client.uploadCalls) != 0 {
				t.Fatal("mutated replacement file")
			}
		})
	}
}

func TestBoundBatchRefusesReplacedDestinationAndUsesOriginalIDs(t *testing.T) {
	for _, operation := range []string{"move", "copy"} {
		t.Run(operation, func(t *testing.T) {
			client := newFakeClient()
			writer := &copyWriter{fakeClient: client}
			s := newTestStorage(t, client, func(c *StorageConfig) { c.Writer = writer })
			if err := s.ApplyBoundBatch(context.Background(), operation, "/top.txt", "/docs/top.txt", "top", "old-docs"); err != errBoundDestinationChanged {
				t.Fatalf("err=%v", err)
			}
			if len(client.moveCalls)+len(writer.calls) != 0 {
				t.Fatal("mutated replaced destination")
			}
			if err := s.ApplyBoundBatch(context.Background(), operation, "/top.txt", "/docs/top.txt", "top", "docs"); err != nil {
				t.Fatal(err)
			}
			if operation == "move" && (len(client.moveCalls) != 1 || client.moveCalls[0] != [3]string{"top", "root", "docs"}) {
				t.Fatal(client.moveCalls)
			}
			if operation == "copy" && (len(writer.calls) != 1 || writer.calls[0] != [2]string{"top", "docs"}) {
				t.Fatal(writer.calls)
			}
		})
	}
}

func TestBatchMetadataBindsSpaceRootToRealFolder(t *testing.T) {
	multi, _ := newTestMulti(t, nil, func(c *MultiSpaceConfig) {
		c.StaticMounts = []Mount{{Name: "A", GroupID: "group-a", RootID: "root-a"}, {Name: "B", GroupID: "group-b", RootID: "root-b"}}
	})
	public, err := multi.Metadata("/A")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := multi.BatchMetadata("/A")
	if err != nil {
		t.Fatal(err)
	}
	if public.ID == bound.ID || bound.ID != "root-a" || bound.Kind != model.KindFolder {
		t.Fatalf("public=%+v bound=%+v", public, bound)
	}
	if err := multi.ApplyBoundBatch(context.Background(), "copy", "/A/file", "/B/file", "file-id", "root-b"); err == nil {
		t.Fatal("cross-space task accepted")
	}
}
