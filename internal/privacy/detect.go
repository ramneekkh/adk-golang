// Package privacy implements the FR-E1 / FR-E2 sensitive-data placeholder swap.
//
// Detection is pattern based (email, URL, Thai citizen ID with checksum,
// Thai mobile number, bank account number). Every distinct value found gets a
// stable, numbered placeholder such as [EMAIL_1] or [ACCOUNT_1], recorded in a
// per-session Vault that lives only on the host (ADK session state). The model
// only ever sees placeholders; Restore swaps them back and normalizes the
// variants a model tends to hallucinate ([email_1], EMAIL_1, [EMAIL], ...).
package privacy

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind is a category of sensitive value.
type Kind string

const (
	KindEmail     Kind = "EMAIL"
	KindURL       Kind = "URL"
	KindCitizenID Kind = "CITIZEN_ID"
	KindPhone     Kind = "PHONE"
	KindAccount   Kind = "ACCOUNT"
)

var allKinds = []Kind{KindEmail, KindURL, KindCitizenID, KindPhone, KindAccount}

type detector struct {
	kind  Kind
	re    *regexp.Regexp
	valid func(string) bool
}

// Order matters: emails before URLs (so user@www.x.com is one email), citizen
// IDs (13 digits) before phones/accounts, phones (10 digits, 06/08/09 prefix)
// before generic 9-12 digit account numbers.
var detectors = []detector{
	{kind: KindEmail, re: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	{kind: KindURL, re: regexp.MustCompile(`(?i)\bhttps?://[^\s<>"'\)\]]+|\bwww\.[^\s<>"'\)\]]+`)},
	{kind: KindCitizenID, re: regexp.MustCompile(`\b\d-?\d{4}-?\d{5}-?\d{2}-?\d\b`), valid: validThaiCitizenID},
	{kind: KindPhone, re: regexp.MustCompile(`\b0[689]\d-?\d{3}-?\d{4}\b`)},
	{kind: KindAccount, re: regexp.MustCompile(`\b\d{3}-\d-\d{5}-\d\b|\b\d{9,12}\b`)},
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validThaiCitizenID applies the official mod-11 checksum.
func validThaiCitizenID(s string) bool {
	d := digitsOnly(s)
	if len(d) != 13 {
		return false
	}
	sum := 0
	for i := 0; i < 12; i++ {
		sum += int(d[i]-'0') * (13 - i)
	}
	check := (11 - sum%11) % 10
	return check == int(d[12]-'0')
}

// Finding records one detected value (never logged with its raw value).
type Finding struct {
	Kind  Kind   `json:"kind"`
	Token string `json:"token"`
}

// Normalization records a restore-time canonicalization guard firing.
type Normalization struct {
	Rule string `json:"rule"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Vault maps placeholder tokens to real values for one session.
type Vault struct {
	Tokens map[string]string `json:"tokens"`
}

func NewVault() *Vault { return &Vault{Tokens: map[string]string{}} }

// VaultFromState rebuilds a vault from a session-state value (which may have
// round-tripped through JSON as map[string]any).
func VaultFromState(v any) *Vault {
	out := NewVault()
	switch m := v.(type) {
	case map[string]string:
		for k, val := range m {
			out.Tokens[k] = val
		}
	case map[string]any:
		for k, val := range m {
			if s, ok := val.(string); ok {
				out.Tokens[k] = s
			}
		}
	}
	return out
}

// Snapshot returns a copy suitable for storing in session state.
func (v *Vault) Snapshot() map[string]string {
	cp := make(map[string]string, len(v.Tokens))
	for k, val := range v.Tokens {
		cp[k] = val
	}
	return cp
}

func tokenKind(tok string) (Kind, int) {
	t := strings.TrimSuffix(strings.TrimPrefix(tok, "["), "]")
	i := strings.LastIndex(t, "_")
	if i < 0 {
		return "", 0
	}
	n, err := strconv.Atoi(t[i+1:])
	if err != nil {
		return "", 0
	}
	return Kind(t[:i]), n
}

func canonicalValue(kind Kind, raw string) string {
	switch kind {
	case KindEmail:
		return strings.ToLower(raw)
	case KindCitizenID, KindPhone, KindAccount:
		return digitsOnly(raw)
	}
	return raw
}

func (v *Vault) tokenFor(kind Kind, raw string) string {
	canon := canonicalValue(kind, raw)
	maxN := 0
	for tok, val := range v.Tokens {
		k, n := tokenKind(tok)
		if k != kind {
			continue
		}
		if canonicalValue(kind, val) == canon {
			return tok
		}
		if n > maxN {
			maxN = n
		}
	}
	tok := fmt.Sprintf("[%s_%d]", kind, maxN+1)
	v.Tokens[tok] = raw
	return tok
}

// Mask replaces every detected sensitive value in text with its placeholder.
func Mask(text string, v *Vault) (string, []Finding) {
	var findings []Finding
	out := text
	for _, d := range detectors {
		out = d.re.ReplaceAllStringFunc(out, func(m string) string {
			if d.valid != nil && !d.valid(m) {
				return m
			}
			tok := v.tokenFor(d.kind, m)
			findings = append(findings, Finding{Kind: d.kind, Token: tok})
			return tok
		})
	}
	return out, findings
}

// variantRe matches placeholder variants a model may emit:
// [EMAIL_1] [email_1] [ EMAIL-1 ] 【EMAIL_1】 EMAIL_1 [EMAIL]
var variantRe = regexp.MustCompile(`(?i)[\[【]\s*(EMAIL|URL|CITIZEN[_ ]?ID|PHONE|ACCOUNT)(?:\s*[_\- ]\s*(\d+))?\s*[\]】]|\b(EMAIL|URL|CITIZEN_ID|PHONE|ACCOUNT)_(\d+)\b`)

// Restore swaps placeholders (and hallucinated variants of them) back to the
// real values, then applies the URL canonicalization guard.
func Restore(text string, v *Vault) (string, []Normalization) {
	var norms []Normalization
	out := variantRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := variantRe.FindStringSubmatch(m)
		kindStr, num := sub[1], sub[2]
		if kindStr == "" {
			kindStr, num = sub[3], sub[4]
		}
		norm := strings.NewReplacer(" ", "", "_", "").Replace(strings.ToUpper(kindStr))
		kind := Kind(norm)
		if norm == "CITIZENID" {
			kind = KindCitizenID
		}
		var tok string
		if num != "" {
			tok = fmt.Sprintf("[%s_%s]", kind, num)
		} else {
			// Unnumbered [EMAIL]: only resolvable if the session has exactly one.
			var cands []string
			for t := range v.Tokens {
				if k, _ := tokenKind(t); k == kind {
					cands = append(cands, t)
				}
			}
			if len(cands) != 1 {
				return m
			}
			tok = cands[0]
		}
		val, ok := v.Tokens[tok]
		if !ok {
			return m
		}
		if m != tok {
			norms = append(norms, Normalization{Rule: "placeholder_variant", From: m, To: tok})
		}
		return val
	})
	out, urlNorms := canonicalizeURLs(out, v)
	return out, append(norms, urlNorms...)
}

// OfficialHosts is the allowlist used by the URL canonicalization guard.
// Configurable with VA_OFFICIAL_HOSTS (comma separated).
func OfficialHosts() []string {
	if s := os.Getenv("VA_OFFICIAL_HOSTS"); s != "" {
		var hs []string
		for _, h := range strings.Split(s, ",") {
			if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
				hs = append(hs, h)
			}
		}
		return hs
	}
	return []string{"krungthai.com"}
}

var urlInText = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"'\)\]]+|\bwww\.[^\s<>"'\)\]]+`)

func hostOf(raw string) string {
	s := raw
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func label(host string) string {
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return host
}

// canonicalizeURLs rewrites URLs whose host is a near-miss of an official or
// user-supplied host (typo within edit distance 2, or same label on a wrong
// TLD such as krungthai.co.th) onto the canonical host.
func canonicalizeURLs(text string, v *Vault) (string, []Normalization) {
	known := map[string]bool{}
	for _, h := range OfficialHosts() {
		known[h] = true
	}
	for tok, val := range v.Tokens {
		if k, _ := tokenKind(tok); k == KindURL {
			if h := hostOf(val); h != "" {
				known[h] = true
			}
		}
	}
	hosts := make([]string, 0, len(known))
	for h := range known {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	var norms []Normalization
	out := urlInText.ReplaceAllStringFunc(text, func(m string) string {
		h := hostOf(m)
		if h == "" || known[h] {
			return m
		}
		for _, canon := range hosts {
			if levenshtein(h, canon) <= 2 || label(h) == label(canon) || levenshtein(label(h), label(canon)) <= 1 && label(h) != "" {
				idx := strings.Index(strings.ToLower(m), h)
				if idx < 0 {
					continue
				}
				fixed := m[:idx] + canon + m[idx+len(h):]
				norms = append(norms, Normalization{Rule: "url_canonicalization", From: m, To: fixed})
				return fixed
			}
		}
		return m
	})
	return out, norms
}

// ContainsRaw reports whether any vault value appears verbatim in s.
func (v *Vault) ContainsRaw(s string) bool {
	for _, val := range v.Tokens {
		if val != "" && strings.Contains(s, val) {
			return true
		}
	}
	return false
}

// Kinds lists supported kinds (for docs / UI).
func Kinds() []Kind { return append([]Kind(nil), allKinds...) }
