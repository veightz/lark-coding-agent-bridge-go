package pricing

import (
	"testing"
	"time"
)

func TestLookup(t *testing.T) {
	tests := []struct {
		model    string
		wantOk   bool
		wantRate float64
	}{
		{"deepseek-chat", true, 0.5},
		{"deepseek/deepseek-chat", true, 0.5},
		{"deepseek-reasoner", true, 1.0},
		{"deepseek/deepseek-r1", true, 1.0},
		{"claude-sonnet-4-20250514", true, 22.0},
		{"claude-3.5-sonnet", true, 22.0},
		{"claude-3-haiku-20240307", true, 5.8},
		{"gpt-4o", true, 18.3},
		{"gpt-4o-mini", true, 1.1},
		{"unknown-model", false, 0},
		{"", false, 0},
	}
	for _, tt := range tests {
		r, _, ok := Lookup(tt.model)
		if ok != tt.wantOk {
			t.Errorf("Lookup(%q) ok=%v, want %v", tt.model, ok, tt.wantOk)
		}
		if ok && r.Input != tt.wantRate {
			t.Errorf("Lookup(%q) Input=%.2f, want %.2f", tt.model, r.Input, tt.wantRate)
		}
	}
}

func TestCalculate(t *testing.T) {
	// 2026-07-27 10:00 UTC = 18:00 Beijing → peak
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		model     string
		input     int
		output    int
		cached    int
		wantCNY   float64
		wantLabel string
	}{
		// deepseek-chat peak: (800*0.5 + 200*0.1 + 500*2) / 1M * 2 = 0.00284
		{
			model:     "deepseek-chat",
			input:     1000,
			output:    500,
			cached:    200,
			wantCNY:   0.0028,
			wantLabel: "¥0.0028 (高峰2x)",
		},
		// deepseek-chat peak: (1000*0.5 + 0*0.1 + 500*2) / 1M * 2 = 0.003
		{
			model:     "deepseek-chat",
			input:     1000,
			output:    500,
			cached:    0,
			wantCNY:   0.003,
			wantLabel: "¥0.0030 (高峰2x)",
		},
		// deepseek-chat peak: (0*0.5 + 1000*0.1 + 500*2) / 1M * 2 = 0.0022
		{
			model:     "deepseek-chat",
			input:     1000,
			output:    500,
			cached:    1000,
			wantCNY:   0.0022,
			wantLabel: "¥0.0022 (高峰2x)",
		},
		// claude-sonnet-4 (no peak): (800*22 + 200*2.2 + 500*110) / 1M
		// = (17600 + 440 + 55000) / 1M = 73040 / 1M = 0.07304
		{
			model:     "claude-sonnet-4",
			input:     1000,
			output:    500,
			cached:    200,
			wantCNY:   0.073,
			wantLabel: "¥0.0730",
		},
	}
	for _, tt := range tests {
		cny, label := Calculate(tt.model, tt.input, tt.output, tt.cached, now)
		if cny != tt.wantCNY {
			t.Errorf("Calculate(%q, %d, %d, %d) = %.6f, want %.6f", tt.model, tt.input, tt.output, tt.cached, cny, tt.wantCNY)
		}
		if label != tt.wantLabel {
			t.Errorf("Calculate(%q, %d, %d, %d) label=%q, want %q", tt.model, tt.input, tt.output, tt.cached, label, tt.wantLabel)
		}
	}
}

func TestCalculateNonPeak(t *testing.T) {
	// 2026-07-27 6:00 UTC = 14:00 Beijing → non-peak
	now := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)
	got, _ := Calculate("deepseek-chat", 1000, 500, 200, now)
	// non-peak: (800*0.5 + 200*0.1 + 500*2) / 1M = 0.00142
	if got != 0.0014 {
		t.Errorf("non-peak got %.6f, want 0.001400", got)
	}
}

func TestCalculatePeakEdge016(t *testing.T) {
	// 2026-07-27 8:00 UTC = 16:00 Beijing → peak (inclusive)
	now := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	got, label := Calculate("deepseek-chat", 1000, 500, 200, now)
	if label == "" || got == 0.00142 {
		t.Errorf("16:00 should be peak: got label=%q, cny=%.6f", label, got)
	}
}

