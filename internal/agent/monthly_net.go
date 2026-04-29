package agent

import (
	"fmt"
	"strings"
	"time"
)

func localMonthKey(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		t = time.Now()
	}
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01")
}

func localTimezoneName(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		t = time.Now()
	}
	if loc == nil {
		loc = time.Local
	}
	if name := strings.TrimSpace(loc.String()); name != "" && name != "Local" {
		return name
	}
	_, offset := t.In(loc).Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, offset/3600, (offset%3600)/60)
}
