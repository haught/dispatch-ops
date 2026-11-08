// Package cronspec parses schedule cron expressions and computes their run
// times in a time zone.
package cronspec

import (
	"errors"
	"fmt"
	"strings"
	"time"
	// Time zones must work wherever the binary runs, including in images
	// without /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

// MinInterval is the shortest time allowed between two runs.
const MinInterval = time.Minute

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Spec is a parsed cron expression in a time zone.
type Spec struct {
	sched cron.Schedule
	loc   *time.Location
}

// Parse parses a five-field cron expression (minute hour day-of-month month
// day-of-week) or a descriptor such as @daily or @every 6h, read in the
// IANA time zone tz ("" means UTC).
func Parse(expr, tz string) (Spec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Spec{}, errors.New("the cron expression is empty")
	}
	// The zone is a separate field; an inline one would override it.
	if strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") {
		return Spec{}, errors.New("set the time zone with the timezone field, not in the cron expression")
	}
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "Local" {
		return Spec{}, fmt.Errorf("unknown time zone %q; use an IANA name such as Europe/Berlin", tz)
	}
	sched, err := parser.Parse(expr)
	if err != nil {
		return Spec{}, fmt.Errorf("invalid cron expression %q: %v", expr, err)
	}
	s := Spec{sched: sched, loc: loc}
	// Five-field expressions can't run more often than every minute, but
	// @every can.
	if every, ok := sched.(cron.ConstantDelaySchedule); ok && every.Delay < MinInterval {
		return Spec{}, fmt.Errorf("schedules may run at most once a minute; %q is more often", expr)
	}
	if s.Next(time.Now()).IsZero() {
		return Spec{}, fmt.Errorf("the cron expression %q never runs", expr)
	}
	return s, nil
}

// Next returns the first run time after t, in UTC, or the zero time if
// there is none.
func (s Spec) Next(t time.Time) time.Time {
	n := s.sched.Next(t.In(s.loc))
	if n.IsZero() {
		return n
	}
	return n.UTC()
}

// NextN returns up to n run times after t.
func (s Spec) NextN(t time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for range n {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}
