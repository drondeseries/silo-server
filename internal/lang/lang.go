// Package lang canonicalizes ISO 639 language codes and ISO 3166-1 country
// codes at ingest write sites so equivalent values ("en"/"eng"/"ENG")
// collapse to a single stored form.
package lang

import (
	"strings"
	"unicode"

	"golang.org/x/text/language"
)

// Canonical returns the ISO 639-1 lowercase 2-letter form of value, or the
// 3-letter form for languages without a 2-letter equivalent (e.g. "fil").
// Unparseable inputs are returned trimmed and lowercased verbatim so we
// never silently drop data. "und" (undetermined) and "mul" (multiple) are
// returned verbatim instead of being fed to the x/text parser, which would
// otherwise map "und" to English.
func Canonical(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return ""
	}
	switch trimmed {
	case "und", "undetermined":
		return "und"
	case "mul", "multiple":
		return "mul"
	}
	tag, err := language.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	base, conf := tag.Base()
	if conf == language.No {
		return trimmed
	}
	return strings.ToLower(base.String())
}

// languageNameCode maps full (lowercased) English language names to their
// canonical ISO 639-1 codes. Release-group MULTI/DUAL audio lists name the
// languages in English far more often than they use ISO codes, so the names
// are the primary vocabulary for ParseLanguages.
var languageNameCode = map[string]string{
	"english": "en", "french": "fr", "german": "de", "spanish": "es",
	"italian": "it", "portuguese": "pt", "dutch": "nl", "japanese": "ja",
	"korean": "ko", "chinese": "zh", "russian": "ru", "hindi": "hi",
	"tamil": "ta", "telugu": "te", "polish": "pl", "swedish": "sv",
	"danish": "da", "finnish": "fi", "turkish": "tr", "arabic": "ar",
	"hebrew": "he", "czech": "cs", "slovak": "sk", "hungarian": "hu",
	"romanian": "ro", "bulgarian": "bg", "greek": "el", "ukrainian": "uk",
	"vietnamese": "vi", "thai": "th", "indonesian": "id", "malay": "ms",
	"norwegian": "no", "catalan": "ca", "croatian": "hr", "serbian": "sr",
	"slovenian": "sl", "lithuanian": "lt", "latvian": "lv", "estonian": "et",
	"icelandic": "is", "welsh": "cy", "irish": "ga", "basque": "eu",
	"galician": "gl", "afrikaans": "af", "bengali": "bn", "marathi": "mr",
	"gujarati": "gu", "kannada": "kn", "malayalam": "ml", "punjabi": "pa",
	"urdu": "ur", "persian": "fa", "nepali": "ne", "swahili": "sw",
	"kazakh": "kk", "uzbek": "uz", "mongolian": "mn", "armenian": "hy",
	"georgian": "ka", "azerbaijani": "az", "burmese": "my", "khmer": "km",
	"lao": "lo", "amharic": "am", "zulu": "zu", "latin": "la",
	"esperanto": "eo",
}

// audioJunkTokens are codec, channel, and role words that show up beside a
// language list in a track title ("AC3 5.1 English+French") and must be
// filtered before language matching so they never read as a language.
var audioJunkTokens = map[string]bool{
	"ac3": true, "eac3": true, "dts": true, "dts-hd": true, "truehd": true, "atmos": true,
	"stereo": true, "mono": true, "surround": true, "commentary": true,
	"dub": true, "original": true, "multi": true, "dual": true,
	"audio": true, "track": true, "lang": true, "language": true,
	"mix": true, "mixed": true, "hd": true, "ma": true,
	"2.0": true, "5.1": true, "7.1": true, "5.1.2": true, "7.1.4": true,
}

// ParseLanguages extracts the languages advertised in an audio track title
// like "English / French" or "AC3 5.1 English+French+Spanish". Tokens are
// split on `[\/+&,]` and whitespace; codec/channel/role words are filtered
// first, then each remaining token is resolved as an English language name, a
// 3-letter ISO code, or an UPPERCASE 2-letter code (lowercase 2-letter tokens
// are rejected to avoid English-word false positives like "it" or "no").
// Results are canonicalized and deduplicated preserving first-seen order.
// Returns nil when no language could be identified.
func ParseLanguages(title string) []string {
	tokens := strings.FieldsFunc(strings.TrimSpace(title), func(r rune) bool {
		return r == '/' || r == '\\' || r == '+' || r == '&' || r == ',' || unicode.IsSpace(r)
	})
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		lower := strings.ToLower(token)
		if audioJunkTokens[lower] {
			continue
		}
		var code string
		switch {
		case languageNameCode[lower] != "":
			code = languageNameCode[lower]
		case len(token) == 3:
			code = threeLetterLanguageCode(lower)
		case len(token) == 2 && token == strings.ToUpper(token):
			code = Canonical(lower)
		}
		if code == "" || code == "und" || code == "mul" {
			continue
		}
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// threeLetterLanguageCode canonicalizes a 3-letter ISO 639 token when it
// parses as a real language. "und"/"mul" and unparseable tokens yield "" so
// they never enter a languages list.
func threeLetterLanguageCode(code string) string {
	if code == "und" || code == "mul" {
		return ""
	}
	tag, err := language.Parse(code)
	if err != nil {
		return ""
	}
	base, conf := tag.Base()
	if conf == language.No {
		return ""
	}
	return strings.ToLower(base.String())
}

// CanonicalCountry returns the ISO 3166-1 alpha-2 uppercase form. Unparseable
// inputs are returned trimmed and uppercased verbatim.
func CanonicalCountry(value string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(value))
	if trimmed == "" {
		return ""
	}
	region, err := language.ParseRegion(trimmed)
	if err != nil {
		return trimmed
	}
	return region.String()
}

// CanonicalCountries returns a copy of values with each entry canonicalized
// and empties dropped. Preserves nil so callers can keep the SQL NULL
// distinction from an empty array.
func CanonicalCountries(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		c := CanonicalCountry(v)
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}
