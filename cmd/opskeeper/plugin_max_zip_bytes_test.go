package main

import "testing"

func TestParsePluginMaxZipBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},                     // empty -> 0 (fallback to default)
		{"   ", 0},                  // whitespace -> 0
		{"0", 0},                    // 0 -> 0
		{"-1", 0},                   // negative -> 0
		{"abc", 0},                  // invalid -> 0
		{"1024", 1024},              // raw bytes
		{"1KB", 1000},               // KB = 1000 bytes (SI)
		{"1KIB", 1024},              // KiB = 1024 bytes (binary)
		{"1MB", 1000 * 1000},        // MB = 10^6
		{"1MiB", 1024 * 1024},       // MiB = 2^20
		{"20MiB", 20 * 1024 * 1024}, // helm values.yaml 常见用法
		{"1GB", 1000 * 1000 * 1000},
		{"1GiB", 1024 * 1024 * 1024},
		{"100kb", 100 * 1000},           // case-insensitive
		{"  10MiB  ", 10 * 1024 * 1024}, // trim whitespace
		{"1.5MiB", 0},                   // decimal not supported -> 0
	}
	for _, tt := range tests {
		got := parsePluginMaxZipBytes(tt.in)
		if got != tt.want {
			t.Errorf("parsePluginMaxZipBytes(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
