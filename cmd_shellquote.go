package gobash

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/syntax"
)

// gobash_quote is an internal helper for the printf compatibility shim. It
// implements the common `%q` and `%q\n` forms without exposing host execution.
func init() {
	registerInternal("gobash_quote", cmdGobashQuote)
	registerInternal("gobash_printf", cmdGobashPrintf)
	registerInternal("gobash_array_length", cmdGobashArrayLength)
}

// rewriteExplicitBuiltinPrintf routes `command printf` and `builtin printf`
// through the compatibility implementation. Without this small AST rewrite,
// those explicit forms bypass the prelude function and expose mvdan/sh's much
// smaller printf subset.
func rewriteExplicitBuiltinPrintf(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		first, second := call.Args[0].Lit(), call.Args[1].Lit()
		if (first != "command" && first != "builtin") || second != "printf" || len(call.Args[1].Parts) != 1 {
			return true
		}
		if lit, ok := call.Args[1].Parts[0].(*syntax.Lit); ok {
			lit.Value = "gobash_printf"
			call.Args = call.Args[1:]
		}
		return true
	})
}

// rewriteTypeAll treats `type -a` as `type` because the sandbox has exactly one
// resolution for every name: a shell construct/function or the fail-closed
// external dispatcher. mvdan/sh otherwise rejects the common discovery flag as
// unimplemented, even though its ordinary `type` output is accurate here.
func rewriteTypeAll(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		commandAt := 0
		if first := call.Args[0].Lit(); first == "command" || first == "builtin" {
			commandAt = 1
		}
		if len(call.Args) <= commandAt+1 || call.Args[commandAt].Lit() != "type" {
			return true
		}
		for i := commandAt + 1; i < len(call.Args); i++ {
			if call.Args[i].Lit() == "-a" && len(call.Args[i].Parts) == 1 {
				call.Args = append(call.Args[:i], call.Args[i+1:]...)
				break
			}
		}
		return true
	})
}

// rewriteArrayLengths works around an upstream associative-array length bug
// while preserving the normal expansion shape, including inside double quotes.
func rewriteArrayLengths(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		var parts *[]syntax.WordPart
		switch node := node.(type) {
		case *syntax.Word:
			parts = &node.Parts
		case *syntax.DblQuoted:
			parts = &node.Parts
		default:
			return true
		}
		for i, part := range *parts {
			param, ok := part.(*syntax.ParamExp)
			if !ok || !param.Length || param.Param == nil || param.Index == nil {
				continue
			}
			index, ok := param.Index.(*syntax.Word)
			if !ok || index.Lit() != "@" && index.Lit() != "*" {
				continue
			}
			helper, err := syntax.NewParser().Parse(strings.NewReader("command gobash_array_length \"${!"+param.Param.Value+"[@]}\""), "gobash-array-length")
			if err == nil {
				(*parts)[i] = &syntax.CmdSubst{Stmts: helper.Stmts}
			}
		}
		return true
	})
}

func cmdGobashArrayLength(_ context.Context, e *Env) int {
	_, err := fmt.Fprint(e.Stdout, len(e.Args)-1)
	if err != nil {
		e.Errorf("%v", err)
		return 1
	}
	return 0
}

func cmdGobashQuote(_ context.Context, e *Env) int {
	if len(e.Args) < 2 || e.Args[1] != "%q" && e.Args[1] != `%q\n` {
		e.Errorf("supported formats are %%q and %%q\\n")
		return 2
	}
	values := e.Args[2:]
	if len(values) == 0 {
		values = []string{""}
	}
	newline := e.Args[1] == `%q\n`
	for _, value := range values {
		if _, err := fmt.Fprint(e.Stdout, bashQuoted(value)); err != nil {
			e.Errorf("%v", err)
			return 1
		}
		if newline {
			if _, err := fmt.Fprintln(e.Stdout); err != nil {
				e.Errorf("%v", err)
				return 1
			}
		}
	}
	return 0
}

