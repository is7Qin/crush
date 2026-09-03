package task

import (
	"database/sql"
	"encoding/json"
	"time"
)

// encodeFallbackModels renders the fallback list as JSON array text;
// the empty list stores as "[]".
func encodeFallbackModels(models []string) (string, error) {
	if len(models) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(models)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// nullString stores "" as NULL so optional strings round-trip as
// empty.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullGeneration stores the zero generation as NULL; generations are
// one-based, so zero always means unset.
func nullGeneration(g uint64) any {
	if g == 0 {
		return nil
	}
	return int64(g)
}

// nullUnixNano encodes a possibly-zero time as NULL so zero times
// round-trip as zero.
func nullUnixNano(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

// unixOrZero encodes a NOT NULL nanosecond timestamp so the zero
// time stores as integer 0 and round-trips back to zero.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func decodeStoredUnixNano(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

func decodeUnixNano(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(0, v.Int64).UTC()
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
