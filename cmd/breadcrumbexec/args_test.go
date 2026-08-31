package main

import (
	"reflect"
	"testing"
)

func TestNormalizeExecArgs(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []string
		want []string
	}{
		{name: "empty"},
		{name: "target", in: []string{"/bin/true"}, want: []string{"/bin/true"}},
		{name: "separator", in: []string{"--", "/bin/true", "arg"}, want: []string{"/bin/true", "arg"}},
		{name: "separator only", in: []string{"--"}, want: []string{}},
		{name: "later separator is argument", in: []string{"/bin/echo", "--", "arg"}, want: []string{"/bin/echo", "--", "arg"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeExecArgs(tt.in)
			if tt.want == nil {
				tt.want = tt.in
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizeExecArgs(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