func cmdGobashPrintf(_ context.Context, e *Env) int {
	args := e.Args[1:]
	endOfOptions := false
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
		endOfOptions = true
	}
	if len(args) == 0 {
		e.Errorf("usage: printf [-v var] format [arguments]")
		return 2
	}
	if args[0] == "-v" {
		e.Errorf("-v is only available through the shell printf builtin")
		return 2
	}
	if !endOfOptions && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		e.Errorf("invalid option: %s", args[0])
		return 2
	}
	out, diagnostics, err := renderBashPrintf(args[0], args[1:])
	// Bash reports a malformed escape but still prints it and succeeds.
	for _, diagnostic := range diagnostics {
		_, _ = fmt.Fprintf(e.Stderr, "printf: %s\n", diagnostic)
	}
	if err != nil {
		e.Errorf("%v", err)
		return 1
	}
	if _, err := fmt.Fprint(e.Stdout, out); err != nil {
		e.Errorf("%v", err)
		return 1
	}
	return 0
}

// renderBashPrintf formats like bash's printf builtin. Diagnostics are
// non-fatal messages (such as a \x escape without digits) that bash writes to
// stderr while still printing the escape text and exiting 0.
func renderBashPrintf(format string, args []string) (string, []string, error) {
	var out strings.Builder
	var diagnostics []string
	argAt := 0
	for {
		consumedAtStart := argAt
		for i := 0; i < len(format); {
			if format[i] == '\\' {
				esc := decodePrintfEscape(format[i:], false)
				out.WriteString(esc.value)
				diagnostics = appendDiagnostic(diagnostics, esc.diagnostic)
				i += esc.used
				continue
			}
			if format[i] != '%' {
				out.WriteByte(format[i])
				i++
				continue
			}
			start := i
			i++
			if i < len(format) && format[i] == '%' {
				out.WriteByte('%')
				i++
				continue
			}
			for i < len(format) && strings.ContainsRune("-+ #0", rune(format[i])) {
				i++
			}
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
			if i < len(format) && format[i] == '.' {
				i++
				for i < len(format) && format[i] >= '0' && format[i] <= '9' {
					i++
				}
			}
			if i >= len(format) {
				return "", diagnostics, fmt.Errorf("missing format character")
			}
			verb := format[i]
			i++
			directive := format[start:i]
			value := ""
			if argAt < len(args) {
				value = args[argAt]
				argAt++
			}
			switch verb {
			case 's':
				_, _ = fmt.Fprintf(&out, directive, value)
			case 'q':
				_, _ = fmt.Fprintf(&out, directive[:len(directive)-1]+"s", bashQuoted(value))
			case 'b':
				decoded, stop, argDiagnostics := decodePrintfBytes(value)
				diagnostics = append(diagnostics, argDiagnostics...)
				_, _ = fmt.Fprintf(&out, directive[:len(directive)-1]+"s", decoded)
				if stop {
					return out.String(), diagnostics, nil
				}
			case 'c':
				r := rune(0)
				if value != "" {
					r, _ = utf8FirstRune(value)
				}
				_, _ = fmt.Fprintf(&out, directive, r)
			case 'd', 'i', 'o', 'x', 'X', 'u':
				n, err := parsePrintfInteger(value)
				if err != nil {
					return "", diagnostics, err
				}
				goDirective := directive
				if verb == 'i' {
					goDirective = directive[:len(directive)-1] + "d"
				}
				if verb == 'u' {
					_, _ = fmt.Fprintf(&out, goDirective[:len(goDirective)-1]+"d", uint64(n))
				} else {
					_, _ = fmt.Fprintf(&out, goDirective, n)
				}
			case 'f', 'F', 'e', 'E', 'g', 'G':
				n, err := strconv.ParseFloat(zeroIfEmpty(value), 64)
				if err != nil {
					return "", diagnostics, fmt.Errorf("%s: invalid number", value)
				}
				_, _ = fmt.Fprintf(&out, directive, n)
			default:
				return "", diagnostics, fmt.Errorf("unsupported format character: %%%c", verb)
			}
		}
		if argAt >= len(args) || argAt == consumedAtStart {
			break
		}
	}
	return out.String(), diagnostics, nil
}

