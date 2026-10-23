package gobash

import (
	"context"
	"strings"
	"testing"
)

// Each construct used to run past silently: PIPESTATUS expanded empty with
// exit 0, and the flags printed "invalid option" and returned 2 while the next
// line ran on empty input. They are now refused before anything runs, naming
// the alternative.
func TestSilentlyUnsupportedConstructsAreRefused(t *testing.T) {
	for script, want := range map[string]string{
		`false | true; echo "${PIPESTATUS[*]}"; echo ran`: "PIPESTATUS is unavailable",
		`false | true; s=${PIPESTATUS[0]}; echo ran`:      "PIPESTATUS is unavailable",
		`printf 'a,b' | read -d , x; echo ran`:            "read -d is unavailable",
		`read -rn 2 x <<< abc; echo ran`:                  "read -n is unavailable",
		`read -t 1 x <<< hi; echo ran`:                    "read -t is unavailable",
		`set -eE; echo ran`:                               "set -E is unavailable",
		`shopt -q nullglob && echo on; echo ran`:          "shopt -q is unavailable",
		`shopt -p; echo ran`:                              "shopt -p is unavailable",
		`f() { read -N 3 x; }; echo before; f; echo ran`:  "read -N is unavailable",
	} {
		result, err := New().Run(context.Background(), script)
		if err == nil || !strings.Contains(err.Error(), want) || result.ExitCode != 2 {
			t.Errorf("%q: want refusal %q with exit 2, got exit %d err %v", script, want, result.ExitCode, err)
		}
		if strings.Contains(result.Stdout, "ran") || strings.Contains(result.Stdout, "before") {
			t.Errorf("%q: script ran before being refused: %q", script, result.Stdout)
		}
	}
}

// The supported forms of the same builtins keep working.
func TestSupportedFormsOfRefusedBuiltinsStillRun(t *testing.T) {
	for script, want := range map[string]string{
		`read -r x <<< hello; echo "$x"`:                 "hello\n",
		`read -r name date <<< "a b"; echo "$name$date"`: "ab\n",
		`set -o pipefail; false | true; echo "rc=$?"`:    "rc=1\n",
		`set -eu; echo ok`:                               "ok\n",
		`shopt -s nullglob; echo ok`:                     "ok\n",
		`echo "-d -n -E -q are only words here"`:         "-d -n -E -q are only words here\n",
		`printf '%s\n' read -d`:                          "read\n-d\n",
		`echo PIPESTATUS`:                                "PIPESTATUS\n",
	} {
		result, err := New().Run(context.Background(), script)
		if err != nil || result.Stdout != want {
			t.Errorf("%q: want %q, got %q err %v", script, want, result.Stdout, err)
		}
	}
}
