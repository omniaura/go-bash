package gobash

import (
	"strings"
	"testing"
)

// TestSortMatchesGNU replays scripts whose expected stdout and exit status
// were recorded from real bash + GNU coreutils sort (see
// cmd_sort_gnu_cases_test.go).
func TestSortMatchesGNU(t *testing.T) {
	for _, tc := range sortGNUCases {
		t.Run(tc.name, func(t *testing.T) {
			result := run(t, New(), tc.script)
			if result.Stdout != tc.want || result.ExitCode != tc.wantExit {
				t.Fatalf("script: %s\nstdout=%q exit=%d stderr=%q\nwant   %q exit=%d",
					tc.script, result.Stdout, result.ExitCode, result.Stderr, tc.want, tc.wantExit)
			}
		})
	}
}

func TestSortDiagnostics(t *testing.T) {
	tests := []struct {
		script string
		stderr string
	}{
		{`printf '' | sort -k0`, "sort: field number is zero: invalid field specification '0'"},
		{`printf '' | sort -k1.0`, "sort: character offset is zero: invalid field specification '1.0'"},
		{`printf '' | sort -k2x`, "sort: stray character in field spec: invalid field specification '2x'"},
		{`printf '' | sort -kx`, "sort: invalid number at field start: invalid count at start of 'x'"},
		{`printf '' | sort -nV`, "sort: options '-nV' are incompatible"},
		{`printf '' | sort -k1,1dn`, "sort: options '-dn' are incompatible"},
		{`printf '' | sort -t ab`, "sort: multi-character tab 'ab'"},
		{`printf '' | sort -t ''`, "sort: empty tab"},
		{`printf '' | sort -t, -t:`, "sort: incompatible tabs"},
		{`printf '' | sort -o a -o b`, "sort: multiple output files specified"},
		{`sort -k`, "sort: option requires an argument -- 'k'"},
		{`sort -z`, "sort: unsupported option -- z"},
		{`sort -g`, "sort: unsupported option -- g"},
		{`sort -R`, "sort: unsupported option -- R"},
		{`sort -c`, "sort: unsupported option -- c"},
		{`sort --random-sort`, "sort: unsupported option '--random-sort'"},
		{`sort --sort=random`, "sort: unsupported argument 'random' for '--sort'"},
	}
	for _, tc := range tests {
		t.Run(tc.script, func(t *testing.T) {
			result := run(t, New(), tc.script)
			if result.ExitCode != 2 || result.Stdout != "" || !strings.Contains(result.Stderr, tc.stderr) {
				t.Fatalf("result=%+v want exit 2 and stderr containing %q", result, tc.stderr)
			}
		})
	}
}

func TestSortHelpListsSupportedForms(t *testing.T) {
	result := run(t, New(), `sort --help`)
	if result.ExitCode != 0 || result.Stderr != "" || !strings.HasPrefix(result.Stdout, "usage: sort ") {
		t.Fatalf("help: %+v", result)
	}
	for _, form := range []string{"-k, --key=POS1[,POS2]", "-t, --field-separator=SEP", "-n, --numeric-sort",
		"-h, --human-numeric-sort", "-V, --version-sort", "-f, --ignore-case", "-s, --stable", "-o, --output=FILE"} {
		if !strings.Contains(result.Stdout, form) {
			t.Fatalf("help does not list %q:\n%s", form, result.Stdout)
		}
	}
}

func TestSortMissingFileWritesNoOutput(t *testing.T) {
	result := run(t, New(), `printf 'a\n' >/tmp/ok; sort /tmp/ok /tmp/missing`)
	if result.ExitCode != 2 || result.Stdout != "" || !strings.Contains(result.Stderr, "/tmp/missing") {
		t.Fatalf("missing file: %+v", result)
	}
}
