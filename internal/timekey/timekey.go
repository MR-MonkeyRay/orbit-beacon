package timekey

import "time"

const persistedLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Format is the sole persisted representation for comparable wall-clock times.
// UTC and fixed nanosecond precision make bytewise SQLite ordering match
// chronological order.
func Format(value time.Time) string {
	return value.UTC().Format(persistedLayout)
}

// Parse reads the persisted representation and normalizes the result to UTC.
func Parse(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
