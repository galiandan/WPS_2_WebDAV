package budget

import (
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func TestReserveDiskAccountsBelowMemoryThresholdAndSharesSpoolBudget(t *testing.T) {
	var calls []string
	b := newSpoolBudget(t, 300, &calls)
	artifact, err := b.ReserveDisk("/artifact-data", 40, 0)
	if err != nil || artifact != 90 || len(calls) != 1 || calls[0] != "/artifact-data" {
		t.Fatalf("disk file below memory threshold was not reserved: %d, %v, %v", artifact, err, calls)
	}
	spool, err := b.ReserveSpool(101, 0)
	if err != nil || spool != 151 || b.Stats().SpoolReservedBytes != 241 {
		t.Fatalf("shared reservation: %d, %v, %+v", spool, err, b.Stats())
	}
	if current, err := b.ReserveDisk("/artifact-data", 100, artifact); err == nil || current != artifact || b.Stats().SpoolReservedBytes != 241 {
		t.Fatalf("failed growth changed reservation: %d, %v, %+v", current, err, b.Stats())
	}
	b.ReleaseSpool(spool)
	artifact, err = b.ReserveDisk("/artifact-data", 100, artifact)
	if err != nil || artifact != 150 || b.Stats().SpoolReservedBytes != 150 {
		t.Fatalf("resizing disk reservation: %d, %v, %+v", artifact, err, b.Stats())
	}
	b.ReleaseSpool(artifact)
	if b.Stats().SpoolReservedBytes != 0 {
		t.Fatal("disk reservation leaked")
	}
}

func TestReserveDiskRejectsInvalidSizesBeforeAccounting(t *testing.T) {
	var calls []string
	b := newSpoolBudget(t, 1<<62, &calls)
	for _, size := range []int64{-1, 1<<62 - 49, 1<<63 - 1} {
		reserved, err := b.ReserveDisk("/artifact-data", size, 0)
		storageErr, ok := model.AsStorageError(err)
		if !ok || storageErr.Kind != model.KindInsufficientStorage || reserved != 0 || len(calls) != 0 || b.Stats().SpoolReservedBytes != 0 {
			t.Fatalf("invalid disk size %d affected accounting: %d, %v", size, reserved, err)
		}
	}
}
