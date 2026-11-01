// Package rules is the built-in rule detector pack: Chinese mobile numbers,
// resident ID cards (checksum), bank cards (Luhn), unified social credit
// codes (checksum), licence plates, e-mail, private IPs, internal domains, and
// key/token shapes. Checksummed numbers have context exceptions: digits after
// "订单号", "工单号", "order_id" and similar are left alone.
package rules

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Finding is a detected span; Start/End are byte offsets into the text.
type Finding struct {
	Kind  string // upper-case category, e.g. PHONE
	Start int
	End   int
}

// Categories in this pack (lower-case is the config key).
const (
	Phone    = "PHONE"
	IDCard   = "ID_CARD"
	BankCard = "BANK_CARD"
	USCC     = "USCC"
	Plate    = "PLATE"
	Email    = "EMAIL"
	IP       = "IP"
	Domain   = "DOMAIN"
	Secret   = "SECRET"
)

type pattern struct {
	kind  string
	re    *regexp.Regexp
	group int                              // submatch to report (0 = whole match)
	check func(v string) bool              // validator on the reported span
	edge  func(text string, s, e int) bool // boundary check on the reported span
	ctx   bool                             // apply "order number" context exception
}

var (
	digitEdge = func(t string, s, e int) bool { return !isDigitAt(t, s-1, true) && !isDigitAt(t, e, false) }
	alnumEdge = func(t string, s, e int) bool { return !isAlnumAt(t, s-1, true) && !isAlnumAt(t, e, false) }
)

