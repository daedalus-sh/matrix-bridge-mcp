package main

import (
	"strings"
	"unicode"
)

// clean makes message text safe to hand a model as data: every control and
// invisible format character goes (bidi overrides, zero-width spaces, tag
// characters — the ways an instruction hides in plain sight), except the
// zero-width joiner emoji sequences are built from; runs of blank lines and
// spaces shrink; and the result is cut at max runes. It does not make the
// text trustworthy: what it says is still somebody else's words.
func clean(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	newlines, spaces, n := 0, 0, 0
	for _, r := range s {
		switch {
		case r == '\n':
			spaces = 0
			newlines++
			if newlines > 2 {
				continue
			}
		case r == '\t' || r == ' ' || unicode.IsSpace(r):
			newlines = 0
			spaces++
			if spaces > 4 {
				continue
			}
			r = ' '
		case r == '‍':
			newlines, spaces = 0, 0
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r):
			continue
		default:
			newlines, spaces = 0, 0
		}
		if n == max {
			b.WriteString(" … [cut]")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// personName drops the " (WA)" the bridge's displayname template appends to
// every WhatsApp contact.
func personName(display string) string {
	return strings.TrimSuffix(strings.TrimSpace(display), " (WA)")
}
