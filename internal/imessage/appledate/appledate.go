// Package appledate converts between Apple's Core Data epoch timestamps
// (as stored in chat.db) and time.Time.
//
// chat.db stores dates relative to 2001-01-01T00:00:00Z. Since macOS 10.13
// they are in nanoseconds; older databases use seconds. A value of 0 means
// "not set".
package appledate

import "time"

// Epoch is the Apple reference date, 2001-01-01T00:00:00Z.
var Epoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// nanosThreshold separates second-based from nanosecond-based values. Second
// values stay below 1e10 until the year 2318; nanosecond values passed 1e10
// ten seconds after the epoch.
const nanosThreshold = 1e10

// ToTime converts a chat.db date value. ok is false for 0 (unset).
func ToTime(v int64) (t time.Time, ok bool) {
	if v == 0 {
		return time.Time{}, false
	}
	if v > nanosThreshold || v < -nanosThreshold {
		return Epoch.Add(time.Duration(v)).UTC(), true
	}
	return Epoch.Add(time.Duration(v) * time.Second).UTC(), true
}

// Ptr is like ToTime but returns nil for unset values, for use in JSON DTOs.
func Ptr(v int64) *time.Time {
	t, ok := ToTime(v)
	if !ok {
		return nil
	}
	return &t
}

// FromTime converts t to a nanosecond chat.db value, suitable for WHERE
// clauses against modern databases.
func FromTime(t time.Time) int64 {
	return int64(t.Sub(Epoch))
}
