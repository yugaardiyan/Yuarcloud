package handler

import (
	"testing"
	"time"
)

// The admin form stores the interval as "15m", "1h", "6h" or "24h" and this
// endpoint has to answer in one of those spellings. Go's Duration.String
// expands every one of them — "1h0m0s", "6h0m0s", "24h0m0s" — so anything
// comparing the two as text found no match, and the form silently displayed
// its first option no matter what had been saved.
func TestFormatDurationMatchesTheFormSpelling(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		// The four the dropdown offers.
		{15 * time.Minute, "15m"},
		{time.Hour, "1h"},
		{6 * time.Hour, "6h"},
		{24 * time.Hour, "24h"},
		// What a hand-edited config can hold.
		{45 * time.Minute, "45m"},
		{90 * time.Minute, "90m"},
		{30 * time.Minute, "30m"},
		{2 * time.Hour, "2h"},
		{30 * time.Second, "30s"},
		{0, "0s"},
		// Not a whole unit, so Go's own spelling — and past a minute that
		// spelling is the readable one: "90s" would be the same duration.
		{45*time.Minute + 30*time.Second, "45m30s"},
		{90 * time.Second, "1m30s"},
		{1500 * time.Millisecond, "1.5s"},
	} {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The round trip that actually matters: the four spellings the admin form
// sends have to come back identical, or the form cannot show what it saved.
func TestFormatDurationRoundTripsTheFormsOwnValues(t *testing.T) {
	for _, sent := range []string{"15m", "1h", "6h", "24h", "45m", "30m", "2h", "0s"} {
		d, err := time.ParseDuration(sent)
		if err != nil {
			t.Fatalf("the form could not send %q: %v", sent, err)
		}
		if got := formatDuration(d); got != sent {
			t.Errorf("sent %q, endpoint answered %q", sent, got)
		}
	}
}

// Whatever the endpoint answers still has to mean the same length, so a client
// that stores the answer and sends it back has not quietly changed anything.
func TestFormatDurationPreservesTheLength(t *testing.T) {
	for _, in := range []time.Duration{
		15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour,
		45 * time.Minute, 90 * time.Second, 45*time.Minute + 30*time.Second,
		1500 * time.Millisecond, 0,
	} {
		answered := formatDuration(in)
		back, err := time.ParseDuration(answered)
		if err != nil {
			t.Errorf("formatDuration(%v) = %q, which will not parse: %v", in, answered, err)
			continue
		}
		if back != in {
			t.Errorf("formatDuration(%v) = %q, which parses back as %v", in, answered, back)
		}
	}
}
