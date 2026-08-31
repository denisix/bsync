package main

import (
	"net"
	"os"
	"reflect"
	"regexp"
	rtdebug "runtime/debug"
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

// --- directory-only flag validation ---

// TestDirOnlyFlagsRejectedInSingleFileMode drives off the same list main uses,
// so a flag added to dirOnlyFlags is automatically covered and one added
// WITHOUT being listed shows up as an uncovered flag below.
func TestFirstDirOnlyFlagUsed(t *testing.T) {
	if got := firstDirOnlyFlagUsed(map[string]bool{}); got != "" {
		t.Errorf("no flags passed: got %q, want \"\"", got)
	}
	// Flags that are NOT directory-only must never trip it -- notably -b/-w,
	// and -q, which every transfer may pass.
	for _, name := range []string{"f", "r", "b", "w", "q", "n", "e", "t", "L", "p"} {
		if got := firstDirOnlyFlagUsed(map[string]bool{name: true}); got != "" {
			t.Errorf("-%s wrongly treated as directory-only (got %q)", name, got)
		}
	}
	for _, name := range dirOnlyFlags {
		if got := firstDirOnlyFlagUsed(map[string]bool{name: true}); got != name {
			t.Errorf("-%s: got %q, want %q", name, got, name)
		}
	}
}

// TestDirOnlyFlagsCoverEveryDeclaredOne guards the list itself: every flag
// whose help text says "directory mode only" must be in dirOnlyFlags, or the
// validation silently skips it.
func TestDirOnlyFlagsCoverEveryDeclaredOne(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`flag\.[A-Za-z]+Var\(&[a-zA-Z]+, "([a-z-]+)", [^,]+, "directory mode only`)
	listed := map[string]bool{}
	for _, name := range dirOnlyFlags {
		listed[name] = true
	}
	for _, m := range declared.FindAllStringSubmatch(string(src), -1) {
		if !listed[m[1]] {
			t.Errorf("-%s declares itself directory-mode-only but is missing from dirOnlyFlags, "+
				"so it is never rejected in single-file mode", m[1])
		}
	}
}

// TestMemLimitSetsRuntimeSoftLimit pins the mechanism -memlimit relies on:
// debug.SetMemoryLimit takes effect immediately and is read back via the
// same call with a negative argument (the documented no-op query form).
// main() itself isn't unit-testable here (it parses the global
// flag.CommandLine and calls os.Exit on certain paths), so this exercises
// the runtime primitive directly rather than flag parsing.
func TestMemLimitSetsRuntimeSoftLimit(t *testing.T) {
	original := rtdebug.SetMemoryLimit(-1) // query without changing anything
	t.Cleanup(func() { rtdebug.SetMemoryLimit(original) })

	const memLimitMB = 256
	rtdebug.SetMemoryLimit(int64(memLimitMB) << 20)
	if got := rtdebug.SetMemoryLimit(-1); got != int64(memLimitMB)<<20 {
		t.Errorf("SetMemoryLimit readback = %d, want %d", got, int64(memLimitMB)<<20)
	}
}
