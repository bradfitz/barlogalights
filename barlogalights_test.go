package main

import (
	"testing"
	"time"
)

func TestIsNightTime(t *testing.T) {
	tests := []struct {
		local string // in America/Los_Angeles
		want  bool
	}{
		{"2026-11-17 20:59", false},
		{"2026-11-17 21:00", true},
		{"2026-11-17 23:59", true},
		{"2026-11-18 00:00", true},
		{"2026-11-18 06:59", true},
		{"2026-11-18 07:00", false},
		{"2026-11-18 12:00", false},
		{"2026-07-04 21:30", true}, // PDT
		{"2026-07-04 07:30", false},
	}
	for _, tt := range tests {
		tm, err := time.ParseInLocation("2006-01-02 15:04", tt.local, nightLoc)
		if err != nil {
			t.Fatal(err)
		}
		// Pass it as UTC to make sure the conversion happens.
		if got := isNightTime(tm.UTC()); got != tt.want {
			t.Errorf("isNightTime(%s) = %v; want %v", tt.local, got, tt.want)
		}
	}
}
