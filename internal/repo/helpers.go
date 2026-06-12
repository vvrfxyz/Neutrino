package repo

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// nextLocalDayStart returns midnight of the day after the given day, reconstructed
// as a fresh time.Date in loc so DST transitions do not leave us on the wrong local wall-clock.
func nextLocalDayStart(loc *time.Location, day time.Time) time.Time {
	next := day.AddDate(0, 0, 1)
	return time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, loc)
}

func quotaWindowBounds(cycleType, tz string, t time.Time) (time.Time, time.Time, string) {
	loc, err := time.LoadLocation(strings.TrimSpace(tz))
	if err != nil || loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)

	switch cycleType {
	case "day":
		startLocal := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		endLocal := nextLocalDayStart(loc, startLocal)
		return startLocal.UTC(), endLocal.UTC(), startLocal.Format("2006-01-02")
	case "week":
		weekday := int(local.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		startLocal := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(weekday - 1))
		endLocal := startLocal
		for i := 0; i < 7; i++ {
			endLocal = nextLocalDayStart(loc, endLocal)
		}
		year, week := startLocal.ISOWeek()
		return startLocal.UTC(), endLocal.UTC(), fmt.Sprintf("%04d-W%02d", year, week)
	default:
		startLocal := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
		endLocal := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc)
		return startLocal.UTC(), endLocal.UTC(), startLocal.Format("2006-01")
	}
}

func nullableString(v string) sql.NullString {
	v = strings.TrimSpace(v)
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func nullableInt64(v int64) sql.NullInt64 {
	if v <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

func parseOptionalRFC3339(raw sql.NullString) *time.Time {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw.String)
	if err != nil {
		return nil
	}
	return &parsed
}

func trafficBucketKey(t time.Time, period string) string {
	t = t.UTC()
	switch period {
	case "hourly":
		return t.Format("2006-01-02T15:00:00Z")
	case "daily":
		return t.Format("2006-01-02T00:00:00Z")
	case "monthly":
		// Truncate to month start explicitly: a literal day inside a Format
		// layout would be parsed as a placeholder (the old "2006-01-01"
		// layout rendered year-month-month).
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	default:
		return t.Format(time.RFC3339)
	}
}
