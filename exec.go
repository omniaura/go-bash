package gobash

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

type runStateKey struct{}

type runState struct {
	commands      atomic.Int32
	maxCommands   int
	mu            sync.Mutex
	writeErr      error
	arithmeticErr error
	cancel        context.CancelFunc
}

func (s *runState) recordArithmeticError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.arithmeticErr == nil {
		s.arithmeticErr = err
		if s.cancel != nil {
			s.cancel()
		}
	}
	s.mu.Unlock()
}

func (s *runState) firstArithmeticError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.arithmeticErr
}

func (s *runState) recordWriteError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.writeErr == nil {
		s.writeErr = err
	}
	s.mu.Unlock()
}

func (s *runState) firstWriteError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErr
}

// execMiddleware dispatches each simple command to a registered Go builtin.
// There is deliberately no fall-through to the host operating system: an
// unknown command behaves like bash's "command not found" (exit code 127).
// This is what keeps the environment safe without an OS-level sandbox.
func (s *Shell) execMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		code := s.runCommandRecovered(ctx, args, hc)
		if code == 0 {
			return nil
		}
		return interp.ExitStatus(uint8(code))
	}
}

// runCommandRecovered contains a panicking command. Pipeline stages other
// than the last run on goroutines that mvdan/sh starts without a recover, so
// a panic there would otherwise terminate the embedding process rather than
// reach the recover in RunIO.
func (s *Shell) runCommandRecovered(ctx context.Context, args []string, hc interp.HandlerContext) (code int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "%s: internal error: %v\n", args[0], recovered)
			code = 2
		}
	}()
	return s.runCommand(ctx, args, hc)
}

func (s *Shell) runCommand(ctx context.Context, args []string, hc interp.HandlerContext) int {
	if state, _ := ctx.Value(runStateKey{}).(*runState); state != nil && !strings.HasPrefix(args[0], "gobash_") &&
		state.commands.Add(1) > int32(state.maxCommands) {
		_, _ = fmt.Fprintf(hc.Stderr, "gobash: command limit (%d) exceeded\n", state.maxCommands)
		return 124
	}
	if err := ctx.Err(); err != nil {
		return 124
	}
	fn, ok := lookup(args[0])
	if !ok {
		_, _ = fmt.Fprintf(hc.Stderr, "%s: command not found\n", args[0])
		return 127
	}
	runNested := func(nestedCtx context.Context, nestedArgs []string) int {
		return s.runShellCommand(nestedCtx, nestedArgs, nil, hc)
	}
	runNestedEnv := func(nestedCtx context.Context, nestedArgs, assignments []string) int {
		return s.runShellCommand(nestedCtx, nestedArgs, assignments, hc)
	}
	env := &Env{
		Args: args, Stdin: hc.Stdin, Stdout: hc.Stdout, Stderr: hc.Stderr,
		FS: s.fs, Dir: hc.Dir, Environ: exportedEnvironment(hc.Env),
		Now: s.now, RunCommand: runNested, RunCommandEnv: runNestedEnv,
	}
	return fn(ctx, env)
}