func appendDiagnostic(diagnostics []string, diagnostic string) []string {
	if diagnostic == "" {
		return diagnostics
	}
	return append(diagnostics, diagnostic)
}

func zeroIfEmpty(value string) string {
	if value == "" {
		return "0"
	}
	return value
}

func parsePrintfInteger(value string) (int64, error) {
	value = zeroIfEmpty(strings.TrimSpace(value))
	if strings.Contains(value, "#") {
		baseText, digits, ok := strings.Cut(value, "#")
		base, err := strconv.Atoi(baseText)
		if !ok || err != nil || base < 2 || base > 36 {
			return 0, fmt.Errorf("%s: invalid number", value)
		}
		n, err := strconv.ParseInt(digits, base, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: invalid number", value)
		}
		return n, nil
	}
	n, err := strconv.ParseInt(value, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid number", value)
	}
	return n, nil
}

// decodePrintfBytes expands a %b argument. The second result reports a \c
// escape, which stops all further printf output.
func decodePrintfBytes(value string) (string, bool, []string) {
	var out strings.Builder
	var diagnostics []string
	for i := 0; i < len(value); {
		if value[i] != '\\' {
			out.WriteByte(value[i])
			i++
			continue
		}
		esc := decodePrintfEscape(value[i:], true)
		if esc.stop {
			return out.String(), true, diagnostics
		}
		out.WriteString(esc.value)
		diagnostics = appendDiagnostic(diagnostics, esc.diagnostic)
		i += esc.used
	}
	return out.String(), false, diagnostics
}

// printfEscape is one decoded backslash escape.
type printfEscape struct {
	value      string // bytes to emit
	used       int    // bytes of input consumed, including the backslash
	stop       bool   // %b's \c: stop all output
	diagnostic string // non-fatal message bash prints to stderr
}

