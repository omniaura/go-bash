#!/usr/bin/env python3
"""Record GNU bash + coreutils behaviour as Go test fixtures.

Run on a Linux host with GNU bash and coreutils (the checked-in fixtures were
recorded on debby, Linux/amd64):

    python3 testdata/gnu/record.py printf > cmd_printf_gnu_cases_test.go
    python3 testdata/gnu/record.py tr > cmd_tr_gnu_cases_test.go

Each script runs as `bash -c SCRIPT` in an empty temporary directory with a
minimal environment and LC_ALL=C unless the case names another locale.
"""
import os
import subprocess
import sys
import tempfile

# (name, script[, locale]). Scripts are single-quoted shell so the exact
# escape text reaches printf/tr.
PRINTF_CASES = [
    # Octal escapes in the format: \N, \NN, \NNN (first digit 0-7).
    ("format octal utf8 e acute", r"printf '\303\251'"),
    ("format octal letters", r"printf '\101\102\103'"),
    ("format octal one digit", r"printf '\1|\7|\0|'"),
    ("format octal two digits", r"printf '\12\41'"),
    ("format octal stops after three digits", r"printf '\1011'"),
    ("format octal leading zero takes three digits", r"printf '\0101'"),
    ("format octal zero then escape", r"printf '\0\0377A'"),
    ("format octal overflow wraps", r"printf '\400|\777|\477'"),
    ("format octal high bytes", r"printf '\200\377\376'"),
    ("format octal stops at 8", r"printf '\18|\8|\9'"),
    ("format nul between text", r"printf 'a\0b'"),
    # Hex escapes: one or two digits.
    ("format hex", r"printf '\x41\x61\x7e'"),
    ("format hex one digit", r"printf '\x4|\xa|'"),
    ("format hex stops after two digits", r"printf '\x414'"),
    ("format hex upper and lower digits", r"printf '\xFF\xfe\xAb'"),
    ("format hex missing digit", r"printf 'a\xg'"),
    ("format hex at end", r"printf 'a\x'"),
    # Unicode escapes.
    ("format unicode ascii", r"printf '\u0041\u7e\U00000042'"),
    ("format unicode stops after four digits", r"printf '\u00411'"),
    ("format unicode short", r"printf '\u41|\U43|'"),
    ("format unicode nul", r"printf 'a\u0000b'"),
    ("format unicode missing digit", r"printf 'a\uzz'"),
    ("format unicode upper missing digit", r"printf 'a\U'"),
    ("format unicode e acute utf8", r"printf '\u00e9'", "C.UTF-8"),
    ("format unicode euro utf8", r"printf '\u20ac|\u20AC'", "C.UTF-8"),
    ("format unicode astral utf8", r"printf '\U0001F600'", "C.UTF-8"),
    ("format unicode two byte boundary utf8", r"printf '\u0080\u07ff\u0800'", "C.UTF-8"),
    ("format unicode stops after eight digits utf8", r"printf '\U0001F6001'", "C.UTF-8"),
    ("format unicode surrogate utf8", r"printf '\uD800|\uDFFF'", "C.UTF-8"),
    ("format unicode beyond max utf8", r"printf '\U00110000|\U001FFFFF|\U03FFFFFF'", "C.UTF-8"),
    ("format unicode 31 bit utf8", r"printf '\U7FFFFFFF|\U80000000|\UFFFFFFFF|'", "C.UTF-8"),
    ("format unicode max utf8", r"printf '\U0010FFFF|\uFFFF'", "C.UTF-8"),
    # Single-character escapes.
    ("format simple escapes", r"printf '\a\b\e\E\f\n\r\t\v'"),
    ("format backslash and quotes", r"""printf '\\|\"|\?|'"'"'\'"'"''"""),
    ("format unknown escapes pass through", r"printf '\q\z\c\%%'"),
    ("format trailing backslash", r"printf 'a\'"),
    ("format escapes mixed with directives", r"printf '\101%s\x42%d\n' x 7"),
    ("format escapes repeat per argument", r"printf '\101%s\n' a b"),
    # %b arguments.
    ("b octal utf8 e acute", r"printf '%b' '\303\251'"),
    ("b octal without leading zero", r"printf '%b' '\101\7\12'"),
    ("b octal leading zero takes three more", r"printf '%b' '\0101|\01234|\0'"),
    ("b octal zero overflow", r"printf '%b' '\0400|\0777'"),
    ("b hex and unicode ascii", r"printf '%b' '\x41\x4\u0042\U00000043'"),
    ("b hex missing digit", r"printf '%b' 'a\xg'"),
    ("b unicode missing digit", r"printf '%b' 'a\u'"),
    ("b unicode utf8", r"printf '%b' '\u00e9\U0001F600'", "C.UTF-8"),
    ("b simple escapes", r"printf '%b' '\a\b\e\E\f\n\r\t\v\\'"),
    ("b quotes are not escapes", r"""printf '%b' '\"\?'"'"'\'"'"''"""),
    ("b unknown escape", r"printf '%b' '\q\z'"),
    ("b trailing backslash", r"printf '%b' 'a\'"),
    ("b stop output", r"printf '%b|%s\n' 'a\cb' x"),
    ("b stop output across arguments", r"printf '%b\n' a 'b\c' c"),
    ("b width", r"printf '[%5b][%-5b]\n' '\101' '\x42'"),
    ("b precision counts decoded bytes", r"printf '[%.2b]\n' '\101\102\103'"),
    ("b escaped percent", r"printf '%b\n' '100%'"),
    ("s does not decode", r"printf '%s\n' '\101\x41'"),
    ("q does not decode", r"printf '%q\n' '\101'"),
]

