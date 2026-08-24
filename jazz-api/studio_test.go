package main

import "testing"

func TestValidManualClipBounds(t *testing.T) {
	tests := []struct {
		name                  string
		startMS, endMS, total int
		want                  bool
	}{
		{name: "ten second clip", startMS: 5000, endMS: 15000, total: 30000, want: true},
		{name: "touches recording end", startMS: 20000, endMS: 30000, total: 30000, want: true},
		{name: "negative start", startMS: -1, endMS: 10000, total: 30000},
		{name: "shorter than half second", startMS: 1000, endMS: 1499, total: 30000},
		{name: "past recording end", startMS: 20000, endMS: 30001, total: 30000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validManualClipBounds(test.startMS, test.endMS, test.total); got != test.want {
				t.Fatalf("validManualClipBounds(%d, %d, %d) = %v, want %v", test.startMS, test.endMS, test.total, got, test.want)
			}
		})
	}
}

func TestValidClipSplitPoint(t *testing.T) {
	if !validClipSplitPoint(1000, 1500, 2000) {
		t.Fatal("half-second segments should be accepted")
	}
	if validClipSplitPoint(1000, 1499, 3000) {
		t.Fatal("a short left segment should be rejected")
	}
	if validClipSplitPoint(1000, 2501, 3000) {
		t.Fatal("a short right segment should be rejected")
	}
}
