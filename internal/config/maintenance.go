package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaintenanceWindow is the venue's weekly trading break — for Coinbase US
// perpetual-style futures, Friday 17:00-18:00 America/New_York (see
// docs/venue-coinbase-perps.md). It is stored as weekday plus a time-of-day range
// in a named location rather than as fixed UTC hours, because the break follows
// US Eastern local time and therefore moves with daylight saving.
//
// The window matters twice: no orders may be submitted inside it, and no funding
// rate is published for that hour, so the funding series records a gap and not a
// zero.
type MaintenanceWindow struct {
	Weekday  time.Weekday
	Start    time.Duration // since local midnight
	End      time.Duration // since local midnight, exclusive
	Location *time.Location
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// ParseMaintenanceWindow reads the MAINTENANCE_BREAK format:
// `<weekday> <HH:MM>-<HH:MM> <IANA location>`.
func ParseMaintenanceWindow(s string) (MaintenanceWindow, error) {
	fields := strings.Fields(s)
	if len(fields) != 3 {
		return MaintenanceWindow{}, errors.New(`expected "<weekday> <HH:MM>-<HH:MM> <IANA location>"`)
	}

	day, ok := weekdays[strings.ToLower(fields[0])]
	if !ok {
		return MaintenanceWindow{}, fmt.Errorf("unknown weekday %q", fields[0])
	}

	start, end, ok := strings.Cut(fields[1], "-")
	if !ok {
		return MaintenanceWindow{}, fmt.Errorf("time range %q is not <HH:MM>-<HH:MM>", fields[1])
	}
	from, err := parseTimeOfDay(start)
	if err != nil {
		return MaintenanceWindow{}, err
	}
	to, err := parseTimeOfDay(end)
	if err != nil {
		return MaintenanceWindow{}, err
	}
	// A window that wraps past midnight would span two weekdays and is not a
	// shape the venue uses; rejecting it keeps Contains a single comparison.
	if to <= from {
		return MaintenanceWindow{}, fmt.Errorf("end %s is not after start %s", end, start)
	}

	loc, err := time.LoadLocation(fields[2])
	if err != nil {
		return MaintenanceWindow{}, fmt.Errorf("unknown location %q", fields[2])
	}

	return MaintenanceWindow{Weekday: day, Start: from, End: to, Location: loc}, nil
}

func parseTimeOfDay(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("time %q is not HH:MM", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Contains reports whether t falls inside the window. The end is exclusive, so
// the hour the market reopens is immediately tradable.
func (w MaintenanceWindow) Contains(t time.Time) bool {
	if w.Location == nil {
		return false
	}
	local := t.In(w.Location)
	if local.Weekday() != w.Weekday {
		return false
	}
	sinceMidnight := time.Duration(local.Hour())*time.Hour +
		time.Duration(local.Minute())*time.Minute +
		time.Duration(local.Second())*time.Second
	return sinceMidnight >= w.Start && sinceMidnight < w.End
}

func (w MaintenanceWindow) String() string {
	if w.Location == nil {
		return "unset"
	}
	return fmt.Sprintf("%s %s-%s %s",
		w.Weekday.String()[:3], formatTimeOfDay(w.Start), formatTimeOfDay(w.End), w.Location)
}

func formatTimeOfDay(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}
