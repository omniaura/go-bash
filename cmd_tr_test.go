package gobash

import (
	"strings"
	"testing"
)

// TestTrMatchesGNU replays trGNUCases, recorded from GNU coreutils tr with
// LC_ALL=C: stdout, stderr and the exit status must all match.
func TestTrMatchesGNU(t *testing.T) {
	for _, tc := range trGNUCases {
		t.Run(tc.name, func(t *testing.T) {
			result := run(t, New(), tc.script)
			if result.Stdout != tc.want || result.ExitCode != tc.wantExit || result.Stderr != tc.wantStderr {
				t.Fatalf("script: %s\nstdout=%q exit=%d stderr=%q\nwant   %q exit=%d stderr=%q",
					tc.script, result.Stdout, result.ExitCode, result.Stderr, tc.want, tc.wantExit, tc.wantStderr)
			}
		})
	}
}

func TestTrStreamsLargeInput(t *testing.T) {
	sh := New(WithLimits(1<<20, 0, 0))
	result, err := sh.RunInput(t.Context(), `tr -s 'a' 'b' | wc -c`, strings.NewReader(strings.Repeat("a", 3<<20)+"\n"))
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "2" {
		t.Fatalf("got %+v err=%v", result, err)
	}
}
