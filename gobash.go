package gobash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/spf13/afero"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Shell is a reusable simulated bash environment. Its in-memory filesystem
// persists across calls to [Shell.Run]; each call starts with a fresh shell
// variable/function/cwd state. A Shell is safe for sequential use; it is not
// safe for concurrent Run calls.
type Shell struct {
	fs          afero.Fs
	cwd         string
	env         []string
	maxOutput   int
	maxScript   int
	maxCommands int
	maxFSBytes  int64
	maxFSFiles  int
	timeout     time.Duration
	now         func() time.Time
}

// Option configures a [Shell].
type Option func(*Shell)

// WithFS sets the filesystem backing the shell. Defaults to a fresh in-memory
// filesystem ([afero.NewMemMapFs]).
func WithFS(fs afero.Fs) Option { return func(s *Shell) { s.fs = fs } }

// WithCwd sets the initial working directory (must be absolute).
func WithCwd(dir string) Option { return func(s *Shell) { s.cwd = dir } }

// WithEnv sets an environment variable visible to scripts (KEY=value form is
// not required; pass key and value separately).
func WithEnv(key, value string) Option {
	return func(s *Shell) { s.env = append(s.env, key+"="+value) }
}

// WithLimits bounds captured output, script source, and external command
// invocations for each Run. Non-positive values retain the safe defaults.
func WithLimits(maxOutput, maxScript, maxCommands int) Option {
	return func(s *Shell) {
		if maxOutput > 0 {
			s.maxOutput = maxOutput
		}
		if maxScript > 0 {
			s.maxScript = maxScript
		}
		if maxCommands > 0 {
			s.maxCommands = maxCommands
		}
	}
}

// WithFSQuota bounds the aggregate bytes and regular-file count in the virtual
// filesystem. Non-positive values retain the safe defaults.
func WithFSQuota(maxBytes int64, maxFiles int) Option {
	return func(s *Shell) {
		if maxBytes > 0 {
			s.maxFSBytes = maxBytes
		}
		if maxFiles > 0 {
			s.maxFSFiles = maxFiles
		}
	}
}

// WithTimeout bounds each Run or RunIO invocation. The default is five
// seconds. Callers may still provide an earlier deadline through the context.
func WithTimeout(timeout time.Duration) Option {
	return func(s *Shell) {
		if timeout > 0 {
			s.timeout = timeout
		}
	}
}

// WithNow supplies the clock used by time-aware commands such as date. It is
// primarily useful for deterministic tests.
func WithNow(now func() time.Time) Option {
	return func(s *Shell) {
		if now != nil {
			s.now = now
		}
	}
}

