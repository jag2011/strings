// Package arb reads and writes Application Resource Bundle files.
//
// ARB is JSON where key order matters and "@"-prefixed keys carry meaning:
// "@@locale" and other "@@" globals, and "@key" metadata objects describing
// the message "key". Go maps lose order, so parsing walks the token stream.
package arb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Message struct {
	Key  string
	Text string
	Meta json.RawMessage // the "@key" object, nil if absent
}

type File struct {
	Locale          string // "@@locale", empty if absent
	Messages        []Message
	TrailingNewline bool
}

// Parse reads an ARB file, keeping message order.
func Parse(data []byte) (*File, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("arb: expected a JSON object")
	}
	f := &File{TrailingNewline: bytes.HasSuffix(data, []byte("\n"))}
	index := map[string]int{}
	meta := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("arb: %w", err)
		}
		key := t.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("arb: value of %q: %w", key, err)
		}
		switch {
		case key == "@@locale":
			if err := json.Unmarshal(raw, &f.Locale); err != nil {
				return nil, fmt.Errorf("arb: @@locale must be a string")
			}
		case strings.HasPrefix(key, "@@"):
			// Other globals (@@last_modified, @@x-…) are not kept.
		case strings.HasPrefix(key, "@"):
			meta[key[1:]] = raw
		default:
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, fmt.Errorf("arb: value of %q must be a string", key)
			}
			if _, dup := index[key]; dup {
				return nil, fmt.Errorf("arb: duplicate key %q", key)
			}
			index[key] = len(f.Messages)
			f.Messages = append(f.Messages, Message{Key: key, Text: text})
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("arb: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("arb: trailing data after the object")
	}
	for k, m := range meta {
		if i, ok := index[k]; ok {
			f.Messages[i].Meta = m
		}
	}
	return f, nil
}

// Write renders an ARB file with 2-space indent, in the given message order.
// Metadata is written when present. Strings are escaped like JSON requires
// and nothing more: no HTML escaping, non-ASCII stays as UTF-8.
func Write(f *File) []byte {
	var b bytes.Buffer
	b.WriteString("{")
	first := true
	entry := func(key string, value []byte) {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString("\n  ")
		writeString(&b, key)
		b.WriteString(": ")
		b.Write(value)
	}
	if f.Locale != "" {
		entry("@@locale", quote(f.Locale))
	}
	for _, m := range f.Messages {
		entry(m.Key, quote(m.Text))
		if len(m.Meta) > 0 {
			entry("@"+m.Key, indentMeta(m.Meta))
		}
	}
	if !first {
		b.WriteString("\n")
	}
	b.WriteString("}")
	if f.TrailingNewline {
		b.WriteString("\n")
	}
	return b.Bytes()
}

func quote(s string) []byte {
	var b bytes.Buffer
	writeString(&b, s)
	return b.Bytes()
}

func indentMeta(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "  ", "  "); err != nil {
		return raw
	}
	return b.Bytes()
}

const hex = "0123456789abcdef"

func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			default:
				if c < 0x20 {
					b.WriteString(`\u00`)
					b.WriteByte(hex[c>>4])
					b.WriteByte(hex[c&0xf])
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(`�`)
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// Locale codes as Flutter uses them in ARB file names:
// language, optional script, optional region, joined by "_" (en, pt_BR, zh_Hant_TW).
var localeRe = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z][a-z]{3})?(_(?:[A-Z]{2}|[0-9]{3}))?$`)

func ValidLocale(s string) bool { return localeRe.MatchString(s) }

// SplitFilename splits "intl_pt_BR.arb" into prefix "intl_" and locale "pt_BR".
// The locale is the longest valid locale at the end of the name.
func SplitFilename(name string) (prefix, locale string, ok bool) {
	base, found := strings.CutSuffix(name, ".arb")
	if !found {
		return "", "", false
	}
	if ValidLocale(base) {
		return "", base, true
	}
	for i := 0; i < len(base); i++ {
		if base[i] == '_' && ValidLocale(base[i+1:]) {
			return base[:i+1], base[i+1:], true
		}
	}
	return "", "", false
}

// Placeholders lists the placeholder names declared in "@key" metadata,
// with their example values if any, for display.
func Placeholders(meta json.RawMessage) (desc string, ph []Placeholder) {
	if len(meta) == 0 {
		return "", nil
	}
	var m struct {
		Description  string `json:"description"`
		Placeholders map[string]struct {
			Type    string `json:"type"`
			Example any    `json:"example"`
		} `json:"placeholders"`
	}
	if json.Unmarshal(meta, &m) != nil {
		return "", nil
	}
	for name, p := range m.Placeholders {
		ex := ""
		if p.Example != nil {
			ex = fmt.Sprint(p.Example)
			if s, ok := p.Example.(string); ok {
				ex = strconv.Quote(s)
			}
		}
		ph = append(ph, Placeholder{Name: name, Type: p.Type, Example: ex})
	}
	sort.Slice(ph, func(i, j int) bool { return ph[i].Name < ph[j].Name })
	return m.Description, ph
}

type Placeholder struct {
	Name, Type, Example string
}