TR_CASES = [
    # Octal escapes.
    ("octal translate", r"printf 'a\001b\002c' | tr '\001\002' 'XY'"),
    ("octal one digit", r"printf 'a\001b' | tr '\1' 'X'"),
    ("octal two digits", r"printf 'a\011b' | tr '\11' 'X'"),
    ("octal nul to newline", r"printf 'a\0b\0c' | tr '\000' '\n'"),
    ("octal nul short", r"printf 'a\0b' | tr '\0' 'x'"),
    ("octal to octal", r"printf 'a b\n' | tr ' ' '\012'"),
    ("octal letters", r"printf 'abc' | tr '\141\142' '\101\102'"),
    ("octal four digits", r"printf 'A1B' | tr '\1011' 'xy'"),
    ("octal ambiguous above 0377", r"printf 'a 0b' | tr -d '\400'"),
    ("octal 0377", r"printf 'a\377b' | tr '\377' 'X'"),
    ("octal high bytes", r"printf 'caf\303\251\n' | tr '\303\251' 'XY'"),
    ("octal digit 8 is literal", r"printf 'a8\0' | tr '\8' 'X'"),
    # Backslash escapes.
    ("backslash escapes", r"printf 'a\a\b\f\n\r\t\vz' | tr '\a\b\f\n\r\t\v' 'ABFNRTV'"),
    ("escaped backslash", r"printf 'x\\y' | tr '\\' '/'"),
    ("unknown escape is literal", r"printf 'qz' | tr '\q' 'Q'"),
    ("backslash n deletes newline", r"printf 'a\nb\n' | tr -d '\n'"),
    ("backslash then n characters", r"printf 'a\\nb' | tr -d '\\n'"),
    ("trailing backslash", r"printf 'a\\b' | tr -d 'a\'"),
    ("escaped hyphen is literal", r"printf 'a-z m' | tr 'a\-z' '123'"),
    ("escaped hyphen delete", r"printf 'a-b-c' | tr -d '\-'"),
    ("leading hyphen after double dash", r"printf 'a-b' | tr -- '-a' '_A'"),
    ("trailing hyphen literal", r"printf 'a-b' | tr 'a-' 'Ax'"),
    # Ranges.
    ("range letters", r"printf 'hello\n' | tr 'a-y' 'b-z'"),
    ("range escaped endpoints delete", r"printf 'a\001\002\003\037 z\n' | tr -d '\001-\037'"),
    ("range octal endpoints", r"printf 'abcd' | tr '\141-\143' 'A-C'"),
    ("range mixed endpoints", r"printf 'abcd' | tr 'a-\143' 'x'"),
    ("range high bytes delete", r"printf 'caf\303\251!\n' | tr -d '\200-\377'"),
    ("range single char", r"printf 'abc' | tr 'b-b' 'X'"),
    ("range reversed", r"printf 'abc' | tr 'z-a' 'x'"),
    ("range reversed escaped", r"printf 'abc' | tr '\003-\001' 'x'"),
    # Classes, equivalence and repeats.
    ("class lower to upper", r"printf 'Hello, World 1!\n' | tr '[:lower:]' '[:upper:]'"),
    ("class upper to lower", r"printf 'Hello, World 1!\n' | tr '[:upper:]' '[:lower:]'"),
    ("class digit delete", r"printf 'a1b22c333\n' | tr -d '[:digit:]'"),
    ("class space delete", r"printf 'a b\tc\nd\re\vf\fg' | tr -d '[:space:]'"),
    ("class blank delete", r"printf 'a b\tc\nd' | tr -d '[:blank:]'"),
    ("class punct delete", r"printf 'a.b,c;d!e?f-g_h(i)\n' | tr -d '[:punct:]'"),
    ("class cntrl delete", r"printf 'a\001b\177c\td\n' | tr -d '[:cntrl:]'"),
    ("class xdigit delete", r"printf 'deadBEEFxyz09\n' | tr -d '[:xdigit:]'"),
    ("class alpha translate", r"printf 'ab12CD\n' | tr '[:alpha:]' 'x'"),
    ("class alnum complement delete", r"printf 'a-b_c 1\n' | tr -cd '[:alnum:]'"),
    ("class print complement delete", r"printf 'a\001b\tc d\n' | tr -cd '[:print:]'"),
    ("class graph complement delete", r"printf 'a\001b\tc d\n' | tr -cd '[:graph:]'"),
    ("class invalid", r"printf 'abc' | tr -d '[:nope:]'"),
    ("class in set2 not case", r"printf 'abc' | tr 'abc' '[:digit:]'"),
    ("class misaligned case", r"printf 'abc' | tr 'a-z' '[:upper:]'"),
    ("brackets without class are literal", r"printf '[a]:' | tr '[a]' 'xyz'"),
    ("escaped bracket is literal", r"printf '[:a:]' | tr -d '\[:a:]'"),
    ("equivalence class", r"printf 'banana' | tr '[=a=]' 'o'"),
    ("repeat fills set2", r"printf 'abcdef' | tr 'abcdef' 'x[y*]z'"),
    ("repeat count", r"printf 'abcdef' | tr 'abcdef' '[x*2][y*3]z'"),
    ("repeat octal count", r"printf 'abcdefghij' | tr 'abcdefghij' '[x*010]yz'"),
    ("repeat escaped char", r"printf 'abc' | tr 'abc' '[\101*]'"),
    ("repeat in set1 rejected", r"printf 'abc' | tr '[a*]' 'x'"),
    ("repeat invalid count", r"printf 'abc' | tr 'abc' '[x*9z]'"),
    ("two indefinite repeats rejected", r"printf 'abc' | tr 'abc' '[x*][y*]'"),
    # Translation shapes.
    ("set2 extended with last char", r"printf 'abcdef' | tr 'abcdef' 'xy'"),
    ("set2 longer is ignored", r"printf 'abc' | tr 'ab' 'wxyz'"),
    ("later duplicate wins", r"printf 'aab' | tr 'aa' 'xy'"),
    ("truncate set1", r"printf 'abcdef' | tr -t 'abcdef' 'xy'"),
    ("empty set2 rejected", r"printf 'abc' | tr 'abc' ''"),
    ("empty sets", r"printf 'abc' | tr '' ''"),
    ("newlines to spaces", r"printf 'a\nb\nc\n' | tr '\n' ' '"),
    # -c
    ("complement translate", r"printf 'ab-12 cd\n' | tr -c 'a-z\n' 'X'"),
    ("complement uppercase C", r"printf 'ab-12 cd\n' | tr -C 'a-z\n' '_'"),
    ("complement words to lines", r"printf 'Hello, big  world! 42\n' | tr -cs '[:alnum:]' '\n'"),
    ("complement class needs single target", r"printf 'abc' | tr -c '[:alpha:]' 'xy'"),
    ("complement delete keeps newline", r"printf 'a1\nb2\n' | tr -dc 'a-z\n'"),
    ("complement delete high bytes", r"printf 'caf\303\251\n' | tr -cd '\000-\177'"),
    # -s
    ("squeeze spaces", r"printf 'a   b    c\n' | tr -s ' '"),
    ("squeeze newlines", r"printf 'a\n\n\nb\n\n' | tr -s '\n'"),
    ("squeeze range", r"printf 'aabbccdd  ee\n' | tr -s 'a-c'"),
    ("squeeze after translate", r"printf 'aabbcc\n' | tr -s 'ab' 'xx'"),
    ("squeeze translate uses set2", r"printf 'aaxx\n' | tr -s 'a' 'b'"),
    ("squeeze complement", r"printf 'aa..bb,,\n' | tr -sc 'a-z'"),
    ("squeeze octal", r"printf 'a\001\001\001b' | tr -s '\001'"),
    ("delete then squeeze", r"printf 'aabbaacc\n' | tr -ds 'a' 'b'"),
    ("delete complement then squeeze", r"printf 'xx11yy22\n' | tr -dcs '0-9\n' '0-9'"),
    ("squeeze long option", r"printf 'a  b\n' | tr --squeeze-repeats ' '"),
    ("delete long option", r"printf 'abc\n' | tr --delete 'b'"),
    ("complement long option", r"printf 'abc\n' | tr --complement -d 'b'"),
    ("truncate long option", r"printf 'abc' | tr --truncate-set1 'abc' 'x'"),
    # Operand errors.
    ("no operands", r"printf 'abc' | tr"),
    ("translate needs two", r"printf 'abc' | tr abc"),
    ("delete takes one", r"printf 'abc' | tr -d a b"),
    ("delete squeeze needs two", r"printf 'abc' | tr -ds a"),
    ("too many operands", r"printf 'abc' | tr a b c"),
    ("repeat with delete squeeze rejected", r"printf 'abc' | tr -ds a '[b*]'"),
    ("invalid option", r"printf 'abc' | tr -z a b"),
    # Empty input.
    ("empty input", r"printf '' | tr a b"),
]


