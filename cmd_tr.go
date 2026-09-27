package gobash

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func init() { Register("tr", cmdTr) }

// cmdTr translates, deletes, or squeezes bytes like GNU coreutils tr in the C
// locale. Like GNU tr it works on bytes, not characters: a multibyte UTF-8
// character in a set stands for each of its bytes.
//
// Supported: -c/-C/--complement, -d/--delete, -s/--squeeze-repeats,
// -t/--truncate-set1, and the SET syntax: backslash escapes (\\ \a \b \f \n
// \r \t \v and one to three octal digits \NNN), ranges m-n, [:class:],
// [=c=], [c*] and [c*n].
func cmdTr(ctx context.Context, e *Env) int {
	opts, operands, code := parseTrOptions(e)
	if code >= 0 {
		return code
	}
	if code := checkTrOperands(e, opts, operands); code != 0 {
		return code
	}
	s1, err := parseTrSet(e, operands[0])
	if err != nil {
		e.Errorf("%v", err)
		return 1
	}
	var s2 *trSet
	if len(operands) == 2 {
		if s2, err = parseTrSet(e, operands[1]); err != nil {
			e.Errorf("%v", err)
			return 1
		}
	}
	translating := len(operands) == 2 && !opts.delete
	if err := validateTrSets(s1, s2, opts, translating); err != nil {
		e.Errorf("%v", err)
		return 1
	}

	var f trFilter
	switch {
	case translating:
		f.translate = s1.translation(s2, opts)
		if opts.squeeze {
			f.squeeze = s2.members()
		}
	case opts.delete:
		f.remove = s1.members()
		if opts.complement {
			f.remove = complementSet(f.remove)
		}
		if opts.squeeze {
			f.squeeze = s2.members()
		}
	default: // squeezing with one operand
		f.squeeze = s1.members()
		if opts.complement {
			f.squeeze = complementSet(f.squeeze)
		}
	}
	if err := f.run(ctx, e.Stdin, e.Stdout); err != nil {
		e.Errorf("%v", err)
		return 1
	}
	return 0
}

type trOptions struct {
	complement, delete, squeeze, truncate bool
}

const trTryHelp = "Try 'tr --help' for more information.\n"

var trLongOptions = []string{"complement", "delete", "squeeze-repeats", "truncate-set1", "help", "version"}

// parseTrOptions follows GNU tr's getopt string "+cCdst": option parsing
// stops at the first operand. It returns code -1 to continue, or an exit code.
func parseTrOptions(e *Env) (trOptions, []string, int) {
	var opts trOptions
	args := e.Args[1:]
	i := 0
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if strings.HasPrefix(arg, "--") {
			name := arg[2:]
			var matches []string
			for _, long := range trLongOptions {
				if long == name {
					matches = []string{long}
					break
				}
				if strings.HasPrefix(long, name) {
					matches = append(matches, long)
				}
			}
			if len(matches) != 1 {
				_, _ = fmt.Fprintf(e.Stderr, "tr: unrecognized option '%s'\n%s", arg, trTryHelp)
				return opts, nil, 1
			}
			switch matches[0] {
			case "complement":
				opts.complement = true
			case "delete":
				opts.delete = true
			case "squeeze-repeats":
				opts.squeeze = true
			case "truncate-set1":
				opts.truncate = true
			case "help":
				_, _ = fmt.Fprint(e.Stdout, trUsage)
				return opts, nil, 0
			case "version":
				_, _ = fmt.Fprintf(e.Stdout, "tr (go-bash) %s\n", Version)
				return opts, nil, 0
			}
			continue
		}
		if len(arg) < 2 || arg[0] != '-' {
			break
		}
		for _, c := range arg[1:] {
			switch c {
			case 'c', 'C':
				opts.complement = true
			case 'd':
				opts.delete = true
			case 's':
				opts.squeeze = true
			case 't':
				opts.truncate = true
			default:
				_, _ = fmt.Fprintf(e.Stderr, "tr: invalid option -- '%c'\n%s", c, trTryHelp)
				return opts, nil, 1
			}
		}
	}
	return opts, args[i:], -1
}

