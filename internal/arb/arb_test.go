package arb

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const fixture = `{
  "@@locale": "en",
  "hello": "Hello {name}!",
  "@hello": {
    "description": "Greeting on the start screen",
    "placeholders": {
      "name": {
        "type": "String",
        "example": "Ada"
      }
    }
  },
  "items": "{count, plural, =0{No items} =1{One item} other{{count} items}}",
  "quote": "She said \"<b>&\"\tthen\nleft \\ ü 🎉",
  "pronoun": "{gender, select, male{he} female{she} other{they}}"
}
`

func TestRoundTrip(t *testing.T) {
	f, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if f.Locale != "en" || len(f.Messages) != 4 || f.Messages[0].Meta == nil {
		t.Fatalf("unexpected parse: %+v", f)
	}
	if got := string(Write(f)); got != fixture {
		t.Fatalf("round trip differs:\n%s", got)
	}
	desc, ph := Placeholders(f.Messages[0].Meta)
	if desc != "Greeting on the start screen" || len(ph) != 1 || ph[0].Example != `"Ada"` {
		t.Fatalf("placeholders: %q %+v", desc, ph)
	}
}

func TestEmpty(t *testing.T) {
	f, err := Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(Write(f)); got != "{}" {
		t.Fatalf("got %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{`[]`, `{"a": 1}`, `{"a": "x", "a": "y"}`, `{"a": "x"} {}`, `{"a": "x"`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%s): expected an error", in)
		}
	}
}

// Files in testdata cover the styles found in the wild: no metadata, with or
// without a trailing newline and @@locale, escapes, plurals, non-Latin scripts.
// They must round-trip byte for byte and pass the ICU parser.
func TestTestdataFiles(t *testing.T) {
	checkFiles(t, "testdata", true)
}

// ARB_TESTDATA=path/to/l10n checks your own files too. Hand-edited source
// files may be formatted differently, so only their content has to survive.
func TestExternalFiles(t *testing.T) {
	dir := os.Getenv("ARB_TESTDATA")
	if dir == "" {
		t.Skip("set ARB_TESTDATA to a directory of .arb files")
	}
	checkFiles(t, dir, false)
}

func checkFiles(t *testing.T, dir string, exact bool) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.arb"))
	if len(files) == 0 {
		t.Fatal("no .arb files in", dir)
	}
	for _, path := range files {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := Parse(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := Write(f)
		if exact && !bytes.Equal(out, data) {
			t.Errorf("%s: round trip differs:\n%s", name, out)
		}
		g, err := Parse(out)
		if err != nil || g.Locale != f.Locale || !slices.EqualFunc(f.Messages, g.Messages, func(a, b Message) bool {
			return a.Key == b.Key && a.Text == b.Text && (len(a.Meta) == 0) == (len(b.Meta) == 0)
		}) {
			t.Errorf("%s: content changed in round trip", name)
		}
		for _, m := range f.Messages {
			if _, err := ParseICU(m.Text); err != nil {
				t.Errorf("%s %s: %v", name, m.Key, err)
			}
		}
	}
}

func TestSplitFilename(t *testing.T) {
	cases := map[string][2]string{
		"intl_en.arb":        {"intl_", "en"},
		"intl_pt_BR.arb":     {"intl_", "pt_BR"},
		"app_zh_Hant_TW.arb": {"app_", "zh_Hant_TW"},
		"my_app_de.arb":      {"my_app_", "de"},
		"de.arb":             {"", "de"},
		"strings_es_419.arb": {"strings_", "es_419"},
	}
	for name, want := range cases {
		prefix, locale, ok := SplitFilename(name)
		if !ok || prefix != want[0] || locale != want[1] {
			t.Errorf("SplitFilename(%q) = %q %q %v", name, prefix, locale, ok)
		}
	}
	for _, name := range []string{"intl_EN.arb", "intl_en.json", "readme.arb"} {
		if _, _, ok := SplitFilename(name); ok {
			t.Errorf("SplitFilename(%q): expected no match", name)
		}
	}
}

func TestICU(t *testing.T) {
	ok := []string{
		"plain text",
		"It's {num} o'clock",
		"{a}{b} and {a}",
		"{n, number}",
		"{n, number, ::currency/EUR}",
		"{count, plural, offset:1 =0{none} one{# item} other{{count} items of {total}}}",
		"{g, select, a{{count, plural, one{x} other{y}}} other{z}}",
	}
	for _, s := range ok {
		if _, err := ParseICU(s); err != nil {
			t.Errorf("ParseICU(%q): %v", s, err)
		}
	}
	bad := []string{"{", "}", "{}", "{a", "a}", "{a b}", "{n, plural, one{x}}", "{n, plural, other{x}", "{n, select, other}"}
	for _, s := range bad {
		if _, err := ParseICU(s); err == nil {
			t.Errorf("ParseICU(%q): expected an error", s)
		}
	}
}

func TestHasText(t *testing.T) {
	for s, want := range map[string]bool{
		"Tank":                             true,
		"{count} Days":                     true,
		"Ελληνικά":                         true,
		"{count}":                          false,
		"{n} %":                            false,
		"{a} – {b}!":                       false,
		"{n, number, ::percent}":           false,
		"{count, plural, one{#} other{#}}": false,
		"{count, plural, one{# day} other{# days}}": true,
		"": false,
	} {
		if got := HasText(s); got != want {
			t.Errorf("HasText(%q) = %v", s, got)
		}
	}
}

func TestCheck(t *testing.T) {
	cases := []struct {
		source, text string
		errs, warns  int
	}{
		{"Hello {name}", "Hallo {name}", 0, 0},
		{"{num} of {total}", "{total} von {num}", 0, 0}, // moved: ok
		{"Hello {name}", "Hallo", 0, 1},                 // unused: warning
		{"Hello {name}", "Hallo {nme}", 1, 1},           // renamed: unknown + unused
		{"Hello {name}", "Hallo {name} {name}", 0, 0},   // repeated: ok
		{"Hello", "  ", 1, 0},                           // empty
		{"", "", 0, 0},                                  // empty source: empty is fine
		{"Hello", "Hallo }", 1, 0},                      // broken braces
		{"{n, plural, one{# day} other{# days}}", "{n, plural, one{# den} few{# dny} other{# dní}}", 0, 0},
		{"{n, plural, one{# day} other{# days}}", "{n} Tage", 1, 0}, // plural lost
		{"a\nb", "a b", 0, 1},                 // line breaks
		{"Hi", strings.Repeat("x", 40), 0, 1}, // length
	}
	for _, c := range cases {
		errs, warns := Check(c.source, c.text)
		if len(errs) != c.errs || len(warns) != c.warns {
			t.Errorf("Check(%q, %q) = %v %v", c.source, c.text, errs, warns)
		}
	}
	errs, _ := Check("Hello {name}", "Hallo {x}")
	if !slices.Equal(errs, []string{"Placeholder {x} does not exist in the source."}) {
		t.Errorf("message: %v", errs)
	}
	errs, _ = Check("Hello {userName}", "Hallo {username}")
	if !slices.Equal(errs, []string{"Placeholder {username} does not exist in the source. Did you mean {userName}?"}) {
		t.Errorf("message: %v", errs)
	}
}
