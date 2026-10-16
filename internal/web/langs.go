package web

import (
	"io/fs"
	"strings"
)

// Display names for common languages. Unknown codes show as the code itself.
var langNames = map[string]string{
	"af": "Afrikaans", "am": "Amharic", "ar": "Arabic", "az": "Azerbaijani", "be": "Belarusian",
	"bg": "Bulgarian", "bn": "Bengali", "bs": "Bosnian", "ca": "Catalan", "cs": "Czech",
	"cy": "Welsh", "da": "Danish", "de": "German", "el": "Greek", "en": "English",
	"eo": "Esperanto", "es": "Spanish", "et": "Estonian", "eu": "Basque", "fa": "Persian",
	"fi": "Finnish", "fil": "Filipino", "fr": "French", "ga": "Irish", "gl": "Galician",
	"gu": "Gujarati", "he": "Hebrew", "hi": "Hindi", "hr": "Croatian", "hu": "Hungarian",
	"hy": "Armenian", "id": "Indonesian", "is": "Icelandic", "it": "Italian", "ja": "Japanese",
	"ka": "Georgian", "kk": "Kazakh", "km": "Khmer", "kn": "Kannada", "ko": "Korean",
	"lo": "Lao", "lt": "Lithuanian", "lv": "Latvian", "mk": "Macedonian", "ml": "Malayalam",
	"mn": "Mongolian", "mr": "Marathi", "ms": "Malay", "my": "Burmese", "nb": "Norwegian Bokmål",
	"ne": "Nepali", "nl": "Dutch", "nn": "Norwegian Nynorsk", "no": "Norwegian", "pa": "Punjabi",
	"pl": "Polish", "pt": "Portuguese", "ro": "Romanian", "ru": "Russian", "si": "Sinhala",
	"sk": "Slovak", "sl": "Slovenian", "sq": "Albanian", "sr": "Serbian", "sv": "Swedish",
	"sw": "Swahili", "ta": "Tamil", "te": "Telugu", "th": "Thai", "tl": "Tagalog",
	"tr": "Turkish", "uk": "Ukrainian", "ur": "Urdu", "uz": "Uzbek", "vi": "Vietnamese",
	"zh": "Chinese", "zu": "Zulu",
}

// Flags for languages, by the country most associated with them. Languages
// without one (like Esperanto) get a blank placeholder. A region in the code
// (pt_BR) takes precedence when its flag is bundled.
var langFlags = map[string]string{
	"af": "za", "am": "et", "ar": "sa", "az": "az", "be": "by", "bg": "bg", "bn": "bd", "bs": "ba",
	"ca": "es-ct", "cs": "cz", "cy": "gb-wls", "da": "dk", "de": "de", "el": "gr", "en": "gb",
	"es": "es", "et": "ee", "eu": "es-pv", "fa": "ir", "fi": "fi", "fil": "ph", "fr": "fr", "ga": "ie",
	"gl": "es-ga", "gu": "in", "he": "il", "hi": "in", "hr": "hr", "hu": "hu", "hy": "am", "id": "id",
	"is": "is", "it": "it", "ja": "jp", "ka": "ge", "kk": "kz", "km": "kh", "kn": "in", "ko": "kr",
	"lo": "la", "lt": "lt", "lv": "lv", "mk": "mk", "ml": "in", "mn": "mn", "mr": "in", "ms": "my",
	"my": "mm", "nb": "no", "ne": "np", "nl": "nl", "nn": "no", "no": "no", "pa": "in", "pl": "pl",
	"pt": "pt", "ro": "ro", "ru": "ru", "si": "lk", "sk": "sk", "sl": "si", "sq": "al", "sr": "rs",
	"sv": "se", "sw": "tz", "ta": "in", "te": "in", "th": "th", "tl": "ph", "tr": "tr", "uk": "ua",
	"ur": "pk", "uz": "uz", "vi": "vn", "zh": "cn", "zu": "za",
}

// bundledFlags is the set of flag files in static/flags, by country code.
var bundledFlags = func() map[string]bool {
	m := map[string]bool{}
	entries, _ := fs.ReadDir(staticFS, "static/flags")
	for _, e := range entries {
		if c, ok := strings.CutSuffix(e.Name(), ".svg"); ok {
			m[c] = true
		}
	}
	return m
}()

// flag returns the static path of the flag for a language code, or "".
func flag(code string) string {
	parts := strings.Split(code, "_")
	for _, p := range parts[1:] {
		if c := strings.ToLower(p); len(p) == 2 && bundledFlags[c] {
			return "flags/" + c + ".svg"
		}
	}
	if c, ok := langFlags[parts[0]]; ok && bundledFlags[c] {
		return "flags/" + c + ".svg"
	}
	return ""
}

var scriptNames = map[string]string{"Hans": "Simplified", "Hant": "Traditional", "Latn": "Latin", "Cyrl": "Cyrillic"}

// langName turns "pt_BR" into "Portuguese (BR)" and "zh_Hant" into "Chinese (Traditional)".
// LangName returns the display name of a language code, like "German" for
// de or "Portuguese (BR)" for pt_BR.
func LangName(code string) string { return langName(code) }

func langName(code string) string {
	parts := strings.Split(code, "_")
	name, ok := langNames[parts[0]]
	if !ok {
		return code
	}
	var extra []string
	for _, p := range parts[1:] {
		if s, ok := scriptNames[p]; ok {
			p = s
		}
		extra = append(extra, p)
	}
	if len(extra) > 0 {
		name += " (" + strings.Join(extra, ", ") + ")"
	}
	return name
}
