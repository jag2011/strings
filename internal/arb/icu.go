package arb

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Arg is one argument used in an ICU message: "{name}", "{n, number}",
// "{count, plural, …}". Kind is the second part, empty for plain placeholders.
type Arg struct {
	Name string
	Kind string
}

// ParseICU checks that a message is valid ICU MessageFormat and returns the
// arguments it uses. Apostrophes are literal text, matching Flutter's default
// (gen-l10n use-escaping: false, intl_utils).
func ParseICU(s string) ([]Arg, error) {
	p := &icuParser{s: s}
	if err := p.message(0); err != nil {
		return nil, err
	}
	return p.args, nil
}

type icuParser struct {
	s    string
	i    int
	args []Arg
}

func (p *icuParser) message(depth int) error {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case '{':
			p.i++
			if err := p.argument(depth); err != nil {
				return err
			}
		case '}':
			if depth == 0 {
				return fmt.Errorf("unmatched }")
			}
			return nil
		default:
			p.i++
		}
	}
	if depth > 0 {
		return fmt.Errorf("missing }")
	}
	return nil
}

func (p *icuParser) argument(depth int) error {
	p.space()
	name := p.ident()
	if name == "" {
		return fmt.Errorf("empty or invalid placeholder name after {")
	}
	p.space()
	if p.eat('}') {
		p.args = append(p.args, Arg{Name: name})
		return nil
	}
	if !p.eat(',') {
		return fmt.Errorf("placeholder {%s: expected } or ,", name)
	}
	p.space()
	kind := p.ident()
	if kind == "" {
		return fmt.Errorf("placeholder {%s: missing type after ,", name)
	}
	p.args = append(p.args, Arg{Name: name, Kind: kind})
	p.space()
	if p.eat('}') {
		return nil
	}
	if !p.eat(',') {
		return fmt.Errorf("placeholder {%s, %s: expected } or ,", name, kind)
	}
	switch kind {
	case "plural", "select", "selectordinal":
		return p.cases(depth, name, kind)
	}
	// number/date/time style: skip to the matching brace.
	for open := 1; p.i < len(p.s); p.i++ {
		switch p.s[p.i] {
		case '{':
			open++
		case '}':
			if open--; open == 0 {
				p.i++
				return nil
			}
		}
	}
	return fmt.Errorf("placeholder {%s, %s: missing }", name, kind)
}

func (p *icuParser) cases(depth int, name, kind string) error {
	var selectors []string
	for {
		p.space()
		if p.i >= len(p.s) {
			return fmt.Errorf("{%s, %s: missing }", name, kind)
		}
		if p.eat('}') {
			break
		}
		start := p.i
		for p.i < len(p.s) && !strings.ContainsRune(" \t\n\r{}", rune(p.s[p.i])) {
			p.i++
		}
		sel := p.s[start:p.i]
		if sel == "" {
			return fmt.Errorf("{%s, %s: expected a case like other{…}", name, kind)
		}
		if strings.HasPrefix(sel, "offset:") {
			continue
		}
		p.space()
		if !p.eat('{') {
			return fmt.Errorf("{%s, %s: case %q needs a {…} message", name, kind, sel)
		}
		if err := p.message(depth + 1); err != nil {
			return err
		}
		if !p.eat('}') {
			return fmt.Errorf("{%s, %s: case %q is missing }", name, kind, sel)
		}
		selectors = append(selectors, sel)
	}
	if !slices.Contains(selectors, "other") {
		return fmt.Errorf("{%s, %s} needs an other{…} case", name, kind)
	}
	return nil
}

func (p *icuParser) eat(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *icuParser) space() {
	for p.i < len(p.s) && strings.IndexByte(" \t\n\r", p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *icuParser) ident() string {
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			p.i++
			continue
		}
		break
	}
	return p.s[start:p.i]
}

// Check validates a translation against its source text. Errors block saving,
// warnings are shown and can be overridden.
func Check(source, text string) (errs, warns []string) {
	if strings.TrimSpace(text) == "" {
		if strings.TrimSpace(source) == "" {
			return nil, nil
		}
		return []string{"The translation is empty."}, nil
	}
	got, err := ParseICU(text)
	if err != nil {
		errs = append(errs, "Invalid braces: "+err.Error()+".")
	}
	want, srcErr := ParseICU(source)
	if err == nil && srcErr == nil {
		e, w := compareArgs(want, got)
		errs, warns = append(errs, e...), append(warns, w...)
	}

	if a, b := strings.Count(source, "\n"), strings.Count(text, "\n"); a != b {
		warns = append(warns, fmt.Sprintf("The source has %d line breaks, the translation has %d.", a, b))
	}
	if a, b := utf8.RuneCountInString(source), utf8.RuneCountInString(text); b > 3*a+20 {
		warns = append(warns, fmt.Sprintf("The translation is much longer than the source (%d vs. %d characters).", b, a))
	}
	return errs, warns
}

// compareArgs: an unknown argument breaks code generation, so it is an
// error. An unused one compiles and is sometimes intended, e.g. when a
// translation spells out a name to inflect it, so it is a warning.
func compareArgs(want, got []Arg) (errs, warns []string) {
	kinds := func(args []Arg) map[string]string {
		m := map[string]string{}
		for _, a := range args {
			if m[a.Name] == "" || a.Kind != "" {
				m[a.Name] = a.Kind
			}
		}
		return m
	}
	w, g := kinds(want), kinds(got)
	for name, k := range w {
		if _, ok := g[name]; !ok {
			warns = append(warns, fmt.Sprintf("Placeholder {%s} is not used. That's fine only if intended.", name))
		} else if isChoice(k) && g[name] != k {
			errs = append(errs, fmt.Sprintf("{%s} must stay a %s.", name, k))
		}
	}
	for name := range g {
		if _, ok := w[name]; !ok {
			msg := fmt.Sprintf("Placeholder {%s} does not exist in the source.", name)
			for known := range w {
				if strings.EqualFold(known, name) {
					msg += fmt.Sprintf(" Did you mean {%s}?", known)
				}
			}
			errs = append(errs, msg)
		}
	}
	slices.Sort(errs)
	slices.Sort(warns)
	return errs, warns
}

func isChoice(kind string) bool {
	return kind == "plural" || kind == "select" || kind == "selectordinal"
}