// runShellCommand safely re-enters the interpreter with one argv vector. The
// quoting prevents xargs input from becoming shell syntax, while using an
// interpreter runner lets commands such as printf resolve as shell builtins.
func (s *Shell) runShellCommand(ctx context.Context, args, assignments []string, hc interp.HandlerContext) int {
	if len(args) == 0 {
		return 0
	}
	quoted := make([]string, 0, len(assignments)+len(args))
	for _, pair := range assignments {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || !syntax.ValidName(name) {
			_, _ = fmt.Fprintf(hc.Stderr, "env: invalid assignment: %s\n", pair)
			return 2
		}
		quoted = append(quoted, name+"="+shellQuote(value))
	}
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(commandPrelude()+strings.Join(quoted, " ")), "xargs")
	if err != nil {
		_, _ = fmt.Fprintf(hc.Stderr, "xargs: parse command: %v\n", err)
		return 2
	}
	runner, err := interp.New(
		interp.StdIO(hc.Stdin, hc.Stdout, hc.Stderr),
		// interp.Dir validates against the host filesystem. Bootstrap from a
		// real directory, then point the exported runner field at the VFS cwd.
		interp.Dir("/"),
		interp.Env(hc.Env),
		interp.ExecHandlers(s.execMiddleware),
		interp.OpenHandler(s.openHandler),
		interp.StatHandler(s.statHandler),
		interp.ReadDirHandler2(s.readDirHandler),
		interp.AccessHandler(s.accessHandler),
	)
	if err != nil {
		_, _ = fmt.Fprintf(hc.Stderr, "xargs: initialize command: %v\n", err)
		return 2
	}
	runner.Dir = hc.Dir
	if err := runner.Run(ctx, file); err != nil {
		var status interp.ExitStatus
		if errors.As(err, &status) {
			return int(status)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 124
		}
		if errors.Is(err, context.Canceled) {
			return 130
		}
		_, _ = fmt.Fprintf(hc.Stderr, "xargs: run command: %v\n", err)
		return 1
	}
	return 0
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func exportedEnvironment(environ expand.Environ) []string {
	values := make(map[string]string)
	environ.Each(func(name string, variable expand.Variable) bool {
		if variable.IsSet() && variable.Exported && variable.Kind == expand.String {
			values[name] = variable.String()
		}
		return true
	})
	pairs := make([]string, 0, len(values))
	for name, value := range values {
		pairs = append(pairs, name+"="+value)
	}
	sort.Strings(pairs)
	return pairs
}

