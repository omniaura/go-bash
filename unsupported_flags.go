package gobash

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// unsupportedBuiltinFlag names a builtin option the interpreter rejects at run
// time with "invalid option" and exit status 2 while the script carries on.
// Generated scripts rarely check that status, so the next line runs on empty
// input and the failure goes unnoticed. Like process substitution, these are
// refused before the script runs, with the alternative in the message. Only
// literal words are inspected; a flag built at run time still reaches the
// interpreter's own error.
func unsupportedBuiltinFlag(call *syntax.CallExpr) string {
	args := call.Args
	// "command read -d" and "builtin read -d" run the same builtin.
	for len(args) > 0 && (args[0].Lit() == "command" || args[0].Lit() == "builtin") {
		args = args[1:]
		for len(args) > 0 && args[0].Lit() == "-p" {
			args = args[1:]
		}
	}
	if len(args) < 2 {
		return ""
	}
	name := args[0].Lit()
	for _, arg := range args[1:] {
		flag := arg.Lit()
		if !strings.HasPrefix(flag, "-") || flag == "-" || flag == "--" {
			if flag == "--" {
				return ""
			}
			continue
		}
		letters := strings.TrimPrefix(flag, "-")
		switch name {
		case "read":
			for _, c := range "dnNt" {
				if strings.ContainsRune(letters, c) {
					return "read -" + string(c) + " is unavailable; read whole lines, or split with jq, xargs -0 or outer JavaScript"
				}
			}
		case "set":
			if strings.ContainsRune(letters, 'E') {
				return "set -E is unavailable; use set -e and check $?"
			}
		case "shopt":
			for _, c := range "pq" {
				if strings.ContainsRune(letters, c) {
					return "shopt -" + string(c) + " is unavailable; set the option with shopt -s/-u instead of querying it"
				}
			}
		}
	}
	return ""
}
