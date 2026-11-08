package cronspec

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		expr, tz, err string
	}{
		{"0 3 * * *", "", ""},
		{"*/15 9-17 * * 1-5", "America/New_York", ""},
		{"@daily", "UTC", ""},
		{"@every 2h", "", ""},
		{"@every 30s", "", "at most once a minute"},
		{"0 3 * *", "", "invalid cron expression"},
		{"0 0 30 2 *", "", "never runs"},
		{"0 3 * * *", "Mars/Olympus", "unknown time zone"},
		{"0 3 * * *", "Local", "unknown time zone"},
		{"CRON_TZ=UTC 0 3 * * *", "", "timezone field"},
		{"  ", "", "empty"},
	} {
		_, err := Parse(tc.expr, tc.tz)
		if tc.err == "" && err != nil {
			t.Errorf("Parse(%q, %q) = %v", tc.expr, tc.tz, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("Parse(%q, %q) = %v, want error containing %q", tc.expr, tc.tz, err, tc.err)
		}
	}
}

func TestNextIsInTheTimeZone(t *testing.T) {
	spec, err := Parse("30 2 * * *", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-01-15 00:00 UTC: Berlin is UTC+1, so 02:30 there is 01:30 UTC.
	from := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	if got, want := spec.Next(from), time.Date(2026, 1, 15, 1, 30, 0, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("Next = %v, want %v", got, want)
	}
	// In summer Berlin is UTC+2.
	from = time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	if got, want := spec.Next(from), time.Date(2026, 7, 15, 0, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
	runs := spec.NextN(from, 3)
	if len(runs) != 3 || runs[2].Sub(runs[0]) != 48*time.Hour {
		t.Errorf("NextN = %v", runs)
	}
}
