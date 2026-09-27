package api

import "testing"

// Rates reach here as smoothed floats, whose tail is arithmetic rather than
// measurement.
func TestRoundRate(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		// What a torrent that had gone quiet used to publish.
		{0.0000025947061343373236, 0},
		{1234.56789, 1234.57},
		{0.005, 0.01},
		{0.004, 0},
		{1 << 20, 1 << 20},
	}
	for _, tc := range tests {
		if got := roundRate(tc.in); got != tc.want {
			t.Errorf("roundRate(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