const trUsage = `Usage: tr [OPTION]... STRING1 [STRING2]
Translate, squeeze, and/or delete characters from standard input,
writing to standard output.

  -c, -C, --complement    use the complement of STRING1
  -d, --delete            delete characters in STRING1, do not translate
  -s, --squeeze-repeats   replace each sequence of a repeated character
                            that is listed in the last specified STRING,
                            with a single occurrence of that character
  -t, --truncate-set1     first truncate STRING1 to length of STRING2
      --help              display this help and exit
      --version           output version information and exit

STRINGs are byte sets as in GNU tr: \NNN (octal), \\, \a, \b, \f, \n, \r,
\t, \v, CHAR1-CHAR2, [CHAR*], [CHAR*REPEAT], [:CLASS:] and [=CHAR=].
`

// checkTrOperands applies GNU tr's operand-count rules.
func checkTrOperands(e *Env, opts trOptions, operands []string) int {
	minOperands, maxOperands := 2, 2
	if opts.delete != opts.squeeze {
		minOperands = 1
	}
	if opts.delete && !opts.squeeze {
		maxOperands = 1
	}
	switch {
	case len(operands) == 0:
		_, _ = fmt.Fprintf(e.Stderr, "tr: missing operand\n%s", trTryHelp)
		return 1
	case len(operands) < minOperands:
		why := "Two strings must be given when translating.\n"
		if opts.squeeze {
			why = "Two strings must be given when both deleting and squeezing repeats.\n"
		}
		_, _ = fmt.Fprintf(e.Stderr, "tr: missing operand after '%s'\n%s%s", operands[len(operands)-1], why, trTryHelp)
		return 1
	case len(operands) > maxOperands:
		_, _ = fmt.Fprintf(e.Stderr, "tr: extra operand '%s'\n", operands[maxOperands])
		if len(operands) == 2 {
			_, _ = fmt.Fprint(e.Stderr, "Only one string may be given when deleting without squeezing repeats.\n")
		}
		_, _ = fmt.Fprint(e.Stderr, trTryHelp)
		return 1
	}
	return 0
}

type trElementKind int

const (
	trChar trElementKind = iota
	trRange
	trClass
	trEquiv
	trRepeat
)

// trElement is one construct of a SET. Repeat elements with count 0 are the
// indefinite [c*] form until validateTrSets fills them.
type trElement struct {
	kind   trElementKind
	lo, hi byte // trChar/trEquiv/trRepeat use lo; trRange uses both
	class  string
	count  int
}

func (el trElement) bytes() []byte {
	switch el.kind {
	case trRange:
		out := make([]byte, 0, int(el.hi)-int(el.lo)+1)
		for c := int(el.lo); c <= int(el.hi); c++ {
			out = append(out, byte(c))
		}
		return out
	case trClass:
		var out []byte
		for c := range 256 {
			if trClassContains(el.class, byte(c)) {
				out = append(out, byte(c))
			}
		}
		return out
	case trRepeat:
		return []byte(strings.Repeat(string([]byte{el.lo}), el.count))
	default:
		return []byte{el.lo}
	}
}

type trSet struct {
	elements []trElement
}

// expand lists the set's bytes in order.
func (s *trSet) expand() []byte {
	var out []byte
	for _, el := range s.elements {
		out = append(out, el.bytes()...)
	}
	return out
}

func (s *trSet) members() *[256]bool {
	var in [256]bool
	if s == nil {
		return &in
	}
	for _, c := range s.expand() {
		in[c] = true
	}
	return &in
}

func complementSet(in *[256]bool) *[256]bool {
	var out [256]bool
	for c := range out {
		out[c] = !in[c]
	}
	return &out
}

// translation builds the byte map for translating s1 to s2.
func (s *trSet) translation(s2 *trSet, opts trOptions) *[256]byte {
	var table [256]byte
	for c := range table {
		table[c] = byte(c)
	}
	to := s2.expand()
	if opts.complement {
		in := s.members()
		next := 0
		for c := range 256 {
			if in[c] {
				continue
			}
			if next >= len(to) {
				break
			}
			table[c] = to[next]
			next++
		}
		return &table
	}
	for i, c := range s.expand() {
		if i >= len(to) {
			break
		}
		table[c] = to[i]
	}
	return &table
}

