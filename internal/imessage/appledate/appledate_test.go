package appledate

import (
	"testing"
	"time"
)

func TestToTime(t *testing.T) {
	want := time.Date(2026, 10, 9, 22, 1, 3, 0, time.UTC)
	secs := int64(want.Sub(Epoch) / time.Second)

	if got, ok := ToTime(secs * 1e9); !ok || !got.Equal(want) {
		t.Errorf("nanoseconds: got %v", got)
	}
	if got, ok := ToTime(secs); !ok || !got.Equal(want) {
		t.Errorf("seconds: got %v", got)
	}
	if _, ok := ToTime(0); ok {
		t.Error("zero should be unset")
	}
	if Ptr(0) != nil {
		t.Error("Ptr(0) should be nil")
	}
	if FromTime(want) != secs*1e9 {
		t.Error("FromTime round trip failed")
	}
}
