package gobash

const (
	// Version is the go-bash release version. Keep it aligned with the module tag.
	Version = "v0.6.3"

	// BashCompatibility is the GNU Bash language compatibility target. go-bash
	// deliberately identifies itself with a suffix rather than claiming to be
	// the GNU implementation.
	BashCompatibility = "5.3"
	// BashVersion is the value exposed through the BASH_VERSION environment
	// variable and runtime metadata.
	BashVersion = "5.3.15(1)-go-bash"

	// Runtime identifies the interpreter implementation independently of the
	// Bash language compatibility target.
	Runtime = "go-bash/mvdan-sh"
	// RuntimeVersion identifies the embedded mvdan/sh interpreter version.
	RuntimeVersion = "mvdan-sh/v3.14.0"
)

// BashLimitations returns deliberate or upstream compatibility boundaries
// that commonly affect generated agent scripts. Keep this list concise and
// actionable; gobash info exposes it at runtime.
func BashLimitations() []string {
	return []string{
		"no process substitution; use pipelines or VFS temporary files",
		"no PIPESTATUS (a script reading it is refused); use set -o pipefail and check $?",
		"read -d/-n/-N/-t are refused before the script runs; use xargs -0, jq, or outer JavaScript",
		"some associative-array key syntax differs; quote complex keys or use jq/outer JavaScript",
		"BASH_SOURCE, BASH_LINENO, and FUNCNAME are unavailable",
		"set -E and shopt -p/-q are refused before the script runs; declare -i is unavailable",
	}
}