// length is the number of bytes in the set, or its complement's size.
func (s *trSet) length(complement bool) int {
	if complement {
		n := 0
		for _, in := range s.members() {
			if !in {
				n++
			}
		}
		return n
	}
	n := 0
	for _, el := range s.elements {
		n += len(el.bytes())
	}
	return n
}

func (s *trSet) indefiniteRepeats() []int {
	var at []int
	for i, el := range s.elements {
		if el.kind == trRepeat && el.count == 0 {
			at = append(at, i)
		}
	}
	return at
}

func (s *trSet) has(kind trElementKind, match func(trElement) bool) bool {
	for _, el := range s.elements {
		if el.kind == kind && (match == nil || match(el)) {
			return true
		}
	}
	return false
}

func isTrCaseClass(el trElement) bool { return el.class == "upper" || el.class == "lower" }

// validateTrSets applies GNU tr's set checks in the same order, filling an
// indefinite [c*] in SET2 and extending a short SET2 with its last byte.
func validateTrSets(s1, s2 *trSet, opts trOptions, translating bool) error {
	if len(s1.indefiniteRepeats()) > 0 {
		return errors.New("the [c*] repeat construct may not appear in string1")
	}
	if s2 == nil {
		return nil
	}
	s1Len := s1.length(opts.complement)
	indefinite := s2.indefiniteRepeats()
	if len(indefinite) > 1 {
		return errors.New("only one [c*] repeat construct may appear in string2")
	}
	if len(indefinite) == 1 {
		if s2Len := s2.length(false); s1Len >= s2Len {
			s2.elements[indefinite[0]].count = s1Len - s2Len
		}
	}
	if !translating {
		if len(indefinite) > 0 {
			return errors.New("the [c*] construct may appear in string2 only when translating")
		}
		return nil
	}
	if s2.has(trEquiv, nil) {
		return errors.New("[=c=] expressions may not appear in string2 when translating")
	}
	if s2.has(trClass, func(el trElement) bool { return !isTrCaseClass(el) }) {
		return errors.New("when translating, the only character classes that may appear in\nstring2 are 'upper' and 'lower'")
	}
	if !opts.complement && s2.has(trClass, nil) && !trCaseClassesAligned(s1, s2) {
		return errors.New("misaligned [:upper:] and/or [:lower:] construct")
	}
	if s2Len := s2.length(false); s1Len > s2Len && !opts.truncate {
		if s2Len == 0 {
			return errors.New("when not truncating set1, string2 must be non-empty")
		}
		last := s2.elements[len(s2.elements)-1]
		if last.kind == trClass {
			return errors.New("when translating with string1 longer than string2,\nthe latter string must not end with a character class")
		}
		fill := last.lo
		if last.kind == trRange {
			fill = last.hi
		}
		s2.elements = append(s2.elements, trElement{kind: trRepeat, lo: fill, count: s1Len - s2Len})
	}
	if opts.complement && s1.has(trClass, nil) {
		to := s2.expand()
		homogeneous := len(to) > 0
		for _, c := range to {
			homogeneous = homogeneous && c == to[0]
		}
		if s2.length(false) != s1Len || !homogeneous {
			return errors.New("when translating with complemented character classes,\nstring2 must map all characters in the domain to one")
		}
	}
	return nil
}

// trCaseClassesAligned reports whether every [:upper:] or [:lower:] in SET2
// starts at the same byte position as an [:upper:] or [:lower:] in SET1.
func trCaseClassesAligned(s1, s2 *trSet) bool {
	starts := map[int]bool{}
	at := 0
	for _, el := range s1.elements {
		if el.kind == trClass && isTrCaseClass(el) {
			starts[at] = true
		}
		at += len(el.bytes())
	}
	at = 0
	for _, el := range s2.elements {
		if el.kind == trClass && !starts[at] {
			return false
		}
		at += len(el.bytes())
	}
	return true
}