func TestCalculatePeakEdge0030(t *testing.T) {
	// 2026-07-27 16:29 UTC = 00:29+1 Beijing → peak
	now := time.Date(2026, 7, 27, 16, 29, 0, 0, time.UTC)
	got, label := Calculate("deepseek-chat", 1000, 500, 200, now)
	if label == "" || got == 0.00142 {
		t.Errorf("00:29 should be peak: got label=%q, cny=%.6f", label, got)
	}
}

func TestCalculatePeakEdge0030End(t *testing.T) {
	// 2026-07-27 16:30 UTC = 00:30+1 Beijing → NOT peak
	now := time.Date(2026, 7, 27, 16, 30, 0, 0, time.UTC)
	got, label := Calculate("deepseek-chat", 1000, 500, 200, now)
	if label == "¥0.0014 (高峰2x)" {
		t.Errorf("00:30 should not be peak: got label=%q, cny=%.6f", label, got)
	}
}

func TestCalculateUnknownModel(t *testing.T) {
	cny, label := Calculate("unknown-model", 1000, 500, 200, time.Now())
	if cny != 0 || label != "" {
		t.Errorf("unknown model: got %.4f, %q; want 0, \"\"", cny, label)
	}
}

func TestCalculateCachedOnly(t *testing.T) {
	now := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC) // non-peak
	// All cached: (0*0.5 + 1000*0.1 + 500*2) / 1M = 0.0011
	got, _ := Calculate("deepseek-chat", 1000, 500, 1000, now)
	if got != 0.0011 {
		t.Errorf("all cached got %.6f, want 0.001100", got)
	}
}

func TestCalculateOpenCodeStyleCache(t *testing.T) {
	// OpenCode: input is non-cached only, cache.read may exceed input.
	// non-peak: (165*0.5 + 19200*0.1 + 50*2) / 1M = (82.5 + 1920 + 100) / 1M = 0.0021025 → 0.0021
	now := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)
	got, _ := Calculate("deepseek-chat", 165, 50, 19200, now)
	if got != 0.0021 {
		t.Errorf("opencode-style cache got %.6f, want 0.002100", got)
	}
}

func TestFormatCNY(t *testing.T) {
	if got := FormatCNY(0); got != "" {
		t.Errorf("FormatCNY(0) = %q, want empty", got)
	}
	if got := FormatCNY(0.0015); got != "¥0.0015" {
		t.Errorf("FormatCNY(0.0015) = %q, want ¥0.0015", got)
	}
	if got := FormatCNY(0.1234); got != "¥0.1234" {
		t.Errorf("FormatCNY(0.1234) = %q, want ¥0.1234", got)
	}
}

func TestPeakBeijing(t *testing.T) {
	loc := time.FixedZone("UTC", 0)

	tests := []struct {
		t    time.Time
		want bool
	}{
		{time.Date(2026, 7, 27, 8, 0, 0, 0, loc), true},    // 16:00 CST → peak
		{time.Date(2026, 7, 27, 8, 1, 0, 0, loc), true},    // 16:01 CST → peak
		{time.Date(2026, 7, 27, 12, 0, 0, 0, loc), true},   // 20:00 CST → peak
		{time.Date(2026, 7, 27, 15, 59, 0, 0, loc), true},  // 23:59 CST → peak
		{time.Date(2026, 7, 27, 16, 0, 0, 0, loc), true},   // 00:00 CST → peak (between 16-00:30)
		{time.Date(2026, 7, 27, 16, 15, 0, 0, loc), true},  // 00:15 CST → peak
		{time.Date(2026, 7, 27, 16, 29, 0, 0, loc), true},  // 00:29 CST → peak
		{time.Date(2026, 7, 27, 16, 30, 0, 0, loc), false}, // 00:30 CST → not peak
		{time.Date(2026, 7, 27, 7, 59, 0, 0, loc), false},  // 15:59 CST → not peak
	}
	for _, tt := range tests {
		if got := peakBeijing(tt.t); got != tt.want {
			t.Errorf("peakBeijing(%v) = %v, want %v", tt.t, got, tt.want)
		}
	}
}