def go_string(data: bytes) -> str:
    out = ['"']
    for b in data:
        c = chr(b)
        if c == '"':
            out.append('\\"')
        elif c == '\\':
            out.append('\\\\')
        elif c == '\n':
            out.append('\\n')
        elif c == '\t':
            out.append('\\t')
        elif 0x20 <= b < 0x7f:
            out.append(c)
        else:
            out.append('\\x%02x' % b)
    out.append('"')
    return ''.join(out)


def go_raw(text: str) -> str:
    if '`' in text:
        return go_string(text.encode())
    return '`' + text + '`'


def record(cases):
    rows = []
    for case in cases:
        name, script = case[0], case[1]
        locale = case[2] if len(case) > 2 else "C"
        with tempfile.TemporaryDirectory() as tmp:
            env = {"PATH": "/usr/bin:/bin", "LC_ALL": locale, "HOME": tmp}
            proc = subprocess.run(["bash", "-c", script], cwd=tmp, env=env,
                                  capture_output=True, timeout=10)
        rows.append((name, script, locale, proc.stdout, proc.stderr, proc.returncode))
    return rows


def main():
    which = sys.argv[1]
    cases = {"printf": PRINTF_CASES, "tr": TR_CASES}[which]
    rows = record(cases)
    bash = subprocess.run(["bash", "-c", "echo $BASH_VERSION"], capture_output=True, text=True).stdout.strip()
    tr = subprocess.run(["tr", "--version"], capture_output=True, text=True).stdout.splitlines()[0]
    var = which + "GNUCases"
    print("// Code generated by testdata/gnu/record.py from real GNU bash and coreutils; DO NOT EDIT BY HAND.")
    if which == "printf":
        print("// Source: GNU bash %s printf builtin, bash on Linux/amd64 (host debby)." % bash)
    else:
        print("// Source: %s, GNU bash %s on Linux/amd64 (host debby)." % (tr, bash))
    print("// Each script ran as `bash -c` in an empty temporary directory with LC_ALL=C")
    print("// unless locale says otherwise; stdout, stderr and the exit status are what")
    print("// GNU bash + coreutils produced.")
    print()
    print("package gobash")
    print()
    print("var %s = []struct {" % var)
    print("\tname       string")
    print("\tscript     string")
    print("\tlocale     string")
    print("\twant       string")
    print("\twantStderr string")
    print("\twantExit   int")
    print("}{")
    for name, script, locale, out, err, code in rows:
        print("\t{name: %s, script: %s, locale: %s, want: %s, wantStderr: %s, wantExit: %d}," % (
            go_string(name.encode()), go_raw(script), go_string(locale.encode()),
            go_string(out), go_string(err), code))
    print("}")


if __name__ == "__main__":
    main()
