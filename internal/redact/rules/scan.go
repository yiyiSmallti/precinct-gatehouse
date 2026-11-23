package rules

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Running every pattern over the whole text made redaction cost about
// 0.25 ms per KB, which on a long agent history (hundreds of KB of code and
// logs) added hundreds of milliseconds per request. A scope narrows each
// pattern to the parts of the text it could match, found with literal scans,
// so that the regexps only run where needed. A scope must never drop a
// match: it returns the whole text when it cannot decide.

// scope is where a pattern runs: the whole text, a list of disjoint spans
// (the pattern's unanchored regexp runs inside each), or a list of match
// starts (the anchored regexp runs at each).
type scope struct {
	whole  bool
	spans  [][2]int
	starts []int
}

type scopeFunc func(t *scanText) scope

var wholeText = scope{whole: true}

// scanText is the text being scanned plus lazily computed helpers.
type scanText struct {
	s       string
	lowerOK bool
	lower   string
	done    bool
	digits  [][2]int // digitRuns, shared by the number patterns
}

func newScanText(s string) *scanText { return &scanText{s: s} }

// lowerText returns the lower-cased text when its byte offsets match the
// original and (?i) matching cannot reach a letter through a non-ASCII fold
// (U+017F ſ folds to s, U+212A K to k); ok is false otherwise.
func (t *scanText) lowerText() (string, bool) {
	if !t.done {
		t.done = true
		if !strings.ContainsRune(t.s, '\u017f') && !strings.ContainsRune(t.s, '\u212a') {
			t.lower, t.lowerOK = LowerSameOffsets(t.s)
		}
	}
	return t.lower, t.lowerOK
}

// LowerSameOffsets lower-cases s and reports whether every byte offset of
// the result matches s, that is whether each character kept its encoded
// width (İ lowers to the one-byte i, Ⱥ to the three-byte ⱥ: equal total
// lengths do not imply equal offsets). Invalid UTF-8 bytes are copied as is.
func LowerSameOffsets(s string) (string, bool) {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s), true
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			b.WriteByte(s[i])
			i++
			continue
		}
		l := unicode.ToLower(r)
		if utf8.RuneLen(l) != n {
			return "", false
		}
		b.WriteRune(l)
		i += n
	}
	return b.String(), true
}

// run reports every match of p within its scope, exactly as
// p.re.FindAllStringSubmatchIndex over the whole text would.
func (t *scanText) run(p pattern, record func(pattern, []int)) {
	sc := wholeText
	if p.scope != nil {
		sc = p.scope(t)
	}
	shift := func(m []int, off int) []int {
		for i := range m {
			if m[i] >= 0 {
				m[i] += off
			}
		}
		return m
	}
	switch {
	case sc.whole:
		for _, m := range p.re.FindAllStringSubmatchIndex(t.s, -1) {
			record(p, m)
		}
	case sc.spans != nil:
		for _, sp := range sc.spans {
			for _, m := range p.re.FindAllStringSubmatchIndex(t.s[sp[0]:sp[1]], -1) {
				record(p, shift(m, sp[0]))
			}
		}
	default:
		last := 0
		for _, st := range sc.starts {
			if st < last {
				continue // inside the previous match, as FindAll skips it
			}
			m := p.anchored.FindStringSubmatchIndex(t.s[st:])
			if m == nil {
				continue
			}
			m = shift(m, st)
			record(p, m)
			last = max(m[1], st+1)
		}
	}
}

// anchoredPattern sets p.re and its anchored form from expr.
func anchoredPattern(p pattern, expr string) pattern {
	p.re = regexp.MustCompile(expr)
	p.anchored = regexp.MustCompile(`^(?:` + expr + `)`)
	return p
}

// ifContains runs the pattern on the whole text when it contains any of lits.
func ifContains(lits ...string) scopeFunc {
	return func(t *scanText) scope {
		for _, l := range lits {
			if strings.Contains(t.s, l) {
				return wholeText
			}
		}
		return scope{spans: [][2]int{}}
	}
}

// ifContainsAll runs the pattern on the whole text when it contains all lits.
func ifContainsAll(lits ...string) scopeFunc {
	return func(t *scanText) scope {
		for _, l := range lits {
			if !strings.Contains(t.s, l) {
				return scope{spans: [][2]int{}}
			}
		}
		return wholeText
	}
}

