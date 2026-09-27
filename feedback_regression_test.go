package gobash

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFeedbackCommandDiscoveryAndRuntimeIdentity(t *testing.T) {
	commands := []string{
		"bash", "sh", "ls", "cat", "head", "tail", "sed", "awk", "grep", "rg",
		"sort", "uniq", "wc", "tr", "cut", "find", "xargs", "seq", "date",
		"env", "printenv", "jq", "mkdir", "cp", "mv", "rm", "mktemp", "pwd",
	}
	script := `for name in ` + strings.Join(commands, " ") + `; do command -v "$name" || exit 10; done
printf 'shell=%s cwd=%s user=%s\n' "$0" "$PWD" "$(whoami)"`
	result := run(t, New(), script)
	if result.ExitCode != 0 {
		t.Fatalf("discovery failed: %+v", result)
	}
	for _, command := range commands {
		if !strings.Contains(result.Stdout, command+"\n") {
			t.Fatalf("%s not discoverable in %q", command, result.Stdout)
		}
	}
	if !strings.HasSuffix(result.Stdout, "shell=bash cwd=/tmp user=agent\n") {
		t.Fatalf("identity mismatch: %q", result.Stdout)
	}
	missing := run(t, New(), `command -v zsh`)
	if missing.ExitCode != 1 || missing.Stdout != "" {
		t.Fatalf("unsupported zsh should remain unavailable: %+v", missing)
	}
}

func TestShDiscoveryAndNestedShellBoundary(t *testing.T) {
	result := run(t, New(), `command -v sh
printf 'status=%s\n' "$?"
type -a sh`)
	if result.ExitCode != 0 || result.Stdout != "sh\nstatus=0\nsh is a function\n" || result.Stderr != "" {
		t.Fatalf("sh discovery: %+v", result)
	}
	result = run(t, New(), `sh -c 'echo unsafe'`)
	if result.ExitCode != 2 || !strings.Contains(result.Stderr, "nested shells are unavailable") {
		t.Fatalf("nested sh: %+v", result)
	}
}

func TestResultMetadataStreamsAndTimeout(t *testing.T) {
	result := run(t, New(WithTimeout(20*time.Millisecond)), `printf stdout; printf stderr >&2`)
	if result.Stdout != "stdout" || result.Stderr != "stderr" {
		t.Fatalf("streams were not separated: %+v", result)
	}
	if result.Shell != "bash" || result.Runtime != "go-bash/mvdan-sh" || result.Cwd != "/tmp" || result.TimeoutMS != 20 {
		t.Fatalf("metadata mismatch: %+v", result)
	}
	timedOut := run(t, New(WithTimeout(time.Millisecond)), `while true; do :; done`)
	if timedOut.ExitCode != 124 || !strings.Contains(timedOut.Stderr, "timed out") {
		t.Fatalf("timeout mismatch: %+v", timedOut)
	}
}

func TestOutputTruncationIsPerStream(t *testing.T) {
	result := run(t, New(WithLimits(4, 256, 10)), `printf 12345; printf abcde >&2`)
	if result.Stdout != "1234" || result.Stderr != "abcd" || !result.Truncated ||
		!result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("per-stream truncation mismatch: %+v", result)
	}
}

func TestTimeoutIsBoundedAndLeavesVFSUsable(t *testing.T) {
	sh := New(WithTimeout(10 * time.Millisecond))
	result := run(t, sh, `trap 'printf cleaned > /tmp/cleanup' EXIT; while true; do :; done`)
	if result.ExitCode != 124 || !strings.Contains(result.Stderr, "timed out") {
		t.Fatalf("timeout mismatch: %+v", result)
	}
	// Context cancellation is an abrupt interruption. mvdan/sh invokes EXIT
	// traps with the already-canceled context, so cleanup commands are not
	// guaranteed to run. The persistent VFS must nevertheless remain usable.
	result, err := sh.Run(context.Background(), `printf usable > /tmp/after-timeout; cat /tmp/after-timeout`)
	if err != nil || result.ExitCode != 0 || result.Stdout != "usable" {
		t.Fatalf("VFS was unusable after timeout: %+v err=%v", result, err)
	}
}

func TestQuotingAndBinarySafeStreams(t *testing.T) {
	result := run(t, New(), `set -- 'space value' $'tab\tvalue' '雪' '' $'line1\nline2' '*' '[abc]'
for arg in "$@"; do printf '<%q>\n' "$arg"; done
printf '\0\377A' > /tmp/binary
cat /tmp/binary`)
	wantPrefix := "<space\\ value>\n<$'tab\\tvalue'>\n<雪>\n<''>\n<$'line1\\nline2'>\n<\\*>\n<\\[abc\\]>\n"
	if result.ExitCode != 0 || result.Stderr != "" || !strings.HasPrefix(result.Stdout, wantPrefix) {
		t.Fatalf("quoting mismatch: %+v want prefix %q", result, wantPrefix)
	}
	if got := []byte(strings.TrimPrefix(result.Stdout, wantPrefix)); len(got) != 3 || got[0] != 0 || got[1] != 0xff || got[2] != 'A' {
		t.Fatalf("binary stream mismatch: %v", got)
	}
}

func TestLargeJSONCanBeReducedWithoutReturningPayload(t *testing.T) {
	sh := New(WithLimits(64<<10, 256<<10, 256))
	padding := strings.Repeat("x", 256<<10)
	result, err := sh.RunInput(context.Background(), `jq -c '{bytes:(.padding|length),id:.items[1].id}'`, strings.NewReader(`{"items":[{"id":"a"},{"id":"b"}],"padding":"`+padding+`"}`))
	if err != nil || result.ExitCode != 0 || result.Stdout != `{"bytes":262144,"id":"b"}`+"\n" || result.Truncated {
		t.Fatalf("large JSON reduction mismatch: %+v err=%v", result, err)
	}
}

func TestAgentFileInspectionFlags(t *testing.T) {
	result := run(t, New(), `printf '0123456789abcdef\nsecond\n' >/results.json
ls -l /results.json
head -c 16 /results.json
printf '\n'
tail -c 7 /results.json`)
	if result.ExitCode != 0 || result.Stderr != "" {
		t.Fatalf("file inspection flags failed: %+v", result)
	}
	lines := strings.Split(result.Stdout, "\n")
	if len(lines) < 4 || !strings.Contains(lines[0], " agent agent 24 ") || !strings.HasSuffix(lines[0], " /results.json") {
		t.Fatalf("ls -l output = %q", result.Stdout)
	}
	if lines[1] != "0123456789abcdef" || lines[2] != "second" {
		t.Fatalf("byte head/tail output = %q", result.Stdout)
	}
}

func TestAgentFileInspectionHelp(t *testing.T) {
	for _, command := range []string{"ls", "head", "tail"} {
		result := run(t, New(), command+" --help")
		if result.ExitCode != 0 || result.Stderr != "" || !strings.HasPrefix(result.Stdout, "usage: "+command+" ") {
			t.Fatalf("%s --help: %+v", command, result)
		}
	}
}
