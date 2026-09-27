package gobash

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// mvdan/sh runs every pipeline stage except the last, and every background
// job, on its own goroutine. All of them share the runner's stdout and stderr
// writers, so these scripts make several goroutines write to the same writer
// at once. They are only meaningful under `go test -race`; without the race
// detector they merely check that nothing is lost or panics.
var concurrentWriterScripts = []struct {
	name   string
	script string
}{
	// Issue #24: sort rejects -k0 and exits without reading stdin, so printf's
	// write can fail on the closed pipe while sort reports its own error.
	{"printf into failing sort", `printf 'a\n' | sort -k0`},
	{"repeated failing sorts", `for i in 1 2 3 4 5 6 7 8; do seq 1 2000 | sort -k0; done`},
	{"every stage fails", `cat /missing-1 | cat /missing-2 | cat /missing-3 | cat /missing-4`},
	{"stderr from both sides", `{ seq 1 500 >&2; echo left; } | { cat; seq 1 500 >&2; }`},
	{"background stderr", `seq 1 2000 >&2 & seq 1 2000 >&2; wait`},
	{"background stdout", `seq 1 2000 & seq 1 2000; wait`},
	{"pipe all", `{ seq 1 500; cat /missing; } |& cat & cat /missing-2; wait`},
}

func TestConcurrentPipelineWritersCaptured(t *testing.T) {
	for _, tc := range concurrentWriterScripts {
		t.Run(tc.name, func(t *testing.T) {
			result, err := New().Run(context.Background(), tc.script)
			if err != nil {
				t.Fatalf("interpreter error: %v", err)
			}
			if result.Truncated {
				t.Fatalf("unexpected truncation: %+v", result)
			}
		})
	}
}

// RunIO callers may pass one writer for both streams (an interleaved
// transcript). go-bash must serialise writes to it.
func TestConcurrentPipelineWritersSharedRunIOWriter(t *testing.T) {
	for _, tc := range concurrentWriterScripts {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := New().RunIO(context.Background(), tc.script, strings.NewReader(""), &out, &out); err != nil {
				t.Fatalf("interpreter error: %v", err)
			}
			_ = out.String()
		})
	}
}

func TestIssue24SortRejectsZeroFieldInPipeline(t *testing.T) {
	result := run(t, New(), `printf 'a\n' | sort -k0`)
	if result.ExitCode != 2 || result.Stdout != "" ||
		!strings.Contains(result.Stderr, "sort: field number is zero: invalid field specification '0'") {
		t.Fatalf("got %+v", result)
	}
}

// A background job may still be running when RunIO returns. It must not write
// into the caller's writer after that point, because the caller now owns it.
func TestNoWritesAfterRunIOReturns(t *testing.T) {
	for range 20 {
		var stdout, stderr bytes.Buffer
		if _, err := New().RunIO(context.Background(), `seq 1 100000 & seq 1 100000 >&2 &`, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("interpreter error: %v", err)
		}
		// Reading the buffers races with any late write under -race.
		_, _ = stdout.Len(), stderr.Len()
	}
}

func init() {
	registerInternal("gobash_test_panic", func(context.Context, *Env) int { panic("boom") })
}

// A command that panics in a non-final pipeline stage runs on a goroutine
// without a recover of its own; it must not take the embedding process down.
func TestPanickingPipelineStageIsContained(t *testing.T) {
	for _, script := range []string{
		`gobash_test_panic | cat`,
		`echo hi | gobash_test_panic`,
		`gobash_test_panic & wait`,
	} {
		result := run(t, New(), script)
		if !strings.Contains(result.Stderr, "gobash_test_panic: internal error: boom") {
			t.Fatalf("%s: got %+v", script, result)
		}
	}
}

// A writer whose pipeline reader has exited behaves as if killed by SIGPIPE:
// no "broken pipe" diagnostic, and status 141 under pipefail.
func TestBrokenPipeWriterIsSilent(t *testing.T) {
	for range 20 {
		result := run(t, New(), `seq 1 90000 | head -n 1`)
		if result.Stdout != "1\n" || result.Stderr != "" || result.ExitCode != 0 {
			t.Fatalf("got %+v", result)
		}
		result = run(t, New(), `printf 'a\n' | sort -k0`)
		if result.Stderr != "sort: field number is zero: invalid field specification '0'\n" || result.ExitCode != 2 {
			t.Fatalf("got %+v", result)
		}
	}
	result := run(t, New(), `set -o pipefail; seq 1 90000 | { read -r line; echo "$line"; }; echo "status=$?"`)
	if result.Stdout != "1\nstatus=141\n" || result.Stderr != "" {
		t.Fatalf("got %+v", result)
	}
}