func trClassContains(class string, c byte) bool {
	switch class {
	case "alnum":
		return trClassContains("alpha", c) || trClassContains("digit", c)
	case "alpha":
		return trClassContains("upper", c) || trClassContains("lower", c)
	case "blank":
		return c == ' ' || c == '\t'
	case "cntrl":
		return c < 0x20 || c == 0x7f
	case "digit":
		return c >= '0' && c <= '9'
	case "graph":
		return c > ' ' && c < 0x7f
	case "lower":
		return c >= 'a' && c <= 'z'
	case "print":
		return c >= ' ' && c < 0x7f
	case "punct":
		return trClassContains("graph", c) && !trClassContains("alnum", c)
	case "space":
		return c == ' ' || c >= '\t' && c <= '\r'
	case "upper":
		return c >= 'A' && c <= 'Z'
	case "xdigit":
		return trClassContains("digit", c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	}
	return false
}

func isTrClassName(name string) bool {
	switch name {
	case "alnum", "alpha", "blank", "cntrl", "digit", "graph", "lower", "print", "punct", "space", "upper", "xdigit":
		return true
	}
	return false
}

// trString is a SET operand after backslash processing. escaped marks bytes
// that came from an escape, which never act as syntax ('-', '[', ':', ...).
type trString struct {
	s       []byte
	escaped []bool
}

func (t trString) is(i int, c byte) bool {
	return i < len(t.s) && t.s[i] == c && !t.escaped[i]
}

// unquoteTrSet processes backslash escapes as GNU tr's unquote does, writing
// its portability warnings to stderr.
func unquoteTrSet(e *Env, value string) trString {
	var out trString
	add := func(c byte, escaped bool) {
		out.s = append(out.s, c)
		out.escaped = append(out.escaped, escaped)
	}
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			add(value[i], false)
			continue
		}
		if i+1 == len(value) {
			_, _ = fmt.Fprint(e.Stderr, "tr: warning: an unescaped backslash at end of string is not portable\n")
			add('\\', false)
			continue
		}
		i++
		c := value[i]
		switch c {
		case 'a':
			c = '\a'
		case 'b':
			c = '\b'
		case 'f':
			c = '\f'
		case 'n':
			c = '\n'
		case 'r':
			c = '\r'
		case 't':
			c = '\t'
		case 'v':
			c = '\v'
		case '0', '1', '2', '3', '4', '5', '6', '7':
			n := int(c - '0')
			if i+1 < len(value) && isOctalDigit(value[i+1]) {
				n = n*8 + int(value[i+1]-'0')
				i++
				if i+1 < len(value) && isOctalDigit(value[i+1]) {
					if next := n*8 + int(value[i+1]-'0'); next <= 0xff {
						n = next
						i++
					} else {
						_, _ = fmt.Fprintf(e.Stderr, "tr: warning: the ambiguous octal escape \\%c%c%c is being\n\tinterpreted as the 2-byte sequence \\0%c%c, %c\n",
							value[i-1], value[i], value[i+1], value[i-1], value[i], value[i+1])
					}
				}
			}
			c = byte(n)
		}
		add(c, true)
	}
	return out
}

func isOctalDigit(c byte) bool { return c >= '0' && c <= '7' }

// parseTrSet parses a SET operand following GNU tr's build_spec_list.
func parseTrSet(e *Env, value string) (*trSet, error) {
	es := unquoteTrSet(e, value)
	set := &trSet{}
	i := 0
	for i+2 < len(es.s) {
		if es.is(i, '[') {
			if es.is(i+1, ':') || es.is(i+1, '=') {
				if closing, ok := trClosingDelim(es, i+2, es.s[i+1]); ok {
					operand := es.s[i+2 : closing]
					if es.s[i+1] == ':' {
						if isTrClassName(string(operand)) {
							set.elements = append(set.elements, trElement{kind: trClass, class: string(operand)})
							i = closing + 2
							continue
						}
						if !trStarDigitsCloseBracket(es, i+2) {
							return nil, fmt.Errorf("invalid character class '%s'", trPrintable(operand))
						}
					} else {
						if len(operand) == 1 {
							set.elements = append(set.elements, trElement{kind: trEquiv, lo: operand[0]})
							i = closing + 2
							continue
						}
						if !trStarDigitsCloseBracket(es, i+2) {
							return nil, fmt.Errorf("%s: equivalence class operand must be a single character", trPrintable(operand))
						}
					}
				}
			}
			el, closing, found, err := trBracketedRepeat(es, i+1)
			if err != nil {
				return nil, err
			}
			if found {
				set.elements = append(set.elements, el)
				i = closing + 1
				continue
			}
		}
		if es.is(i+1, '-') {
			lo, hi := es.s[i], es.s[i+2]
			if lo > hi {
				return nil, fmt.Errorf("range-endpoints of '%s-%s' are in reverse collating sequence order", trPrintable([]byte{lo}), trPrintable([]byte{hi}))
			}
			set.elements = append(set.elements, trElement{kind: trRange, lo: lo, hi: hi})
			i += 3
			continue
		}
		set.elements = append(set.elements, trElement{kind: trChar, lo: es.s[i]})
		i++
	}
	for ; i < len(es.s); i++ {
		set.elements = append(set.elements, trElement{kind: trChar, lo: es.s[i]})
	}
	return set, nil
}

