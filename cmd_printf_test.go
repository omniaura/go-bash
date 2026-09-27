package gobash

import (
	"strings"
	"testing"
)

// TestPrintfMatchesGNUBash replays printfGNUCases, recorded from GNU bash's
// printf builtin. go-bash has no locale: \u and \U always produce UTF-8, so
// those cases were recorded under C.UTF-8 (under LC_ALL=C bash prints the
// escape text, such as é, instead). Every other case is locale
// independent and was recorded under LC_ALL=C.
func TestPrintfMatchesGNUBash(t *testing.T) {
	for _, tc := range printfGNUCases {
		t.Run(tc.name, func(t *testing.T) {
			result := run(t, New(), tc.script)
			// bash prefixes builtin diagnostics with its own location.
			wantStderr := strings.ReplaceAll(tc.wantStderr, "bash: line 1: ", "")
			if result.Stdout != tc.want || result.ExitCode != tc.wantExit || result.Stderr != wantStderr {
				t.Fatalf("script: %s\nstdout=%q exit=%d stderr=%q\nwant   %q exit=%d stderr=%q",
					tc.script, result.Stdout, result.ExitCode, result.Stderr, tc.want, tc.wantExit, wantStderr)
			}
		})
	}
}

func TestPrintfEscapesThroughShellForms(t *testing.T) {
	result := run(t, New(), `printf -v v '\303\251%b' '\101'; printf '%s|' "$v"; builtin printf '\x41'; command printf '\0101'`)
	if want := "\xc3\xa9A|A\x081"; result.Stdout != want || result.ExitCode != 0 {
		t.Fatalf("got %+v, want stdout %q", result, want)
	}
}
