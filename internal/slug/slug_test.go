package slug_test

import (
	"fmt"
	"testing"

	"github.com/koicro/ace-go/internal/slug"
)

func TestMake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"simple", "Hello World", "hello-world"},
		{"collapses separators", "  a -- b__c  ", "a-b-c"},
		{"digits", "Go 1.27 release", "go-1-27-release"},
		{"accents folded", "Crème Brûlée", "creme-brulee"},
		{"non-latin is separator", "日本 go", "go"},
		{"only symbols", "!!!", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := slug.Make(tt.in); got != tt.want {
				t.Errorf("Make(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// FuzzMake checks invariants that must hold for every input.
// `make eval` runs it briefly; `make fuzz` runs it longer.
// Failing inputs are saved under testdata/fuzz and become regression tests.
func FuzzMake(f *testing.F) {
	for _, s := range []string{"", "Hello World", "Crème Brûlée", "a--b", "-x-", "\x00\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := slug.Make(in)
		for i, r := range got {
			valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !valid {
				t.Fatalf("Make(%q) = %q: invalid rune %q at %d", in, got, r, i)
			}
		}
		if got != "" && (got[0] == '-' || got[len(got)-1] == '-') {
			t.Fatalf("Make(%q) = %q: leading or trailing hyphen", in, got)
		}
		if again := slug.Make(got); again != got {
			t.Fatalf("not idempotent: Make(%q) = %q, Make(%q) = %q", in, got, got, again)
		}
	})
}

func BenchmarkMake(b *testing.B) {
	const in = "The Quick Brown Fox — Jumps Over the Lazy Dog, Crème Brûlée edition"
	for b.Loop() {
		slug.Make(in)
	}
}

func ExampleMake() {
	fmt.Println(slug.Make("Crème Brûlée: A Recipe!"))
	// Output: creme-brulee-a-recipe
}
