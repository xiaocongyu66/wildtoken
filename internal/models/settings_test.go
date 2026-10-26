package models

import (
	"math"
	"testing"
)

func TestDashboardMultiplierRange(t *testing.T) {
	for _, tc := range []struct {
		value float64
		ok    bool
	}{
		{1, true},
		{0.5, true},
		{MaxDashboardMultiplier, true},
		{0, false},
		{-1, false},
		{MaxDashboardMultiplier + 0.1, false},
		// NaN slips past a "reject out of range" check; must still be refused.
		{math.NaN(), false},
	} {
		settings := DefaultRuntimeSettings()
		settings.DashboardMultiplier = tc.value
		if err := settings.Validate(); (err == nil) != tc.ok {
			t.Errorf("multiplier %v: err = %v, want ok = %v", tc.value, err, tc.ok)
		}
	}
}

// Bodies stored by chunk let the window and the cap grow well past what whole
// snapshots allowed: 10,000 rows and 1 MiB.
func TestLogBodyLimitsRange(t *testing.T) {
	for _, tc := range []struct {
		keep, maxBytes int64
		ok             bool
	}{
		{1, 0, true},
		{MaxLogBodyKeepCount, MaxLogBodyMaxBytes, true},
		{0, 0, false},
		{MaxLogBodyKeepCount + 1, 0, false},
		{1, MaxLogBodyMaxBytes + 1, false},
		{1, -1, false},
	} {
		settings := DefaultRuntimeSettings()
		settings.LogBodyKeepCount, settings.LogBodyMaxBytes = tc.keep, tc.maxBytes
		if err := settings.Validate(); (err == nil) != tc.ok {
			t.Errorf("keep %d, max %d: err = %v, want ok = %v", tc.keep, tc.maxBytes, err, tc.ok)
		}
	}
}