// trClosingDelim finds the unescaped "delim]" that closes [:...:] or [=...=].
func trClosingDelim(es trString, start int, delim byte) (int, bool) {
	for i := start; i+1 < len(es.s); i++ {
		if es.is(i, delim) && es.is(i+1, ']') {
			return i, true
		}
	}
	return 0, false
}

// trStarDigitsCloseBracket reports whether es[at:] looks like "*digits]", so
// that "[:*3]" is read as a repeat of ':' rather than a bad class name.
func trStarDigitsCloseBracket(es trString, at int) bool {
	if !es.is(at, '*') {
		return false
	}
	for i := at + 1; i < len(es.s); i++ {
		if es.s[i] < '0' || es.s[i] > '9' || es.escaped[i] {
			return es.is(i, ']')
		}
	}
	return false
}

// trBracketedRepeat matches [c*] or [c*n] with c at es[start]. A count with
// a leading zero is octal; [c*] and [c*0] are indefinite (count 0).
func trBracketedRepeat(es trString, start int) (trElement, int, bool, error) {
	if !es.is(start+1, '*') {
		return trElement{}, 0, false, nil
	}
	for i := start + 2; i < len(es.s) && !es.escaped[i]; i++ {
		if es.s[i] != ']' {
			continue
		}
		el := trElement{kind: trRepeat, lo: es.s[start]}
		digits := string(es.s[start+2 : i])
		if digits != "" {
			base := 10
			if digits[0] == '0' {
				base = 8
			}
			n, err := strconv.ParseUint(digits, base, 31)
			if err != nil || strings.ContainsAny(digits[:1], "+-") {
				return trElement{}, 0, false, fmt.Errorf("invalid repeat count '%s' in [c*n] construct", trPrintable([]byte(digits)))
			}
			el.count = int(n)
		}
		return el, i, true, nil
	}
	return trElement{}, 0, false, nil
}

// trPrintable renders bytes for diagnostics as GNU tr's make_printable_str.
func trPrintable(bytes []byte) string {
	var b strings.Builder
	for _, c := range bytes {
		switch c {
		case '\\':
			b.WriteString(`\`)
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\v':
			b.WriteString(`\v`)
		default:
			if c >= ' ' && c < 0x7f {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, `\%03o`, c)
			}
		}
	}
	return b.String()
}

// trFilter streams stdin to stdout, translating or deleting bytes and then
// squeezing runs of bytes in the squeeze set.
type trFilter struct {
	translate *[256]byte
	remove    *[256]bool
	squeeze   *[256]bool
}

func (f trFilter) run(ctx context.Context, r io.Reader, w io.Writer) error {
	out := bufio.NewWriterSize(w, 32<<10)
	buf := make([]byte, 32<<10)
	last := -1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := r.Read(buf)
		for _, c := range buf[:n] {
			if f.remove != nil && f.remove[c] {
				continue
			}
			if f.translate != nil {
				c = f.translate[c]
			}
			if f.squeeze != nil && f.squeeze[c] && int(c) == last {
				continue
			}
			last = int(c)
			if err := out.WriteByte(c); err != nil {
				return fmt.Errorf("write error: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read error: %w", readErr)
		}
	}
	if err := out.Flush(); err != nil {
		return fmt.Errorf("write error: %w", err)
	}
	return nil
}
