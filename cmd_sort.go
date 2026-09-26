package gobash

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
)

func init() { Register("sort", cmdSort) }

// sortUsage lists the forms cmdSort implements. Everything else is rejected
// with "unsupported option" rather than being silently ignored.
const sortUsage = `usage: sort [-bdfhiMnrsuV] [-k POS1[,POS2]]... [-t SEP] [-o FILE] [FILE]...
Sort lines of text (GNU coreutils semantics in the C locale).
  -b, --ignore-leading-blanks  ignore leading blanks when finding keys
  -d, --dictionary-order       consider only blanks and alphanumerics
  -f, --ignore-case            fold lower case to upper case
  -h, --human-numeric-sort     compare human readable numbers (2K, 1G)
  -i, --ignore-nonprinting     consider only printable characters
  -M, --month-sort             compare JAN < ... < DEC
  -n, --numeric-sort           compare the leading numeric prefix
  -V, --version-sort           natural sort of version numbers
      --sort=WORD              numeric, human-numeric, month or version
  -r, --reverse                reverse the result of comparisons
  -s, --stable                 disable the last-resort whole-line comparison
  -u, --unique                 output only the first of an equal run
  -k, --key=POS1[,POS2]        sort by a key; POS is F[.C][OPTS], where OPTS
                               are b, d, f, h, i, M, n, r and V
  -t, --field-separator=SEP    use SEP instead of blank-to-nonblank transitions
  -o, --output=FILE            write the result to FILE instead of stdout
      --help                   display this help and exit`

// sortNoPos marks a key that starts at the beginning of the line (-k1) or ends
// at the end of the line (no POS2). GNU sort uses SIZE_MAX for the same role.
const sortNoPos = -1

// sortDefaultTab means fields are separated by blank-to-nonblank transitions.
const sortDefaultTab = -1

type sortIgnore uint8

const (
	sortIgnoreNone sortIgnore = iota
	sortIgnoreNondictionary
	sortIgnoreNonprinting
)

// sortKey mirrors struct keyfield in GNU sort. Field and character indexes
// are zero-based, as they are after GNU's option parsing.
type sortKey struct {
	sword, schar int
	eword, echar int

	skipsBlanks bool
	skipeBlanks bool
	ignore      sortIgnore
	fold        bool
	numeric     bool
	human       bool
	month       bool
	version     bool
	reverse     bool
}

// defaultCompare reports whether no ordering option is set on the key; like
// GNU sort, reverse alone does not count.
func (k *sortKey) defaultCompare() bool {
	return k.ignore == sortIgnoreNone && !k.fold && !k.skipsBlanks && !k.skipeBlanks &&
		!k.numeric && !k.human && !k.month && !k.version
}

// opts renders the key's ordering flags the way GNU's key_to_opts does.
func (k *sortKey) opts() string {
	var b strings.Builder
	if k.skipsBlanks || k.skipeBlanks {
		b.WriteByte('b')
	}
	if k.ignore == sortIgnoreNondictionary {
		b.WriteByte('d')
	}
	if k.fold {
		b.WriteByte('f')
	}
	if k.human {
		b.WriteByte('h')
	}
	if k.ignore == sortIgnoreNonprinting {
		b.WriteByte('i')
	}
	if k.month {
		b.WriteByte('M')
	}
	if k.numeric {
		b.WriteByte('n')
	}
	if k.reverse {
		b.WriteByte('r')
	}
	if k.version {
		b.WriteByte('V')
	}
	return b.String()
}

type sortOptions struct {
	global    sortKey
	keys      []*sortKey
	tab       int
	unique    bool
	stable    bool
	output    string
	hasOutput bool
	help      bool
	operands  []string
}

type sortBlankRole uint8

const (
	sortBlankStart sortBlankRole = 1 << iota
	sortBlankEnd
	sortBlankBoth = sortBlankStart | sortBlankEnd
)

// setOrdering consumes ordering flag characters from s, like GNU's
// set_ordering, and returns the unconsumed remainder.
func (k *sortKey) setOrdering(s string, role sortBlankRole) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 'b':
			if role&sortBlankStart != 0 {
				k.skipsBlanks = true
			}
			if role&sortBlankEnd != 0 {
				k.skipeBlanks = true
			}
		case 'd':
			k.ignore = sortIgnoreNondictionary
		case 'f':
			k.fold = true
		case 'h':
			k.human = true
		case 'i':
			// -d implies -i, but -i must not override -d.
			if k.ignore == sortIgnoreNone {
				k.ignore = sortIgnoreNonprinting
			}
		case 'M':
			k.month = true
		case 'n':
			k.numeric = true
		case 'r':
			k.reverse = true
		case 'V':
			k.version = true
		default:
			return s[i:]
		}
	}
	return ""
}

