package main

import (
	"math"
	"strings"
	"testing"
)

func TestParseTestEvents(t *testing.T) {
	stream := `{"Action":"run","Package":"ex/a","Test":"TestOK"}
{"Action":"pass","Package":"ex/a","Test":"TestOK"}
{"Action":"run","Package":"ex/a","Test":"TestBad"}
{"Action":"output","Package":"ex/a","Test":"TestBad","Output":"    a_test.go:9: got 1, want 2\n"}
{"Action":"fail","Package":"ex/a","Test":"TestBad"}
{"Action":"output","Package":"ex/a","Test":"TestTable/case","Output":"    a_test.go:20: boom\n"}
{"Action":"fail","Package":"ex/a","Test":"TestTable/case"}
{"Action":"fail","Package":"ex/a","Test":"TestTable"}
{"Action":"fail","Package":"ex/a"}
{"Action":"skip","Package":"ex/a","Test":"TestLater"}
{"ImportPath":"ex/b","Action":"build-output","Output":"b.go:3:1: syntax error\n"}
{"Action":"fail","Package":"ex/b"}
not json
`
	sum := parseTestEvents(strings.NewReader(stream))
	if sum.Passed != 1 || sum.Failed != 3 || sum.Skipped != 1 {
		t.Fatalf("counts = %s, want 1 passed, 3 failed, 1 skipped", sum)
	}
	if got := sum.Failures[0]; got.Test != "TestBad" || !strings.Contains(got.Output, "want 2") {
		t.Errorf("first failure = %+v, want TestBad with its output", got)
	}
	if got := sum.Failures[1]; got.Test != "TestTable/case" {
		t.Errorf("second failure = %+v, want only the subtest, not its parent", got)
	}
	if got := sum.Failures[2]; got.Package != "ex/b" || got.Test != "" || !strings.Contains(got.Output, "syntax error") {
		t.Errorf("third failure = %+v, want package-level build failure for ex/b", got)
	}
}

func TestParseCoverProfile(t *testing.T) {
	profile := `mode: atomic
ex/a/a.go:1.1,2.2 3 1
ex/a/a.go:3.1,4.2 1 0
ex/a/a.go:3.1,4.2 1 5
ex/b/b.go:1.1,2.2 4 0
ex/tools/x/x.go:1.1,2.2 100 0
`
	info, err := parseCoverProfile(strings.NewReader(profile), []string{"/tools/"})
	if err != nil {
		t.Fatal(err)
	}
	// a: 4/4 (duplicate block merged as covered); b: 0/4; tools excluded.
	if math.Abs(info.Total-50) > 1e-9 {
		t.Errorf("total = %v, want 50", info.Total)
	}
	if len(info.Packages) != 2 || info.Packages[0].Package != "ex/b" || info.Packages[1].Percent != 100 {
		t.Errorf("packages = %+v, want [ex/b 0%%, ex/a 100%%]", info.Packages)
	}

	if _, err := parseCoverProfile(strings.NewReader("garbage\n"), nil); err == nil {
		t.Error("malformed profile: want error")
	}
}

func TestParseFuzzList(t *testing.T) {
	out := `FuzzA
FuzzB
ok  	ex/a	0.10s
?   	ex/cmd	[no test files]
ok  	ex/none	0.05s
FuzzC
ok  	ex/c	0.07s
`
	got := parseFuzzList([]byte(out))
	want := []FuzzTarget{{"ex/a", "FuzzA"}, {"ex/a", "FuzzB"}, {"ex/c", "FuzzC"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestCapLines(t *testing.T) {
	got := capLines([]string{"a", "b", "c"}, 2)
	if len(got) != 3 || got[2] != "... 1 more (see .eval/)" {
		t.Errorf("capLines = %q", got)
	}
}
