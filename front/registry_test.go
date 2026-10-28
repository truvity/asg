package front

import (
	"fmt"
	"strings"
	"testing"
)

func TestRegistryValidate(t *testing.T) {
	t.Parallel()

	many := map[string]int{}
	for i := 0; i <= MaxListeners; i++ {
		many[fmt.Sprintf("svc-%d", i)] = 1000 + i
	}

	tests := []struct {
		name string
		reg  Registry
		want []string // substrings; empty means valid
	}{
		{"empty is valid", Registry{}, nil},
		{"ok", Registry{
			Listeners: map[string]int{"buildkit-arm64": 1234, "buildkit-amd64": 1235},
			Names:     map[string]string{"arm64.build": "buildkit-arm64"},
		}, nil},
		{"exactly the quota", Registry{Listeners: fill(MaxListeners)}, nil},
		{"over the quota", Registry{Listeners: many}, []string{"51 listeners: an NLB takes at most 50"}},
		{"bad names", Registry{Listeners: map[string]int{"-x": 1, "a_b": 2, strings.Repeat("a", 33): 3}},
			[]string{`listener "-x"`, `listener "a_b"`, "listener \"" + strings.Repeat("a", 33)}},
		{"port range", Registry{Listeners: map[string]int{"a": 0, "b": 65536}},
			[]string{`listener "a": port 0`, `listener "b": port 65536`}},
		{"duplicate port", Registry{Listeners: map[string]int{"a": 80, "b": 80}},
			[]string{"port 80 is held by [a b]"}},
		{"bad host", Registry{Listeners: map[string]int{"a": 80}, Names: map[string]string{"Bad.Host": "a"}},
			[]string{`name "Bad.Host"`}},
		{"name without listener", Registry{Listeners: map[string]int{"a": 80}, Names: map[string]string{"x.y": "b"}},
			[]string{`name "x.y" serves listener "b", which is not in listeners`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.reg.Validate()
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("want errors %v, got none", tt.want)
			}

			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func fill(n int) map[string]int {
	m := make(map[string]int, n)
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("svc-%d", i)] = 1000 + i
	}

	return m
}
