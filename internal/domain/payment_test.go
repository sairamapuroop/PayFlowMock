package domain

import "testing"

func TestValidTransition(t *testing.T) {
	tests := []struct {
		name  string
		from  Status
		to    Status
		want  bool
	}{
		// initiated → only processing
		{"initiated to processing", StatusInitiated, StatusProcessing, true},
		{"initiated to success", StatusInitiated, StatusSuccess, false},
		{"initiated to failed", StatusInitiated, StatusFailed, false},
		{"initiated to refunded", StatusInitiated, StatusRefunded, false},
		{"initiated to initiated", StatusInitiated, StatusInitiated, false},

		// processing → success or failed
		{"processing to success", StatusProcessing, StatusSuccess, true},
		{"processing to failed", StatusProcessing, StatusFailed, true},
		{"processing to processing", StatusProcessing, StatusProcessing, false},
		{"processing to initiated", StatusProcessing, StatusInitiated, false},
		{"processing to refunded", StatusProcessing, StatusRefunded, false},

		// success → only refunded
		{"success to refunded", StatusSuccess, StatusRefunded, true},
		{"success to success", StatusSuccess, StatusSuccess, false},
		{"success to processing", StatusSuccess, StatusProcessing, false},
		{"success to failed", StatusSuccess, StatusFailed, false},
		{"success to initiated", StatusSuccess, StatusInitiated, false},

		// failed → terminal
		{"failed to any processing", StatusFailed, StatusProcessing, false},
		{"failed to success", StatusFailed, StatusSuccess, false},
		{"failed to failed", StatusFailed, StatusFailed, false},

		// refunded → terminal
		{"refunded to success", StatusRefunded, StatusSuccess, false},
		{"refunded to refunded", StatusRefunded, StatusRefunded, false},

		// unknown from status
		{"unknown from to processing", Status("unknown"), StatusProcessing, false},
		{"empty from to success", Status(""), StatusSuccess, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("ValidTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestValidCurrency(t *testing.T) {
	tests := []struct {
		currency string
		want     bool
	}{
		{"USD", true},
		{"EUR", true},
		{"GBP", true},
		{"INR", true},
		{"usd", false},
		{"", false},
		{"JPY", false},
		{"XXX", false},
	}
	for _, tt := range tests {
		t.Run(tt.currency, func(t *testing.T) {
			if got := ValidCurrency(tt.currency); got != tt.want {
				t.Errorf("ValidCurrency(%q) = %v, want %v", tt.currency, got, tt.want)
			}
		})
	}
}

func TestIsKnownStatus(t *testing.T) {
	tests := []struct {
		s    Status
		want bool
	}{
		{StatusInitiated, true},
		{StatusProcessing, true},
		{StatusSuccess, true},
		{StatusFailed, true},
		{StatusRefunded, true},
		{Status("pending"), false},
		{Status(""), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.s), func(t *testing.T) {
			if got := IsKnownStatus(tt.s); got != tt.want {
				t.Errorf("IsKnownStatus(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}