func cmdSort(ctx context.Context, e *Env) int {
	opts, err := parseSortArgs(e.Args[1:])
	if err != nil {
		e.Errorf("%v", err)
		return 2
	}
	if opts.help {
		_, _ = fmt.Fprintln(e.Stdout, sortUsage)
		return 0
	}

	var lines []string
	code := forEachInput(ctx, e, opts.operands, func(ctx context.Context, _ string, r io.Reader) error {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		lines = appendSortLines(lines, string(data))
		return ctx.Err()
	})
	if code != 0 {
		// GNU sort reports every input failure as trouble (exit 2) and
		// writes no partial output.
		return 2
	}

	s := newLineSorter(opts)
	items := make([]sortItem, len(lines))
	for i, line := range lines {
		items[i] = s.item(line)
	}
	slices.SortStableFunc(items, s.compare)
	if err := ctx.Err(); err != nil {
		e.Errorf("%v", err)
		return 2
	}

	out := e.Stdout
	var file io.WriteCloser
	if opts.hasOutput {
		f, err := e.FS.OpenFile(e.Resolve(opts.output), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			e.Errorf("open failed: %s: %v", opts.output, err)
			return 2
		}
		out, file = f, f
	}
	w := bufio.NewWriter(out)
	werr := writeSorted(w, s, items)
	if werr == nil {
		werr = w.Flush()
	}
	if file != nil {
		if cerr := file.Close(); werr == nil {
			werr = cerr
		}
	}
	if werr != nil {
		e.Errorf("write failed: %v", werr)
		return 2
	}
	return 0
}

func writeSorted(w *bufio.Writer, s *lineSorter, items []sortItem) error {
	for i := range items {
		// Like GNU write_unique: drop lines equal to the first line of the
		// current run under the active comparison (keys only when keyed).
		if s.opts.unique && i > 0 && s.compare(items[i], items[i-1]) == 0 {
			continue
		}
		if _, err := w.WriteString(items[i].line); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

// appendSortLines splits one input on newlines. A final line without a
// trailing newline still counts, and carriage returns are kept, as in GNU sort.
func appendSortLines(lines []string, text string) []string {
	for text != "" {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			return append(lines, text)
		}
		lines = append(lines, text[:i])
		text = text[i+1:]
	}
	return lines
}

func parseSortArgs(args []string) (*sortOptions, error) {
	opts := &sortOptions{tab: sortDefaultTab}
	opts.global = sortKey{sword: sortNoPos, eword: sortNoPos}
	endOfOptions := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case endOfOptions || arg == "-" || !strings.HasPrefix(arg, "-"):
			opts.operands = append(opts.operands, arg)
		case arg == "--":
			endOfOptions = true
		case strings.HasPrefix(arg, "--"):
			next, err := opts.parseLong(args, i)
			if err != nil {
				return nil, err
			}
			i = next
		default:
			next, err := opts.parseShortCluster(args, i)
			if err != nil {
				return nil, err
			}
			i = next
		}
	}
	if err := opts.finish(); err != nil {
		return nil, err
	}
	return opts, nil
}

// parseShortCluster handles one "-xyz" argument with getopt semantics: the
// value-taking options k, t and o consume the rest of the cluster or, when it
// is empty, the next argument.
func (o *sortOptions) parseShortCluster(args []string, i int) (int, error) {
	arg := args[i]
	for j := 1; j < len(arg); j++ {
		c := arg[j]
		switch c {
		case 'k', 't', 'o':
			value := arg[j+1:]
			if value == "" {
				if i+1 >= len(args) {
					return i, fmt.Errorf("option requires an argument -- '%c'", c)
				}
				i++
				value = args[i]
			}
			return i, o.applyValue(c, value)
		case 'b', 'd', 'f', 'h', 'i', 'M', 'n', 'r', 'V':
			o.global.setOrdering(string(c), sortBlankBoth)
		case 's':
			o.stable = true
		case 'u':
			o.unique = true
		default:
			return i, fmt.Errorf("unsupported option -- %c", c)
		}
	}
	return i, nil
}