// commandPrelude makes every registered command visible to mvdan's `command
// -v` builtin. Each function immediately re-enters the fail-closed dispatcher;
// no host executable is resolved or invoked.
func commandPrelude() string {
	var b strings.Builder
	b.WriteString("readonly BASH_VERSION\nif [ -z \"${BASH_VERSINFO+x}\" ]; then BASH_VERSINFO=(5 3 15 1 go-bash go-bash); readonly -a BASH_VERSINFO; fi\n")
	// `sh` is an interpreter entry point rather than an external utility, so it
	// cannot live in the command registry. Keep it discoverable like `bash`, but
	// route invocation through the same nested-shell guard instead of starting a
	// second interpreter.
	b.WriteString("sh() { command bash \"$@\"; }\n")
	b.WriteString(`__gobash_dir_stack=("$PWD")
cd() {
  builtin cd "$@" || return $?
  __gobash_dir_stack[0]=$PWD
}
dirs() {
  local __gobash_i __gobash_index
  if [ "$#" -gt 1 ]; then command gobash_printf 'dirs: invalid argument\n' >&2; return 2; fi
  case ${1-} in
    -c) __gobash_dir_stack=("$PWD"); return;;
    -p|-l)
      for ((__gobash_i=0; __gobash_i<${#__gobash_dir_stack[@]}; __gobash_i++)); do
        command gobash_printf '%s\n' "${__gobash_dir_stack[__gobash_i]}"
      done
      return;;
    -v)
      for ((__gobash_i=0; __gobash_i<${#__gobash_dir_stack[@]}; __gobash_i++)); do
        command gobash_printf '%d  %s\n' "$__gobash_i" "${__gobash_dir_stack[__gobash_i]}"
      done
      return;;
    +[0-9]*) __gobash_index=${1#+};;
    -[0-9]*) __gobash_index=$((${#__gobash_dir_stack[@]}-1-${1#-}));;
    '') ;;
    *) command gobash_printf 'dirs: invalid argument\n' >&2; return 2;;
  esac
  if [ -n "${__gobash_index-}" ]; then
    if ((__gobash_index < 0 || __gobash_index >= ${#__gobash_dir_stack[@]})); then command gobash_printf 'dirs: directory stack index out of range\n' >&2; return 1; fi
    command gobash_printf '%s\n' "${__gobash_dir_stack[__gobash_index]}"
    return
  fi
  for ((__gobash_i=0; __gobash_i<${#__gobash_dir_stack[@]}; __gobash_i++)); do
    if ((__gobash_i)); then command gobash_printf ' '; fi
    command gobash_printf '%s' "${__gobash_dir_stack[__gobash_i]}"
  done
  command gobash_printf '\n'
}
pushd() {
  local __gobash_no_cd=0 __gobash_target __gobash_old=$PWD __gobash_tmp
  if [ "${1-}" = -n ]; then __gobash_no_cd=1; shift; fi
  if [ "$#" -gt 1 ]; then command gobash_printf 'pushd: too many arguments\n' >&2; return 2; fi
  if [ "$#" -eq 0 ]; then
    if [ "${#__gobash_dir_stack[@]}" -lt 2 ]; then command gobash_printf 'pushd: no other directory\n' >&2; return 1; fi
    __gobash_tmp=${__gobash_dir_stack[0]}
    __gobash_dir_stack[0]=${__gobash_dir_stack[1]}
    __gobash_dir_stack[1]=$__gobash_tmp
    if [ "$__gobash_no_cd" -eq 0 ]; then
      builtin cd "${__gobash_dir_stack[0]}" || { __gobash_target=$?; __gobash_tmp=${__gobash_dir_stack[0]}; __gobash_dir_stack[0]=${__gobash_dir_stack[1]}; __gobash_dir_stack[1]=$__gobash_tmp; return "$__gobash_target"; }
    fi
  else
    __gobash_target=$1
    if [ "$__gobash_no_cd" -eq 0 ]; then
      builtin cd "$__gobash_target" || return $?
      __gobash_target=$PWD
      __gobash_dir_stack=("$__gobash_target" "$__gobash_old" "${__gobash_dir_stack[@]:1}")
    else
      __gobash_dir_stack=("${__gobash_dir_stack[0]}" "$__gobash_target" "${__gobash_dir_stack[@]:1}")
    fi
  fi
  dirs
}
popd() {
  local __gobash_no_cd=0 __gobash_removed __gobash_status
  if [ "${1-}" = -n ]; then __gobash_no_cd=1; shift; fi
  if [ "$#" -ne 0 ]; then command gobash_printf 'popd: invalid argument\n' >&2; return 2; fi
  if [ "${#__gobash_dir_stack[@]}" -lt 2 ]; then command gobash_printf 'popd: directory stack empty\n' >&2; return 1; fi
  __gobash_removed=${__gobash_dir_stack[0]}
  __gobash_dir_stack=("${__gobash_dir_stack[@]:1}")
  if [ "$__gobash_no_cd" -eq 0 ]; then
    builtin cd "${__gobash_dir_stack[0]}" || { __gobash_status=$?; __gobash_dir_stack=("$__gobash_removed" "${__gobash_dir_stack[@]}"); return "$__gobash_status"; }
  fi
  dirs
}
`)
	b.WriteString(`printf() {
  case ${1-} in
  -v)
    if [ "$#" -lt 3 ]; then builtin printf '%s\n' 'printf: usage: printf -v var format [arguments]' >&2; return 2; fi
    local __gobash_printf_name=$2 __gobash_printf_value
    shift 2
    case $__gobash_printf_name in ''|[0-9]*|*[!a-zA-Z0-9_]*) builtin printf 'printf: invalid variable name: %s\n' "$__gobash_printf_name" >&2; return 2;; esac
    __gobash_printf_value=$(command gobash_printf "$@"; builtin printf $'\001') || return
    __gobash_printf_value=${__gobash_printf_value%$'\001'}
    builtin eval "$__gobash_printf_name=\$__gobash_printf_value"
    return
    ;;
  esac
  command gobash_printf "$@"
}
`)
	for _, name := range commandNamesIncludingInternal() {
		fmt.Fprintf(&b, "%s() { command %s \"$@\"; }\n", name, name)
	}
	return b.String()
}
