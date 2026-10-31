// Package slug converts arbitrary text into URL-safe identifiers.
//
// It is a small reference package showing the testing conventions used in
// this repository: table-driven tests, fuzz tests that check invariants,
// benchmarks, and runnable examples. Delete it once real packages exist.
package slug

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Make returns a lowercase slug of s: ASCII letters and digits separated by
// single hyphens, with no leading or trailing hyphen. Accented Latin letters
// are folded to their base letter; other characters act as separators.
// The result is empty if s contains no letters or digits.
func Make(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSep := false
	for _, r := range norm.NFKD.String(s) {
		switch {
		case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if pendingSep && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingSep = false
			b.WriteRune(unicode.ToLower(r))
		case unicode.Is(unicode.Mn, r):
			// Combining mark left over from decomposition (é -> e + ◌́): drop it.
		default:
			pendingSep = true
		}
	}
	return b.String()
}
