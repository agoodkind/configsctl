// Package clock is the single source of wall-clock time, so the rest of the
// tree stays testable and the no-time.Now rule holds everywhere else.
package clock

import "time"

// NowUTC returns the current time in UTC.
func NowUTC() time.Time {
	return time.Now().UTC()
}

// Stamp returns the current UTC time formatted as an RFC3339 timestamp, the form
// the baseline file records.
func Stamp() string {
	return NowUTC().Format(time.RFC3339)
}

// FileStamp returns the current UTC time in a form that is safe inside a
// filename and still sorts chronologically, as 20060102T150405Z.
func FileStamp() string {
	return NowUTC().Format("20060102T150405Z")
}