var sortLongFlags = map[string]byte{
	"ignore-leading-blanks": 'b',
	"dictionary-order":      'd',
	"ignore-case":           'f',
	"human-numeric-sort":    'h',
	"ignore-nonprinting":    'i',
	"month-sort":            'M',
	"numeric-sort":          'n',
	"reverse":               'r',
	"version-sort":          'V',
	"stable":                's',
	"unique":                'u',
}

var sortLongValues = map[string]byte{
	"key":             'k',
	"field-separator": 't',
	"output":          'o',
	"sort":            'S',
}

var sortWords = map[string]byte{
	"human-numeric": 'h',
	"month":         'M',
	"numeric":       'n',
	"version":       'V',
}

func (o *sortOptions) parseLong(args []string, i int) (int, error) {
	name, value, hasValue := strings.Cut(args[i][2:], "=")
	if name == "help" && !hasValue {
		o.help = true
		return i, nil
	}
	if c, ok := sortLongFlags[name]; ok && !hasValue {
		switch c {
		case 's':
			o.stable = true
		case 'u':
			o.unique = true
		default:
			o.global.setOrdering(string(c), sortBlankBoth)
		}
		return i, nil
	}
	c, ok := sortLongValues[name]
	if !ok {
		return i, fmt.Errorf("unsupported option '--%s'", name)
	}
	if !hasValue {
		if i+1 >= len(args) {
			return i, fmt.Errorf("option '--%s' requires an argument", name)
		}
		i++
		value = args[i]
	}
	if c == 'S' {
		flag, ok := sortWords[value]
		if !ok {
			return i, fmt.Errorf("unsupported argument '%s' for '--sort'", value)
		}
		o.global.setOrdering(string(flag), sortBlankBoth)
		return i, nil
	}
	return i, o.applyValue(c, value)
}

func (o *sortOptions) applyValue(c byte, value string) error {
	switch c {
	case 'k':
		key, err := parseSortKey(value)
		if err != nil {
			return err
		}
		o.keys = append(o.keys, key)
	case 't':
		return o.setTab(value)
	case 'o':
		if o.hasOutput && o.output != value {
			return fmt.Errorf("multiple output files specified")
		}
		o.output, o.hasOutput = value, true
	}
	return nil
}

func (o *sortOptions) setTab(value string) error {
	if value == "" {
		return fmt.Errorf("empty tab")
	}
	tab := int(value[0])
	if len(value) > 1 {
		if value != `\0` {
			return fmt.Errorf("multi-character tab '%s'", value)
		}
		tab = 0
	}
	if o.tab != sortDefaultTab && o.tab != tab {
		return fmt.Errorf("incompatible tabs")
	}
	o.tab = tab
	return nil
}

// finish applies GNU's post-parse rules: keys without ordering options inherit
// the global ones, a non-default global ordering without -k becomes a
// whole-line key, and conflicting comparison types are rejected.
func (o *sortOptions) finish() error {
	for _, key := range o.keys {
		if key.defaultCompare() && !key.reverse {
			sword, schar, eword, echar := key.sword, key.schar, key.eword, key.echar
			*key = o.global
			key.sword, key.schar, key.eword, key.echar = sword, schar, eword, echar
		}
	}
	if len(o.keys) == 0 && !o.global.defaultCompare() {
		key := o.global
		o.keys = append(o.keys, &key)
	}
	for _, key := range o.keys {
		kinds := 0
		for _, set := range []bool{key.numeric, key.human, key.month, key.version || key.ignore != sortIgnoreNone} {
			if set {
				kinds++
			}
		}
		if kinds > 1 {
			shown := *key
			shown.skipsBlanks, shown.skipeBlanks, shown.reverse = false, false, false
			return fmt.Errorf("options '-%s' are incompatible", shown.opts())
		}
	}
	return nil
}

// parseSortKey parses a -k POS1[,POS2] specification exactly as GNU sort
// does, including its diagnostics.
func parseSortKey(spec string) (*sortKey, error) {
	key := &sortKey{}
	badSpec := func(msg string) error {
		return fmt.Errorf("%s: invalid field specification '%s'", msg, spec)
	}
	n, s, err := parseSortFieldCount(spec, "invalid number at field start")
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, badSpec("field number is zero")
	}
	key.sword = n - 1
	if strings.HasPrefix(s, ".") {
		if n, s, err = parseSortFieldCount(s[1:], "invalid number after '.'"); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, badSpec("character offset is zero")
		}
		key.schar = n - 1
	}
	if key.sword == 0 && key.schar == 0 {
		key.sword = sortNoPos
	}
	s = key.setOrdering(s, sortBlankStart)
	if !strings.HasPrefix(s, ",") {
		key.eword, key.echar = sortNoPos, 0
	} else {
		if n, s, err = parseSortFieldCount(s[1:], "invalid number after ','"); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, badSpec("field number is zero")
		}
		key.eword = n - 1
		if strings.HasPrefix(s, ".") {
			if n, s, err = parseSortFieldCount(s[1:], "invalid number after '.'"); err != nil {
				return nil, err
			}
			key.echar = n
		}
		s = key.setOrdering(s, sortBlankEnd)
	}
	if s != "" {
		return nil, badSpec("stray character in field spec")
	}
	return key, nil
}

