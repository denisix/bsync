package main

import (
	"net"
	"reflect"
	"strconv"
	"testing"
)

func TestPickHighPort(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		p := pickHighPort()
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("pickHighPort returned non-numeric %q", p)
		}
		if n <= 10000 || n > 65535 {
			t.Errorf("port %d out of expected range (>10000)", n)
		}
		// The returned port must be bindable (i.e. actually free).
		l, err := net.Listen("tcp", "127.0.0.1:"+p)
		if err != nil {
			t.Errorf("port %s not bindable: %v", p, err)
			continue
		}
		l.Close()
		seen[p] = true
	}
	if len(seen) < 2 {
		t.Error("pickHighPort returned the same port every time")
	}
}

func TestParseExcludePatterns(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"single", "*.log", []string{"*.log"}},
		{"whitespace trimmed around each element", " *.log , *.tmp,node_modules ", []string{"*.log", "*.tmp", "node_modules"}},
		{"leading/trailing/doubled commas dropped", ",*.log,,*.tmp,", []string{"*.log", "*.tmp"}},
		{"substring glob preserved as-is", "*some_file_name*", []string{"*some_file_name*"}},
		{"all whitespace", "   ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseExcludePatterns(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseExcludePatterns(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}