// ifContainsFold is ifContains for a lower-case literal under (?i).
func ifContainsFold(lit string) scopeFunc {
	return func(t *scanText) scope {
		lower, ok := t.lowerText()
		if !ok || strings.Contains(lower, lit) {
			return wholeText
		}
		return scope{spans: [][2]int{}}
	}
}

// runs returns the maximal runs of bytes accepted by in that satisfy keep.
// Every match of a pattern whose characters are all accepted by in lies
// inside one run; when in accepts every \w byte, \b at a run's edges means
// the same as in the whole text.
func runs(s string, in func(byte) bool, keep func(i, j int) bool) [][2]int {
	out := [][2]int{}
	for i := 0; i < len(s); {
		if !in(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && in(s[j]) {
			j++
		}
		if keep(i, j) {
			out = append(out, [2]int{i, j})
		}
		i = j
	}
	return out
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isWordByte(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// digitRuns scopes the number patterns (ID card, USCC, phone, bank card),
// whose characters are ASCII letters, digits, '+', '-' and ' ', and which
// all need at least six digits.
func digitRuns(t *scanText) scope {
	if t.digits == nil {
		t.digits = runs(t.s,
			func(c byte) bool { return isWordByte(c) || c == '+' || c == '-' || c == ' ' },
			func(i, j int) bool {
				n := 0
				for ; i < j; i++ {
					if isDigit(t.s[i]) {
						if n++; n >= 6 {
							return true
						}
					}
				}
				return false
			})
	}
	return scope{spans: t.digits}
}

// emailRuns scopes e-mail addresses to runs of address characters with '@'.
func emailRuns(t *scanText) scope {
	if !strings.Contains(t.s, "@") {
		return scope{spans: [][2]int{}}
	}
	return scope{spans: runs(t.s,
		func(c byte) bool { return isWordByte(c) || strings.IndexByte(".%+-@", c) >= 0 },
		func(i, j int) bool { return strings.IndexByte(t.s[i:j], '@') >= 0 })}
}

// ipRuns scopes private IPv4 addresses to runs of word bytes and dots that
// hold at least three dots.
func ipRuns(t *scanText) scope {
	return scope{spans: runs(t.s,
		func(c byte) bool { return isWordByte(c) || c == '.' },
		func(i, j int) bool { return strings.Count(t.s[i:j], ".") >= 3 })}
}

// domainRuns scopes internal host names to runs of host characters that
// contain one of the suffixes. Non-ASCII suffixes or (?i) folds through
// non-ASCII letters fall back to the whole text.
func domainRuns(suffix []string) scopeFunc {
	return func(t *scanText) scope {
		for _, sfx := range suffix {
			if !isASCII(sfx) {
				return wholeText
			}
		}
		lower, ok := t.lowerText()
		if !ok {
			return wholeText
		}
		return scope{spans: runs(t.s,
			func(c byte) bool { return isWordByte(c) || c == '.' || c == '-' },
			func(i, j int) bool {
				if strings.IndexByte(t.s[i:j], '.') < 0 {
					return false
				}
				for _, sfx := range suffix {
					if strings.Contains(lower[i:j], sfx) {
						return true
					}
				}
				return false
			})}
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// kvTriggers start every keyword of the key/value secret pattern.
var kvTriggers = []string{"pass", "pwd", "secret", "api", "access", "private", "auth", "client", "密码", "口令", "密钥"}

// kvStarts returns where a key/value secret keyword starts.
func kvStarts(t *scanText) scope {
	lower, ok := t.lowerText()
	if !ok {
		return wholeText
	}
	var starts []int
	for _, k := range kvTriggers {
		for off := 0; ; {
			i := strings.Index(lower[off:], k)
			if i < 0 {
				break
			}
			starts = append(starts, off+i)
			off += i + len(k)
		}
	}
	sort.Ints(starts)
	return scope{starts: starts}
}

var plateProvinces = func() map[rune]bool {
	m := map[rune]bool{}
	for _, r := range "京津沪渝冀豫云辽黑湘皖鲁新苏浙赣鄂桂甘晋蒙陕吉闽贵粤青藏川宁琼" {
		m[r] = true
	}
	return m
}()

// plateStarts returns where a province character is followed by an
// upper-case letter, as every licence plate starts.
func plateStarts(t *scanText) scope {
	starts := []int{}
	s := t.s
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if i+n < len(s) && s[i+n] >= 'A' && s[i+n] <= 'Z' && plateProvinces[r] {
			starts = append(starts, i)
		}
		i += n
	}
	return scope{starts: starts}
}