// parseSortFieldCount parses a leading decimal count, saturating on overflow
// the way GNU sort substitutes SIZE_MAX.
func parseSortFieldCount(s, msg string) (int, string, error) {
	end := 0
	for end < len(s) && isSortDigit(s[end]) {
		end++
	}
	if end == 0 {
		return 0, s, fmt.Errorf("%s: invalid count at start of '%s'", msg, s)
	}
	n := 0
	for _, c := range []byte(s[:end]) {
		if n > (math.MaxInt32-int(c-'0'))/10 {
			n = math.MaxInt32
			break
		}
		n = n*10 + int(c-'0')
	}
	return n, s[end:], nil
}

// sortItem caches each key's extracted (and ignore/fold-translated) text so
// comparisons do not recompute field boundaries.
type sortItem struct {
	line string
	keys []string
}

type lineSorter struct {
	opts *sortOptions
}

func newLineSorter(opts *sortOptions) *lineSorter { return &lineSorter{opts: opts} }

func (s *lineSorter) item(line string) sortItem {
	it := sortItem{line: line}
	if len(s.opts.keys) == 0 {
		return it
	}
	it.keys = make([]string, len(s.opts.keys))
	for i, key := range s.opts.keys {
		beg, lim := s.keyBounds(line, key)
		it.keys[i] = translateSortKey(line[beg:lim], key)
	}
	return it
}

// compare is GNU's compare(): keys first, then (unless -s or -u) a
// last-resort byte comparison of whole lines, reversed only by global -r.
func (s *lineSorter) compare(a, b sortItem) int {
	if len(s.opts.keys) > 0 {
		for i, key := range s.opts.keys {
			if diff := compareSortKey(a.keys[i], b.keys[i], key); diff != 0 {
				if key.reverse {
					return -diff
				}
				return diff
			}
		}
		if s.opts.unique || s.opts.stable {
			return 0
		}
	}
	diff := strings.Compare(a.line, b.line)
	if s.opts.global.reverse {
		return -diff
	}
	return diff
}

func (s *lineSorter) keyBounds(line string, key *sortKey) (int, int) {
	lim := len(line)
	if key.eword != sortNoPos {
		lim = s.limField(line, key)
	}
	beg := 0
	if key.sword != sortNoPos {
		beg = s.begField(line, key)
	} else if key.skipsBlanks {
		for beg < lim && isSortBlank(line[beg]) {
			beg++
		}
	}
	// Treat field ends before field starts as empty fields.
	return beg, max(beg, lim)
}

// skipField advances ptr past one field: to just after the next separator
// with -t, otherwise past leading blanks and then the non-blank run.
func (s *lineSorter) skipField(line string, ptr int) int {
	lim := len(line)
	if s.opts.tab != sortDefaultTab {
		for ptr < lim && int(line[ptr]) != s.opts.tab {
			ptr++
		}
		return ptr
	}
	for ptr < lim && isSortBlank(line[ptr]) {
		ptr++
	}
	for ptr < lim && !isSortBlank(line[ptr]) {
		ptr++
	}
	return ptr
}

func (s *lineSorter) begField(line string, key *sortKey) int {
	ptr, lim := 0, len(line)
	for sword := key.sword; ptr < lim && sword > 0; sword-- {
		ptr = s.skipField(line, ptr)
		if s.opts.tab != sortDefaultTab && ptr < lim {
			ptr++
		}
	}
	if key.skipsBlanks {
		for ptr < lim && isSortBlank(line[ptr]) {
			ptr++
		}
	}
	return min(lim, ptr+key.schar)
}