// New creates a Shell. By default it uses a fresh in-memory filesystem rooted
// at "/" with a virtual working directory of "/tmp" and no host environment
// leakage.
func New(opts ...Option) *Shell {
	s := &Shell{
		fs:  afero.NewMemMapFs(),
		cwd: "/tmp",
		env: []string{
			"BASH_COMPAT=" + BashCompatibility,
			"BASH_VERSION=" + BashVersion,
			"GOBASH_RUNTIME=mvdan-sh",
			"GOBASH_VERSION=" + Version,
			"HOME=/tmp",
			"LOGNAME=agent",
			"PATH=/gobash/bin",
			"PWD=/tmp",
			"SHELL=bash",
			"USER=agent",
		},
		maxOutput:   64 << 10,
		maxScript:   256 << 10,
		maxCommands: 256,
		maxFSBytes:  64 << 20,
		maxFSFiles:  1024,
		timeout:     5 * time.Second,
		now:         time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	// Keep the conventional device directory visible to file-oriented
	// utilities. /dev/null itself is implemented by virtualDeviceFS and is not
	// quota-counted storage.
	_ = s.fs.MkdirAll("/dev", 0o755)
	s.fs = newQuotaFS(s.fs, s.maxFSBytes, s.maxFSFiles)
	s.fs = newVirtualDeviceFS(s.fs)
	s.cwd = path.Clean(s.cwd)
	_ = s.fs.MkdirAll(s.cwd, 0o755)
	_ = s.fs.MkdirAll("/tmp", 0o700)
	return s
}

// FS exposes the underlying virtual filesystem, e.g. to seed files before a run
// or inspect results afterwards.
func (s *Shell) FS() afero.Fs { return s.fs }

// Result is the outcome of a captured [Shell.Run].
type Result struct {
	Stdout          string
	Stderr          string
	ExitCode        int
	Truncated       bool
	StdoutTruncated bool
	StderrTruncated bool
	Shell           string
	Runtime         string
	RuntimeVersion  string
	GoBashVersion   string
	BashVersion     string
	Cwd             string
	TimeoutMS       int64
}

// Run executes script, capturing stdout and stderr into the returned Result.
// The returned error is non-nil only for interpreter/parse failures, not for a
// non-zero exit status (which is reported via Result.ExitCode).
func (s *Shell) Run(ctx context.Context, script string) (Result, error) {
	return s.RunInput(ctx, script, strings.NewReader(""))
}

// RunInput executes script with the supplied stdin, capturing stdout and
// stderr into the returned Result. It is useful for hosts that want to pass a
// structured value into a shell pipeline without first writing a VFS file.
// The configured execution timeout cannot interrupt a Reader blocked inside
// its own Read method; callers that need a strict wall-clock bound must pass a
// finite or context-aware Reader. Code Mode uses an in-memory bytes.Reader.
func (s *Shell) RunInput(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	stdout := newCaptureBuffer(s.maxOutput)
	stderr := newCaptureBuffer(s.maxOutput)
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	code, err := s.RunIO(ctx, script, stdin, stdout, stderr)
	return Result{
		Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code,
		Truncated:       stdout.truncated || stderr.truncated,
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
		Shell: "bash", Runtime: Runtime, RuntimeVersion: RuntimeVersion,
		GoBashVersion: Version, BashVersion: BashVersion, Cwd: s.cwd,
		TimeoutMS: s.timeout.Milliseconds(),
	}, err
}

type captureBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func newCaptureBuffer(limit int) *captureBuffer { return &captureBuffer{limit: limit} }

func (b *captureBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

func (b *captureBuffer) WriteString(s string) (int, error) { return b.Write([]byte(s)) }

// errOutputClosed is returned to a background job that writes after its run
// has finished.
var errOutputClosed = errors.New("gobash: run finished; output is closed")

// outputGate serialises every write a single run makes to its stdout and
// stderr. One mutex covers both streams because callers may pass the same
// writer for each. After close, writes fail instead of reaching the caller.
type outputGate struct {
	mu     sync.Mutex
	closed bool
}

func (g *outputGate) wrap(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return &gatedWriter{gate: g, w: w}
}

// close waits for any in-flight write and rejects later ones. Taking the
// mutex also orders every accepted write before the caller's next read.
func (g *outputGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

type gatedWriter struct {
	gate *outputGate
	w    io.Writer
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.gate.mu.Lock()
	defer w.gate.mu.Unlock()
	if w.gate.closed {
		return 0, errOutputClosed
	}
	return w.w.Write(p)
}

// RunIO executes script with explicit stdin/stdout/stderr streams and returns
// the exit code. The error is non-nil only for parse/interpreter failures.
func (s *Shell) RunIO(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, runErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			exitCode = 2
			runErr = fmt.Errorf("gobash: interpreter panic: %v", recovered)
		}
	}()
	if len(script) > s.maxScript {
		return 2, fmt.Errorf("gobash: script exceeds %d-byte limit", s.maxScript)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	state := &runState{maxCommands: s.maxCommands, cancel: cancel}
	ctx = context.WithValue(ctx, runStateKey{}, state)
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(script), "bash")
	if err != nil {
		return 2, err
	}
	rewriteExplicitBuiltinPrintf(file)
	rewriteTypeAll(file)
	rewriteArrayLengths(file)
	guardInvalidOctalArithmetic(file)
	var processSubstitution bool
	syntax.Walk(file, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.ProcSubst); ok {
			processSubstitution = true
			return false
		}
		return true
	})
	if processSubstitution {
		return 2, fmt.Errorf("bash: process substitution is unavailable in the virtual filesystem; use a pipeline or temporary VFS file")
	}
	prelude, err := syntax.NewParser().Parse(strings.NewReader(commandPrelude()), "gobash-prelude")
	if err != nil {
		return 2, fmt.Errorf("gobash: parse command prelude: %w", err)
	}
	file.Stmts = append(prelude.Stmts, file.Stmts...)
	// Pipeline stages and background jobs run on their own goroutines and all
	// share these writers, so every write is serialised. The gate also stops a
	// still-running background job from writing after RunIO has returned and
	// the caller owns the writers again.
	output := &outputGate{}
	defer output.close()
	runner, err := interp.New(
		interp.StdIO(stdin, output.wrap(stdout), output.wrap(stderr)),
		// interp.Dir validates against the host filesystem. Bootstrap from a
		// host directory and then point the runner at the virtual cwd.
		interp.Dir("/"),
		interp.Env(expand.ListEnviron(s.env...)),
		interp.ExecHandlers(s.execMiddleware),
		interp.OpenHandler(s.openHandler),
		interp.StatHandler(s.statHandler),
		interp.ReadDirHandler2(s.readDirHandler),
		interp.AccessHandler(s.accessHandler),
	)
	if err != nil {
		return 2, err
	}
	runner.Dir = s.cwd
	runErr = runner.Run(ctx, file)
	// Close before the diagnostics below write to stderr directly.
	output.close()
	if arithmeticErr := state.firstArithmeticError(); arithmeticErr != nil {
		_, _ = fmt.Fprintf(stderr, "bash: %v\n", arithmeticErr)
		return 1, nil
	}
	if writeErr := state.firstWriteError(); writeErr != nil {
		_, _ = fmt.Fprintf(stderr, "bash: write error: %v\n", writeErr)
		return 1, nil
	}
	if runErr == nil {
		return 0, nil
	}
	var status interp.ExitStatus
	if errors.As(runErr, &status) {
		return int(status), nil
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		_, _ = fmt.Fprintln(stderr, "bash: execution timed out")
		return 124, nil
	}
	if errors.Is(runErr, context.Canceled) {
		_, _ = fmt.Fprintln(stderr, "bash: execution canceled")
		return 130, nil
	}
	if strings.Contains(runErr.Error(), "gobash: virtual filesystem quota exceeded") {
		_, _ = fmt.Fprintf(stderr, "bash: %v\n", runErr)
		return 1, nil
	}
	return 1, runErr
}
