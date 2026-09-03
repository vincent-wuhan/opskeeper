package setting

import (
	"reflect"
	"testing"
)

func TestDedupeStrings(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		// The out-of-box bug: the OpenAI catalog was seeded with the
		// configured model (defaulting to gpt-4o) plus a base list that
		// already contained gpt-4o → two gpt-4o rows in the picker.
		{"out-of-box openai dup", []string{"gpt-4o", "gpt-4o", "gpt-4-turbo"}, []string{"gpt-4o", "gpt-4-turbo"}},
		{"empty entries dropped", []string{"", "a", "", "b"}, []string{"a", "b"}},
		{"order preserved, later dups dropped", []string{"b", "a", "b", "c", "a"}, []string{"b", "a", "c"}},
		{"nil -> empty", nil, []string{}},
		{"no dups untouched", []string{"x", "y"}, []string{"x", "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupeStrings(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dedupeStrings(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