func (s *lineSorter) limField(line string, key *sortKey) int {
	ptr, lim := 0, len(line)
	eword, echar := key.eword, key.echar
	if echar == 0 {
		eword++ // Skip all of the end field.
	}
	for ptr < lim && eword > 0 {
		eword--
		ptr = s.skipField(line, ptr)
		if s.opts.tab != sortDefaultTab && ptr < lim && (eword > 0 || echar > 0) {
			ptr++
		}
	}
	if echar != 0 {
		if key.skipeBlanks {
			for ptr < lim && isSortBlank(line[ptr]) {
				ptr++
			}
		}
		ptr = min(lim, ptr+echar)
	}
	return ptr
}

func translateSortKey(text string, key *sortKey) string {
	if key.ignore == sortIgnoreNone && !key.fold {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch key.ignore {
		case sortIgnoreNondictionary:
			if !isSortBlank(c) && !isSortAlnum(c) {
				continue
			}
		case sortIgnoreNonprinting:
			if c < 0x20 || c > 0x7e {
				continue
			}
		}
		if key.fold && 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func compareSortKey(a, b string, key *sortKey) int {
	switch {
	case key.numeric:
		return sortNumCompare(a, b)
	case key.human:
		return sortHumanCompare(a, b)
	case key.month:
		return sortMonth(a) - sortMonth(b)
	case key.version:
		return sortVersionCompare(a, b)
	default:
		return strings.Compare(a, b)
	}
}

// sortNumber is the leading numeric prefix GNU's strnumcmp understands in
// the C locale: optional '-', digits, optional '.' and digits. There is no
// '+' sign, exponent or thousands separator.
type sortNumber struct {
	negative bool
	integer  string // without leading zeros
	fraction string // without trailing zeros
}

func parseSortNumber(s string) sortNumber {
	var n sortNumber
	i := 0
	if i < len(s) && s[i] == '-' {
		n.negative = true
		i++
	}
	start := i
	for i < len(s) && isSortDigit(s[i]) {
		i++
	}
	n.integer = strings.TrimLeft(s[start:i], "0")
	if i < len(s) && s[i] == '.' {
		i++
		start = i
		for i < len(s) && isSortDigit(s[i]) {
			i++
		}
		n.fraction = strings.TrimRight(s[start:i], "0")
	}
	if n.integer == "" && n.fraction == "" {
		n.negative = false // -0 == 0, and a non-numeric prefix is zero
	}
	return n
}

func trimSortBlanks(s string) string {
	i := 0
	for i < len(s) && isSortBlank(s[i]) {
		i++
	}
	return s[i:]
}

func sortNumCompare(a, b string) int {
	x, y := parseSortNumber(trimSortBlanks(a)), parseSortNumber(trimSortBlanks(b))
	sign := func(n sortNumber) int {
		switch {
		case n.integer == "" && n.fraction == "":
			return 0
		case n.negative:
			return -1
		default:
			return 1
		}
	}
	if sx, sy := sign(x), sign(y); sx != sy {
		return sx - sy
	}
	diff := len(x.integer) - len(y.integer)
	if diff == 0 {
		diff = strings.Compare(x.integer, y.integer)
	}
	if diff == 0 {
		diff = strings.Compare(x.fraction, y.fraction)
	}
	if x.negative {
		return -diff
	}
	return diff
}

// sortUnitOrder mirrors GNU's unit_order table: K/k < M < G < T < P < E < Z <
// Y < R < Q.
func sortUnitOrder(c byte) int {
	switch c {
	case 'K', 'k':
		return 1
	case 'M':
		return 2
	case 'G':
		return 3
	case 'T':
		return 4
	case 'P':
		return 5
	case 'E':
		return 6
	case 'Z':
		return 7
	case 'Y':
		return 8
	case 'R':
		return 9
	case 'Q':
		return 10
	}
	return 0
}

// sortFindUnitOrder is GNU's find_unit_order: the unit byte after the number
// counts only when the number has a nonzero digit, negated for negatives.
func sortFindUnitOrder(s string) int {
	negative := strings.HasPrefix(s, "-")
	i := 0
	if negative {
		i++
	}
	maxDigit := byte(0)
	scan := func() {
		for i < len(s) && isSortDigit(s[i]) {
			maxDigit = max(maxDigit, s[i])
			i++
		}
	}
	scan()
	if i < len(s) && s[i] == '.' {
		i++
		scan()
	}
	if maxDigit <= '0' || i >= len(s) {
		return 0
	}
	order := sortUnitOrder(s[i])
	if negative {
		return -order
	}
	return order
}

func sortHumanCompare(a, b string) int {
	a, b = trimSortBlanks(a), trimSortBlanks(b)
	if diff := sortFindUnitOrder(a) - sortFindUnitOrder(b); diff != 0 {
		return diff
	}
	return sortNumCompare(a, b)
}

var sortMonths = [...]string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}

// sortMonth returns 1..12 for a leading (blank-skipped, case-insensitive)
// C-locale month abbreviation, else 0.
func sortMonth(s string) int {
	s = trimSortBlanks(s)
	if len(s) < 3 {
		return 0
	}
	var prefix [3]byte
	for i := range prefix {
		prefix[i] = s[i]
		if 'a' <= prefix[i] && prefix[i] <= 'z' {
			prefix[i] -= 'a' - 'A'
		}
	}
	for i, month := range sortMonths {
		if string(prefix[:]) == month {
			return i + 1
		}
	}
	return 0
}

// sortVersionCompare ports gnulib's filenvercmp, which GNU sort -V uses.
func sortVersionCompare(a, b string) int {
	if a == "" {
		if b == "" {
			return 0
		}
		return -1
	}
	if b == "" {
		return 1
	}
	// "." sorts first, then "..", then other names with a leading ".".
	if a[0] == '.' {
		if b[0] != '.' {
			return -1
		}
		if diff, decided := sortSpecialFirst(a == ".", b == "."); decided {
			return diff
		}
		if diff, decided := sortSpecialFirst(a == "..", b == ".."); decided {
			return diff
		}
	} else if b[0] == '.' {
		return 1
	}
	aPrefix, bPrefix := sortFilePrefixLen(a), sortFilePrefixLen(b)
	result := sortVerRevCmp(a[:aPrefix], b[:bPrefix])
	if result != 0 || (aPrefix == len(a) && bPrefix == len(b)) {
		return result
	}
	return sortVerRevCmp(a, b)
}

// sortSpecialFirst orders a special name before every other name. It reports
// whether either side was special and, if so, the comparison result.
func sortSpecialFirst(aSpecial, bSpecial bool) (int, bool) {
	switch {
	case aSpecial && bSpecial:
		return 0, true
	case aSpecial:
		return -1, true
	case bSpecial:
		return 1, true
	}
	return 0, false
}

// sortFilePrefixLen returns the length of s without its file suffix, the
// longest match of (\.[A-Za-z~][A-Za-z0-9~]*)*$.
func sortFilePrefixLen(s string) int {
	n := len(s)
	prefix := 0
	for i := 0; ; {
		if i == n {
			return prefix
		}
		i++
		prefix = i
		for i+1 < n && s[i] == '.' && (isSortAlpha(s[i+1]) || s[i+1] == '~') {
			i += 2
			for i < n && (isSortAlnum(s[i]) || s[i] == '~') {
				i++
			}
		}
	}
}

func sortVersionOrder(s string, pos int) int {
	if pos == len(s) {
		return -1
	}
	c := s[pos]
	switch {
	case isSortDigit(c):
		return 0
	case isSortAlpha(c):
		return int(c)
	case c == '~':
		return -2
	default:
		return int(c) + 256
	}
}

func sortVerRevCmp(s1, s2 string) int {
	p1, p2 := 0, 0
	for p1 < len(s1) || p2 < len(s2) {
		firstDiff := 0
		for (p1 < len(s1) && !isSortDigit(s1[p1])) || (p2 < len(s2) && !isSortDigit(s2[p2])) {
			c1, c2 := sortVersionOrder(s1, p1), sortVersionOrder(s2, p2)
			if c1 != c2 {
				return c1 - c2
			}
			p1++
			p2++
		}
		for p1 < len(s1) && s1[p1] == '0' {
			p1++
		}
		for p2 < len(s2) && s2[p2] == '0' {
			p2++
		}
		for p1 < len(s1) && p2 < len(s2) && isSortDigit(s1[p1]) && isSortDigit(s2[p2]) {
			if firstDiff == 0 {
				firstDiff = int(s1[p1]) - int(s2[p2])
			}
			p1++
			p2++
		}
		if p1 < len(s1) && isSortDigit(s1[p1]) {
			return 1
		}
		if p2 < len(s2) && isSortDigit(s2[p2]) {
			return -1
		}
		if firstDiff != 0 {
			return firstDiff
		}
	}
	return 0
}

// isSortBlank matches GNU sort's blanks table in the C locale.
func isSortBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }
func isSortDigit(c byte) bool { return '0' <= c && c <= '9' }
func isSortAlpha(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }
func isSortAlnum(c byte) bool { return isSortDigit(c) || isSortAlpha(c) }
