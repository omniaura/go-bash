# go-bash

A simulated bash environment with an in-memory filesystem, for AI agents — a Go
port of the ideas behind [just-bash](https://github.com/vercel-labs/just-bash).

Agents need a shell, but giving them a *real* shell is risky. `go-bash` gives
them a modern Bash-compatible *language* with pipes, redirects, variables, functions, loops
and globs — but the filesystem is virtual (in-memory), network is opt-in, and
nothing touches the host. No OS-level sandbox required.

```go
sh := gobash.New() // fresh in-memory filesystem; working directory is /tmp
res, _ := sh.Run(context.Background(), `
  mkdir -p /data
  echo "hello" > /data/greeting.txt
  cat /data/greeting.txt | tr a-z A-Z
`)
fmt.Print(res.Stdout) // HELLO
```

## Architecture

`go-bash` is deliberately a thin assembly over two mature libraries plus a layer
of reimplemented coreutils:

| Layer | Provided by | Notes |
|-------|-------------|-------|
| Shell **language** (parse + interpret) | [`mvdan.cc/sh/v3`](https://github.com/mvdan/sh) | Pure Go, no external shell. Pipes, redirects, `&&`/`||`, loops, functions, globs, heredocs, expansion. |
| Shell **builtins** (`echo`, `printf`, `pwd`, `cd`, `test`, `[`, `true`, `false`, `read`, `getopts`, ...) | `mvdan.cc/sh/v3/interp` | Handled internally by the interpreter — **not** reimplemented here. |
| **Virtual filesystem** | [`spf13/afero`](https://github.com/spf13/afero) `MemMapFs` | In-memory, concurrent-safe, read/write. |
| **External coreutils** (`cat`, `ls`, `mkdir`, `rm`, `grep`, `sed`, `awk`, `jq`, ...) | this repo | Go builtins dispatched via the interpreter's `ExecHandlers`, operating on the afero FS. **This is where the work is.** |

The interpreter's `ExecHandlers` middleware dispatches `argv[0]` to a registered
Go builtin; there is **no fall-through to host binaries** — an unknown command
is a `command not found` (exit 127). The `Open`/`Stat`/`ReadDir` handlers route
redirects and globbing to the same in-memory filesystem; the `Access` handler
keeps `cd` and `-r`/`-w`/`-x` tests in that VFS as well.

This is a Bash-compatible interpreter, not GNU Bash or a Linux container. It
tracks the current Bash 5.3 patch level while keeping a distinct implementation
identity. `$0`
reports `bash`, and `$BASH_VERSION` reports the explicitly suffixed compatibility
identity `5.3.15(1)-go-bash`; `$BASH_COMPAT` reports the `5.3` language target.
`$BASH_VERSINFO` reports the same compatibility tuple. The embedded interpreter
is mvdan/sh v3.14.0; go-bash does not claim to be the GNU executable.
`gobash info` identifies the go-bash and mvdan/sh versions, the virtual working
directory, and the sandbox boundary.
`gobash commands` is the authoritative external-command inventory, and
`command -v <name>` accurately recognizes each listed command plus interpreter
builtins. `sh` is a discoverable compatibility alias for the current interpreter.
Because the sandbox has one resolution per name, `type -a NAME` is equivalent
to `type NAME` and reports that single resolution.
`bash --help` provides the same boundary summary. Nested `bash -c` or `sh -c`
invocations are intentionally unavailable because the current interpreter is
already the sandbox boundary. Python, Node.js, zsh, package installation, host
executables, host files, and network access are intentionally unavailable;
callers can perform richer computation in their outer JavaScript runtime.

See [`AGENTS.md`](./AGENTS.md) for the contributor contract (how to add a
command, the test/TDD model, and the boundary rules).

## Included command set

The initial agent/data-analysis toolset is implemented end-to-end:
`bash`, `gobash`, `cat`, `ls`, `mkdir`, `touch`, `rm`, `cp`, `mv`, `tree`, `head`, `tail`, `wc`,
`sort`, `uniq`, `cut`, `tr`, `grep`/`egrep`/`fgrep`, `rg`, `sed`, `awk`, `find`,
`basename`, `dirname`, `jq`, `base64`, `tee`, `date`, `env`, `printenv`,
`whoami`, `seq`, `xargs`, `mktemp`, `gzip`/`gunzip`, `tar`, `zip`/`unzip`,
`xxd`, `od`, and `hexdump`.

### Compatibility highlights

The data utilities intentionally implement a bounded, agent-oriented subset of
their GNU/BSD counterparts. The most common result-inspection forms are:

| Command | Supported forms |
|---------|-----------------|
| `find` | one path plus `-maxdepth N`, `-type f\|d`, `-name`, `-iname`, `-print`, and `-print0`, in any order; predicates are implicitly ANDed |
| `jq` | `-R`, `-r`, `-c`, `-s`, `-n`, `-S`, `-e`, `--arg`, and `--argjson`; `-e` follows jq's last-result truthiness exit codes |
| `date` | `-u`, GNU-style `+FORMAT`, `-d` with `now`/`today`/`tomorrow`/`yesterday`, RFC3339, `YYYY-MM-DD`, `YYYY-MM-DD HH:MM:SS` with an optional `UTC` or numeric offset, `@TIMESTAMP`, and one signed seconds/minutes/hours/days/weeks offset; `-u` also parses timezone-free anchors as UTC |
| `grep` / `rg` | grep BRE plus `-E`/`-F`, `-w`, `-A`/`-B`/`-C`, recursive search, line/count/file modes; `rg` reads stdin when no path is supplied (use `rg PATTERN .` for explicit VFS recursion) and adds `--files` plus globstar-aware `-g`/`--glob` includes/excludes |
| `ls` | one-entry-per-line output plus combinable `-a`, `-1`, `-d`, and `-l`; long output uses stable virtual owner/group names and UTC timestamps |
| `head` / `tail` | line counts via `-n N`, `-n +N`, `-nN`, and `--lines=N`; byte counts via `-c N`, `-c +N`, `-cN`, and `--bytes=N` |
| `gzip` / `gunzip` | gzip streams and files with `-c`, `-d`, `-k`, `-t`, `-f`, and compression levels `-1` through `-9`; ordinary file mode follows `.gz` naming and removal conventions |
| `tar` | create, extract, and list via `-c`/`-x`/`-t`, `-f`, `-z`, `-C`, `-v`, combined flags such as `-czf`, and `-` for stdin/stdout; extraction is confined to the VFS destination and supports regular files/directories |
| `zip` / `unzip` | recursive, basename-only, or stored ZIP creation with `-r`, `-j`, `-q`, `-0`, and compression levels `-1` through `-9`; extraction/listing/streaming with `-d`, `-l`, `-Z1`, `-p`, `-q`, `-o`, and `-n`; member selection supports exact paths and globs |
| `xxd` / `od` / `hexdump` | canonical and plain `xxd`, including `-r -p`, byte limits/offsets/columns; `od -An -tx1` with `-N`/`-j`; canonical `hexdump -C` with `-n`/`-s` |
| `xargs` | `-0`, `-r`, `-n`, and line-preserving `-I`; invoked argv resolves through the same shell-aware dispatcher, so both shell builtins such as `printf` and registered external commands work |
| `printf` | common Bash formats including mixed `%q`, `%b`, numeric width/precision, `--`, and shell-local `-v`; explicit `command printf` and `builtin printf` use the same formatter. Escapes match GNU bash byte for byte: `\NNN` octal in the format, `\0NNN` in `%b`, `\xHH`, and `\uHHHH`/`\UHHHHHHHH` as UTF-8 |
| `tr` | GNU coreutils byte semantics (C locale): `-c`/`-C`, `-d`, `-s`, `-t` and their long forms; sets with `\NNN` octal and backslash escapes, ranges, `[:class:]`, `[=c=]`, `[c*]` and `[c*n]` |

Use `gobash commands`, `gobash info --json`, and the supported forms above as
the runtime inventory. Unsupported predicates, options, and formats fail
explicitly rather than silently changing meaning or falling through to a host
executable.

Archive commands support the formats agents most commonly encounter while
inspecting tool artifacts. They deliberately omit archive encryption, device
nodes, links, ownership restoration, and host-specific metadata. Unsafe
absolute or parent-traversing member paths fail before being written.

### Bash compatibility boundary

Each `Shell.Run` starts a fresh shell variable, function, option, and cwd state;
the virtual filesystem persists. The most important remaining language
boundaries are reported by `gobash info --json`: process substitution,
`PIPESTATUS`, `read -d/-n/-N/-t`, associative-array edge cases,
`BASH_SOURCE`/`BASH_LINENO`/`FUNCNAME`, `set -E`, `shopt -p/-q`, and
`declare -i`. Prefer pipelines or VFS temporary files, `set -o pipefail`,
`find -print0 | xargs -0`, jq, and the host application's outer JavaScript.
Basic `cd`/`dirs`/`pushd`/`popd` behavior—including `dirs -p/-v/-c`—is kept
consistent inside the VFS. Associative-array cardinality is compatible for
both literal and dynamic assignments; quote keys containing shell syntax.

`Shell.Run` also bounds script bytes, captured output, and external command
invocations. Runs have a five-second default deadline; hosts can tighten it
with `WithTimeout` or an earlier `context.Context` deadline. Results identify
stdout and stderr truncation separately as well as through the compatibility
`Truncated` field. Deadline cancellation is abrupt: do not rely on an `EXIT`
trap to finish cleanup after a timeout. The VFS remains valid for a later run,
so callers can inspect or remove partial files explicitly.

For structured host integration, `Shell.RunInput` accepts an `io.Reader` for
stdin. A host can JSON-encode a value into stdin, run a jq pipeline, and decode
the single JSON value written to stdout. This keeps Bash focused on VFS and
pipeline work while an outer JavaScript layer owns rich computation and tool
calls. The execution deadline applies while the interpreter is running, but Go
cannot forcibly cancel an arbitrary `io.Reader` blocked inside its own `Read`
method; strict-deadline callers should use finite in-memory input or a
context-aware reader. Code Mode uses a finite `bytes.Reader`.

## License

TBD (pending decision — see repo owner).