// decodePrintfEscape decodes the escape at the start of value (value[0] is a
// backslash) with the rules of bash's printf builtin (tescape in printf.def).
// inArg selects %b argument rules: \0 is followed by up to three more octal
// digits, \c stops output, and \' \" \? are not escapes.
//
// Octal (\NNN) and hex (\xHH) escapes produce one raw byte, keeping only the
// low eight bits. \uHHHH and \UHHHHHHHH produce UTF-8, as bash does in a UTF-8
// locale. An escape bash does not recognise produces a lone backslash and
// consumes only that backslash, so the next character is read normally.
func decodePrintfEscape(value string, inArg bool) printfEscape {
	literal := printfEscape{value: `\`, used: 1}
	if len(value) < 2 {
		return literal
	}
	c := value[1]
	switch c {
	case 'a':
		return printfEscape{value: "\a", used: 2}
	case 'b':
		return printfEscape{value: "\b", used: 2}
	case 'e', 'E':
		return printfEscape{value: "\x1b", used: 2}
	case 'f':
		return printfEscape{value: "\f", used: 2}
	case 'n':
		return printfEscape{value: "\n", used: 2}
	case 'r':
		return printfEscape{value: "\r", used: 2}
	case 't':
		return printfEscape{value: "\t", used: 2}
	case 'v':
		return printfEscape{value: "\v", used: 2}
	case '\\':
		return printfEscape{value: `\`, used: 2}
	case '\'', '"', '?':
		if inArg {
			return literal
		}
		return printfEscape{value: string(c), used: 2}
	case 'c':
		if inArg {
			return printfEscape{used: 2, stop: true}
		}
		return literal
	case '0', '1', '2', '3', '4', '5', '6', '7':
		n := uint(c - '0')
		more := 2
		if n == 0 && inArg {
			more = 3
		}
		end := 2
		for end < len(value) && more > 0 && value[end] >= '0' && value[end] <= '7' {
			n = n*8 + uint(value[end]-'0')
			end++
			more--
		}
		return printfEscape{value: string([]byte{byte(n)}), used: end}
	case 'x':
		n, end := printfHexDigits(value, 2)
		if end == 2 {
			literal.diagnostic = `missing hex digit for \x`
			return literal
		}
		return printfEscape{value: string([]byte{byte(n)}), used: end}
	case 'u', 'U':
		maxDigits := 4
		if c == 'U' {
			maxDigits = 8
		}
		n, end := printfHexDigits(value, maxDigits)
		if end == 2 {
			literal.diagnostic = `missing unicode digit for \` + string(c)
			return literal
		}
		return printfEscape{value: bashUTF8(n), used: end}
	default:
		return literal
	}
}

// printfHexDigits reads up to maxDigits hex digits starting at value[2].
func printfHexDigits(value string, maxDigits int) (uint64, int) {
	var n uint64
	end := 2
	for end < len(value) && end-2 < maxDigits {
		c := value[end]
		var digit byte
		switch {
		case c >= '0' && c <= '9':
			digit = c - '0'
		case c >= 'a' && c <= 'f':
			digit = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			digit = c - 'A' + 10
		default:
			return n, end
		}
		n = n*16 + uint64(digit)
		end++
	}
	return n, end
}

// bashUTF8 encodes a \u or \U value the way bash does in a UTF-8 locale
// (u32toutf8): values up to 0x7f are one byte, surrogates are encoded rather
// than rejected, values above U+10FFFF use the original five- and six-byte
// UTF-8 forms, and values from 0x80000000 produce nothing.
func bashUTF8(n uint64) string {
	switch {
	case n < 0x80:
		return string([]byte{byte(n)})
	case n < 0x800:
		return string([]byte{0xc0 | byte(n>>6), 0x80 | byte(n&0x3f)})
	case n < 0x10000:
		return string([]byte{0xe0 | byte(n>>12), 0x80 | byte(n>>6&0x3f), 0x80 | byte(n&0x3f)})
	case n < 0x200000:
		return string([]byte{0xf0 | byte(n>>18), 0x80 | byte(n>>12&0x3f), 0x80 | byte(n>>6&0x3f), 0x80 | byte(n&0x3f)})
	case n < 0x4000000:
		return string([]byte{0xf8 | byte(n>>24), 0x80 | byte(n>>18&0x3f), 0x80 | byte(n>>12&0x3f), 0x80 | byte(n>>6&0x3f), 0x80 | byte(n&0x3f)})
	case n < 0x80000000:
		return string([]byte{0xfc | byte(n>>30), 0x80 | byte(n>>24&0x3f), 0x80 | byte(n>>18&0x3f), 0x80 | byte(n>>12&0x3f), 0x80 | byte(n>>6&0x3f), 0x80 | byte(n&0x3f)})
	default:
		return ""
	}
}

func utf8FirstRune(value string) (rune, int) {
	for _, r := range value {
		return r, len(string(r))
	}
	return 0, 0
}

func bashQuoted(value string) string {
	if value == "" {
		return "''"
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			var out strings.Builder
			out.WriteString("$'")
			for _, q := range value {
				switch q {
				case '\n':
					out.WriteString(`\n`)
				case '\r':
					out.WriteString(`\r`)
				case '\t':
					out.WriteString(`\t`)
				case '\\', '\'':
					out.WriteByte('\\')
					out.WriteRune(q)
				default:
					if unicode.IsControl(q) {
						fmt.Fprintf(&out, `\x%02x`, q)
					} else {
						out.WriteRune(q)
					}
				}
			}
			out.WriteByte('\'')
			return out.String()
		}
	}
	var out strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_@%+=:,./-", r) {
			out.WriteRune(r)
		} else {
			out.WriteByte('\\')
			out.WriteRune(r)
		}
	}
	return out.String()
}
