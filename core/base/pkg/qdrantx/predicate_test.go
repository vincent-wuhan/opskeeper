package qdrantx

import "testing"

func TestMatchPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		must    map[string]any
		want    bool
	}{
		{
			name:    "empty must matches anything",
			payload: map[string]any{"tenant_id": "t1"},
			must:    nil,
			want:    true,
		},
		{
			name:    "scalar equality",
			payload: map[string]any{"tenant_id": "t1"},
			must:    map[string]any{"tenant_id": "t1"},
			want:    true,
		},
		{
			name:    "scalar mismatch",
			payload: map[string]any{"tenant_id": "t1"},
			must:    map[string]any{"tenant_id": "t2"},
			want:    false,
		},
		{
			name:    "missing key does not match",
			payload: map[string]any{"tenant_id": "t1"},
			must:    map[string]any{"visibility": "public"},
			want:    false,
		},
		{
			name:    "all clauses must hold",
			payload: map[string]any{"tenant_id": "t1", "visibility": "public"},
			must:    map[string]any{"tenant_id": "t1", "visibility": "private"},
			want:    false,
		},

		// Numbers survive a JSON round trip as float64 while the caller
		// still holds the uint64 it wrote, so comparison is by rendered
		// string rather than by type.
		{
			name:    "uint64 payload matches float64 clause",
			payload: map[string]any{"doc_id": float64(7)},
			must:    map[string]any{"doc_id": uint64(7)},
			want:    true,
		},
		{
			name:    "int64 payload matches int clause",
			payload: map[string]any{"pattern_id": float64(42)},
			must:    map[string]any{"pattern_id": 42},
			want:    true,
		},
		{
			name:    "numeric mismatch",
			payload: map[string]any{"doc_id": float64(7)},
			must:    map[string]any{"doc_id": uint64(8)},
			want:    false,
		},

		// []string is match.any.
		{
			name:    "any-of against a scalar payload",
			payload: map[string]any{"lang": "go"},
			must:    map[string]any{"lang": []string{"go", "rust"}},
			want:    true,
		},
		{
			name:    "any-of against a scalar payload, no hit",
			payload: map[string]any{"lang": "go"},
			must:    map[string]any{"lang": []string{"rust", "zig"}},
			want:    false,
		},
		{
			name:    "any-of against a []string payload",
			payload: map[string]any{"tags": []string{"a", "b"}},
			must:    map[string]any{"tags": []string{"b", "c"}},
			want:    true,
		},
		{
			name:    "any-of against a []any payload, as JSON decoding leaves it",
			payload: map[string]any{"tags": []any{"a", "b"}},
			must:    map[string]any{"tags": []string{"b"}},
			want:    true,
		},
		{
			name:    "any-of against a []any payload, no hit",
			payload: map[string]any{"tags": []any{"a", "b"}},
			must:    map[string]any{"tags": []string{"c"}},
			want:    false,
		},
		{
			name:    "empty any-of clause is dropped",
			payload: map[string]any{"lang": "go"},
			must:    map[string]any{"lang": []string{}},
			want:    true,
		},

		// A scalar clause also matches an array payload that holds it.
		{
			name:    "scalar clause matches array payload",
			payload: map[string]any{"tags": []string{"a", "b"}},
			must:    map[string]any{"tags": "b"},
			want:    true,
		},
		{
			name:    "scalar clause does not match unrelated array payload",
			payload: map[string]any{"tags": []string{"a", "b"}},
			must:    map[string]any{"tags": "c"},
			want:    false,
		},

		// PrefixMatch is match.text.
		{
			name:    "prefix on a scalar payload",
			payload: map[string]any{"path": "core/manager/biz"},
			must:    map[string]any{"path": PrefixMatch{Prefix: "core/"}},
			want:    true,
		},
		{
			name:    "prefix on a scalar payload, no hit",
			payload: map[string]any{"path": "web/src"},
			must:    map[string]any{"path": PrefixMatch{Prefix: "core/"}},
			want:    false,
		},
		{
			name:    "prefix on a []string payload",
			payload: map[string]any{"path_prefixes": []string{"web/src", "core/base"}},
			must:    map[string]any{"path_prefixes": PrefixMatch{Prefix: "core/"}},
			want:    true,
		},
		{
			name:    "prefix on a []any payload",
			payload: map[string]any{"path_prefixes": []any{"web/src", "core/base"}},
			must:    map[string]any{"path_prefixes": PrefixMatch{Prefix: "core/"}},
			want:    true,
		},
		{
			name:    "empty prefix clause is dropped",
			payload: map[string]any{"path": "core/manager/biz"},
			must:    map[string]any{"path": PrefixMatch{}},
			want:    true,
		},
		{
			name:    "a non-string payload has no prefix",
			payload: map[string]any{"path": 42},
			must:    map[string]any{"path": PrefixMatch{Prefix: "4"}},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchPayload(tc.payload, tc.must); got != tc.want {
				t.Fatalf("MatchPayload(%v, %v) = %v, want %v", tc.payload, tc.must, got, tc.want)
			}
		})
	}
}
