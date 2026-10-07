package dataguard

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		input   string
		want    Sensitivity
		wantErr bool
	}{
		{"Public", Public, false},
		{"Internal", Internal, false},
		{"Confidential", Confidential, false},
		{"Restricted", Restricted, false},
		{"TopSecret", TopSecret, false},
		{"public", "", true}, // 大小写敏感
		{"unknown", "", true},
		{"", "", true},
		{"  Public  ", Public, false}, // trim whitespace
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := Parse(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		s, other Sensitivity
		want     int
	}{
		{Public, Public, 0},
		{Public, Internal, -1}, // Public 更宽松
		{TopSecret, Public, 4}, // TopSecret 更敏感
		{Confidential, Internal, 1},
		{Restricted, Confidential, 1},
	}
	for _, tt := range tests {
		t.Run(string(tt.s)+"_vs_"+string(tt.other), func(t *testing.T) {
			if got := tt.s.Compare(tt.other); got != tt.want {
				t.Errorf("Compare(%v, %v) = %d, want %d", tt.s, tt.other, got, tt.want)
			}
		})
	}
}

func TestIsZeroTamper(t *testing.T) {
	tests := []struct {
		s    Sensitivity
		want bool
	}{
		{Public, false},
		{Internal, false},
		{Confidential, false},
		{Restricted, true},
		{TopSecret, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.s), func(t *testing.T) {
			if got := tt.s.IsZeroTamper(); got != tt.want {
				t.Errorf("IsZeroTamper(%v) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestIsValid(t *testing.T) {
	if !IsValid("Public") {
		t.Error("IsValid(Public) = false, want true")
	}
	if IsValid("invalid") {
		t.Error("IsValid(invalid) = true, want false")
	}
}