var patterns = []pattern{
	// Secrets first: they win overlaps.
	{kind: Secret, re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)},
	{kind: Secret, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{kind: Secret, re: regexp.MustCompile(`\b(?:sk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|LTAI[0-9A-Za-z]{12,24}|AKID[0-9A-Za-z]{13,40}|gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,}|xox[abprs]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{35}|glpat-[A-Za-z0-9_-]{20,})`)},
	{kind: Secret, re: regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]{20,}=*)`), group: 1},
	{kind: Secret, re: regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:([^/\s@]{3,})@`), group: 1},
	{kind: Secret, re: regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|auth[_-]?token|access[_-]?token|client[_-]?secret|密码|口令|密钥)["']?\s*[:=：]\s*["']?([^\s"',;，；]{6,})`), group: 1},

	{kind: IDCard, re: regexp.MustCompile(`[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]`), check: idCardOK, edge: alnumEdge, ctx: true},
	{kind: USCC, re: regexp.MustCompile(`[0-9A-HJ-NP-RTUW-Y]{2}\d{6}[0-9A-HJ-NP-RTUW-Y]{10}`), check: usccOK, edge: alnumEdge},
	{kind: Phone, re: regexp.MustCompile(`(?:\+?86[- ]?)?1[3-9]\d(?:[- ]?\d{4}){2}`), edge: digitEdge, ctx: true},
	{kind: BankCard, re: regexp.MustCompile(`\d{4}(?:[ -]?\d{4}){2,3}(?:[ -]?\d{1,3})?|\d{13,19}`), check: bankOK, edge: digitEdge, ctx: true},
	{kind: Email, re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)},
	{kind: Plate, re: regexp.MustCompile(`[京津沪渝冀豫云辽黑湘皖鲁新苏浙赣鄂桂甘晋蒙陕吉闽贵粤青藏川宁琼][A-HJ-NP-Z][·•]?[A-HJ-NP-Z0-9]{4,5}[A-HJ-NP-Z0-9挂学警]`), edge: alnumEdge},
	{kind: IP, re: regexp.MustCompile(`\b(?:10\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])|192\.168)\.\d{1,3}\.\d{1,3}\b`), check: ipOK},
}

var orderCtx = regexp.MustCompile(`(?i)(?:订单号|订单编号|订单|工单号|工单编号|工单|单号|流水号|快递单|运单号|交易号|order[ _-]?(?:id|no|number)?|ticket[ _-]?(?:id|no)?|tracking[ _-]?(?:id|no)?|invoice[ _-]?no)["']?\s*[:：#=]?\s*["']?$`)

// Detector runs the pack.
type Detector struct {
	domains *regexp.Regexp
}

// DefaultInternalDomains are the suffixes treated as internal host names.
var DefaultInternalDomains = []string{"internal", "corp", "lan", "local", "intra", "intranet"}

// New builds a detector; internalDomains are suffixes such as "corp" or "example.internal".
func New(internalDomains []string) *Detector {
	if len(internalDomains) == 0 {
		internalDomains = DefaultInternalDomains
	}
	alts := make([]string, 0, len(internalDomains))
	for _, d := range internalDomains {
		alts = append(alts, regexp.QuoteMeta(strings.TrimPrefix(d, ".")))
	}
	re := regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:` + strings.Join(alts, "|") + `)\b`)
	return &Detector{domains: re}
}

// Detect returns non-overlapping findings ordered by position.
func (d *Detector) Detect(text string) []Finding {
	var out []Finding
	taken := func(s, e int) bool {
		for _, f := range out {
			if s < f.End && e > f.Start {
				return true
			}
		}
		return false
	}
	run := func(p pattern) {
		for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[2*p.group], m[2*p.group+1]
			if s < 0 {
				continue
			}
			v := text[s:e]
			if p.edge != nil && !p.edge(text, s, e) {
				continue
			}
			if p.check != nil && !p.check(v) {
				continue
			}
			if p.ctx && orderCtx.MatchString(text[max(0, s-40):s]) {
				continue
			}
			if taken(s, e) {
				continue
			}
			out = append(out, Finding{Kind: p.kind, Start: s, End: e})
		}
	}
	for _, p := range patterns {
		run(p)
		if p.kind == Email {
			// internal domains after e-mail so that addresses stay whole
			run(pattern{kind: Domain, re: d.domains})
		}
	}
	sortFindings(out)
	return out
}

func sortFindings(f []Finding) {
	for i := 1; i < len(f); i++ {
		for j := i; j > 0 && f[j].Start < f[j-1].Start; j-- {
			f[j], f[j-1] = f[j-1], f[j]
		}
	}
}

func isDigitAt(t string, i int, before bool) bool {
	if i < 0 || i >= len(t) {
		return false
	}
	if before {
		r, _ := utf8.DecodeLastRuneInString(t[:i+1])
		return r >= '0' && r <= '9'
	}
	r, _ := utf8.DecodeRuneInString(t[i:])
	return r >= '0' && r <= '9'
}

func isAlnumAt(t string, i int, before bool) bool {
	if i < 0 || i >= len(t) {
		return false
	}
	var r rune
	if before {
		r, _ = utf8.DecodeLastRuneInString(t[:i+1])
	} else {
		r, _ = utf8.DecodeRuneInString(t[i:])
	}
	return r < utf8.RuneSelf && (r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
}

// Normalize canonicalises a value before hashing so that "138 0013 8000"
// and "13800138000" share a placeholder.
func Normalize(kind, v string) string {
	switch kind {
	case Phone, BankCard:
		v = strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, v)
		if kind == Phone && len(v) == 13 && strings.HasPrefix(v, "86") {
			v = v[2:]
		}
		return v
	case IDCard, USCC:
		return strings.ToUpper(v)
	case Email, Domain:
		return strings.ToLower(v)
	}
	return v
}

func idCardOK(v string) bool {
	w := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i := 0; i < 17; i++ {
		sum += int(v[i]-'0') * w[i]
	}
	want := "10X98765432"[sum%11]
	got := v[17]
	if got == 'x' {
		got = 'X'
	}
	return got == want
}

func bankOK(v string) bool {
	n := Normalize(BankCard, v)
	if len(n) < 13 || len(n) > 19 {
		return false
	}
	if !strings.ContainsAny(n[:1], "3456") && !strings.HasPrefix(n, "62") {
		return false
	}
	sum, alt := 0, false
	for i := len(n) - 1; i >= 0; i-- {
		d := int(n[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

const usccChars = "0123456789ABCDEFGHJKLMNPQRTUWXY"

func usccOK(v string) bool {
	if strings.IndexFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' }) < 0 {
		return false // all digits: leave to the other rules
	}
	w := []int{1, 3, 9, 27, 19, 26, 16, 17, 20, 29, 25, 13, 8, 24, 10, 30, 28}
	sum := 0
	for i := 0; i < 17; i++ {
		x := strings.IndexByte(usccChars, v[i])
		if x < 0 {
			return false
		}
		sum += x * w[i]
	}
	c := (31 - sum%31) % 31
	return usccChars[c] == v[17]
}

func ipOK(v string) bool {
	for _, part := range strings.Split(v, ".") {
		if len(part) > 3 || (len(part) == 3 && part > "255") {
			return false
		}
	}
	return true
}
