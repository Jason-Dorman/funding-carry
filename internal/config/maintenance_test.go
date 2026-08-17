package config

import (
	"testing"
	"time"
)

func TestParseMaintenanceWindow(t *testing.T) {
	t.Parallel()

	w, err := ParseMaintenanceWindow(defaultMaintenanceBreak)
	if err != nil {
		t.Fatalf("ParseMaintenanceWindow(%q): %v", defaultMaintenanceBreak, err)
	}

	if got, want := w.Weekday, time.Friday; got != want {
		t.Errorf("Weekday = %v, want %v", got, want)
	}
	if got, want := w.Start, 17*time.Hour; got != want {
		t.Errorf("Start = %v, want %v", got, want)
	}
	if got, want := w.End, 18*time.Hour; got != want {
		t.Errorf("End = %v, want %v", got, want)
	}
	if got, want := w.Location.String(), "America/New_York"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got, want := w.String(), defaultMaintenanceBreak; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestParseMaintenanceWindowRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"too few fields", "Fri 17:00-18:00"},
		{"too many fields", "Fri 17:00-18:00 America/New_York extra"},
		{"unknown weekday", "Frday 17:00-18:00 America/New_York"},
		{"missing range separator", "Fri 17:00 America/New_York"},
		{"non-clock start", "Fri 25:00-18:00 America/New_York"},
		{"non-clock end", "Fri 17:00-18:61 America/New_York"},
		{"end before start", "Fri 18:00-17:00 America/New_York"},
		{"end equal to start", "Fri 17:00-17:00 America/New_York"},
		{"unknown location", "Fri 17:00-18:00 Mars/Olympus_Mons"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := ParseMaintenanceWindow(tt.input); err == nil {
				t.Fatalf("ParseMaintenanceWindow(%q): want error, got nil", tt.input)
			}
		})
	}
}

// Contains is what gates order submission and what marks the funding hour as a
// gap rather than a zero, so the boundaries and the daylight-saving behavior are
// both pinned down here.
func TestMaintenanceWindowContains(t *testing.T) {
	t.Parallel()

	w, err := ParseMaintenanceWindow(defaultMaintenanceBreak)
	if err != nil {
		t.Fatalf("ParseMaintenanceWindow: %v", err)
	}

	tests := []struct {
		name string
		when time.Time
		want bool
	}{
		// Eastern Daylight Time (UTC-4): the break is 21:00-22:00 UTC.
		{"one minute before the break, EDT", time.Date(2026, 8, 14, 20, 59, 0, 0, time.UTC), false},
		{"at the open of the break, EDT", time.Date(2026, 8, 14, 21, 0, 0, 0, time.UTC), true},
		{"mid-break, EDT", time.Date(2026, 8, 14, 21, 30, 0, 0, time.UTC), true},
		{"at the reopen, EDT", time.Date(2026, 8, 14, 22, 0, 0, 0, time.UTC), false},

		// Eastern Standard Time (UTC-5): the same local break is 22:00-23:00 UTC.
		// A window pinned to fixed UTC hours would get both of these wrong.
		{"break hour in EST", time.Date(2026, 1, 16, 22, 30, 0, 0, time.UTC), true},
		{"EDT break hour applied in EST", time.Date(2026, 1, 16, 21, 30, 0, 0, time.UTC), false},

		// Same clock time, wrong day.
		{"Thursday at 17:30 ET", time.Date(2026, 8, 13, 21, 30, 0, 0, time.UTC), false},
		{"Saturday at 17:30 ET", time.Date(2026, 8, 15, 21, 30, 0, 0, time.UTC), false},

		// Saturday in UTC, still Friday in New York: the weekday has to be read
		// in the venue's location, not the process's.
		{"Saturday 00:30 UTC is Friday 20:30 ET", time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := w.Contains(tt.when); got != tt.want {
				t.Errorf("Contains(%s) = %v, want %v", tt.when.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestZeroMaintenanceWindowContainsNothing(t *testing.T) {
	t.Parallel()

	var w MaintenanceWindow
	if w.Contains(time.Date(2026, 8, 14, 21, 30, 0, 0, time.UTC)) {
		t.Error("the zero window should never report a maintenance break")
	}
	if got, want := w.String(), "unset"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
