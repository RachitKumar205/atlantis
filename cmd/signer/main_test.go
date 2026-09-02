package main

import (
	"testing"
	"time"
)

// A requested lifetime can only shorten a certificate, never lengthen it.
func TestClampTTL(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want time.Duration
	}{
		{0, certTTL},
		{-100, certTTL},
		{60, minTTL},
		{3600, time.Hour},
		{int(certTTL.Seconds()) + 1, certTTL},
		{int(minTTL.Seconds()), minTTL},
	} {
		if got := clampTTL(tc.in); got != tc.want {
			t.Errorf("clampTTL(%d) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
