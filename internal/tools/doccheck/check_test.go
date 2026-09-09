// Tests for the per-file rules: what counts as a header, what counts as
// documented, and what counts as starting with the right name. Each case parses
// a source string rather than a fixture file, so the shape being tested is
// visible next to the expectation. The file walk is covered in main_test.go.

package main

import (
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// checkSrc parses one source string as the named file and returns the findings
// as strings, so a table case can assert on the message a reader would see.
func checkSrc(t *testing.T, name, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	findings := check(fset, file, strings.HasSuffix(name, "_test.go"))
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Message)
	}
	return out
}

func TestHeaderComment(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool // want a "no header comment" finding.
	}{
		{"none", "package p\n", true},
		{"package comment", "// Package p does a thing.\npackage p\n", false},
		{"detached header", "// What this file owns.\n\npackage p\n", false},
		{"build tag only", "//go:build tuiprobe\n\npackage p\n", true},
		{"build tag then header", "//go:build tuiprobe\n\n// What this file owns.\n\npackage p\n", false},
		{"block comment", "/* What this file owns. */\n\npackage p\n", false},
		{"comment after package", "package p\n\n// Not a header.\nvar x = 1 // x\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkSrc(t, "f.go", tc.src)
			has := false
			for _, m := range got {
				if strings.Contains(m, "no header comment") {
					has = true
				}
			}
			if has != tc.want {
				t.Errorf("header finding = %v, want %v (findings: %v)", has, tc.want, got)
			}
		})
	}
}

// header is prepended to every declaration case so the file-header rule does not
// fire and drown the finding under test.
const header = "// Owns the declarations under test.\n\npackage p\n"

func TestDeclarationsNeedDocComments(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"undocumented func", "func f() {}\n", []string{"func f has no doc comment"}},
		{"documented func", "// f does a thing.\nfunc f() {}\n", nil},
		{"undocumented type", "type t struct{}\n", []string{"type t has no doc comment"}},
		{"undocumented method", "type t struct{}\n\nfunc (t) m() {}\n", []string{
			"type t has no doc comment", "method m has no doc comment",
		}},
		{"undocumented const", "const c = 1\n", []string{"const c has no doc comment"}},
		{"undocumented var", "var v = 1\n", []string{"var v has no doc comment"}},
		{"blank identifier is skipped", "var _ = 1\n", nil},
		{"imports need nothing", "import \"fmt\"\n\nvar _ = fmt.Sprint\n", nil},
		{
			// A group doc explains the set; it does not excuse the members.
			// Before this rule, one line above a parenthesised block licensed
			// any number of undocumented names underneath it.
			"grouped block comment alone is not enough",
			"// The colours pimctl uses.\nconst (\n\ta = 1\n\tb = 2\n)\n",
			[]string{"const a has no doc comment", "const b has no doc comment"},
		},
		{
			"grouped block comment plus trailing comments",
			"// The colours pimctl uses.\nconst (\n\ta = 1 // red, for a failure.\n\tb = 2 // green, for success.\n)\n",
			nil,
		},
		{
			"grouped without any comment reports each spec",
			"const (\n\ta = 1\n\tb = 2\n)\n",
			[]string{"const a has no doc comment", "const b has no doc comment"},
		},
		{
			"grouped with per-spec comments",
			"const (\n\t// a is the first one.\n\ta = 1\n\t// b is the second one.\n\tb = 2\n)\n",
			nil,
		},
		{
			// A directive is attached as a doc comment but says nothing to a
			// reader, so a //nolint above a block used to document it.
			"directive is not documentation",
			"//nolint:gochecknoglobals // package state\nvar (\n\ta = 1\n\tb = 2\n)\n",
			[]string{"var a has no doc comment", "var b has no doc comment"},
		},
		{
			"directive above an ungrouped declaration",
			"//go:generate stringer -type=k\ntype k int\n",
			[]string{"type k has no doc comment"},
		},
		{
			// One comment documents the whole line, so it may open with either
			// name — but it must open with one of them.
			"multi-name spec documented under one of its names",
			"// a and b are the two halves.\nvar a, b = 1, 2\n",
			nil,
		},
		{
			"multi-name spec documented under neither name",
			"// Two useful numbers to have around.\nvar a, b = 1, 2\n",
			[]string{`doc comment on var a should start with "a"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFindings(t, checkSrc(t, "f.go", header+"\n"+tc.src), tc.want)
		})
	}
}

func TestDocCommentMustStartWithTheName(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"plain name", "// Label renders one scope.\nfunc Label() {}\n", nil},
		{"article", "// A Palette decides on colour.\ntype Palette struct{}\n", nil},
		{"the", "// The Row a table prints.\ntype Row struct{}\n", nil},
		{"doc link", "// [Row] is one eligible role.\ntype Row struct{}\n", nil},
		{
			"wrong name",
			"// Renders one scope.\nfunc Label() {}\n",
			[]string{`doc comment on func Label should start with "Label"`},
		},
		{
			"prefix is not enough",
			"// Labeller renders one scope.\nfunc Label() {}\n",
			[]string{`doc comment on func Label should start with "Label"`},
		},
		{
			"spec comment inside a documented group",
			"// The keys.\nconst (\n\t// wrong text.\n\tk = 1\n)\n",
			[]string{`doc comment on const k should start with "k"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFindings(t, checkSrc(t, "f.go", header+"\n"+tc.src), tc.want)
		})
	}
}

func TestTestFilesNeedOnlyAHeader(t *testing.T) {
	src := "// Covers the thing.\n\npackage p\n\nfunc TestThing() {}\n\ntype helper struct{}\n"
	assertFindings(t, checkSrc(t, "f_test.go", src), nil)

	bare := "package p\n\nfunc TestThing() {}\n"
	assertFindings(t, checkSrc(t, "f_test.go", bare), []string{
		"file has no header comment above its package clause",
	})
}

func TestIsDirective(t *testing.T) {
	cases := map[string]bool{
		"//go:build tuiprobe":                  true,
		"//line f.go:1":                        true,
		"//nolint:gosec // path is ours":       true,
		"// +build tuiprobe":                   true,
		"// What this file owns.":              false,
		"// Ratio a:b is the interesting one.": false,
		"/* a block */":                        false,
	}
	for text, want := range cases {
		if got := isDirective(text); got != want {
			t.Errorf("isDirective(%q) = %v, want %v", text, got, want)
		}
	}
}

// assertFindings compares findings against the messages a case expects, in
// order. It reports the whole slice on a mismatch: which findings were produced
// is more useful than which one differed first.
func assertFindings(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("findings =\n  %q\nwant\n  %q", got, want)
	}
}

// TestVacuousDocComments: a comment that restates the identifier passes every
// mechanical rule and tells the next reader nothing, which is the failure mode
// a documentation gate exists to prevent.
func TestVacuousDocComments(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			"says nothing beyond the name",
			"// Timeout is the timeout.\nvar Timeout = 1\n",
			[]string{"doc comment on var Timeout says nothing beyond its name; say what it does and why"},
		},
		{
			"name repeated with an article",
			"// The Timeout.\nvar Timeout = 1\n",
			[]string{"doc comment on var Timeout says nothing beyond its name; say what it does and why"},
		},
		{
			"short but substantive",
			"// Timeout bounds one ARM call.\nvar Timeout = 1\n",
			nil,
		},
		{
			"does not end a sentence",
			"// Timeout bounds one ARM call\nvar Timeout = 1\n",
			[]string{"doc comment on var Timeout does not end in a full stop"},
		},
		{
			// A code block or list is where a full stop would be wrong, and
			// godot makes the same exemption.
			"ends in an indented block",
			"// Timeout bounds one ARM call:\n//\n//\tvar Timeout = 1\nvar Timeout = 1\n",
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFindings(t, checkSrc(t, "f.go", header+"\n"+tc.src), tc.want)
		})
	}
}

// TestStructFieldsAndInterfaceMethods: CLAUDE.md asks for a comment on any
// field whose meaning is not self-evident, which no checker can judge, so the
// mechanical rule is that every field and every interface method carries one.
// Nested anonymous structs count, because a wire format's inner fields are
// exactly where the meaning hides.
func TestStructFieldsAndInterfaceMethods(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			"undocumented fields",
			"// T is a thing.\ntype T struct {\n\tA int\n\tB int\n}\n",
			[]string{"field T.A has no comment", "field T.B has no comment"},
		},
		{
			"trailing comments are enough",
			"// T is a thing.\ntype T struct {\n\tA int // how many.\n\tB int // how few.\n}\n",
			nil,
		},
		{
			"doc comments are enough",
			"// T is a thing.\ntype T struct {\n\t// A is how many.\n\tA int\n}\n",
			nil,
		},
		{
			"embedded field",
			"// T is a thing.\ntype T struct {\n\tsync.Mutex\n}\n",
			[]string{"field T.sync.Mutex has no comment"},
		},
		{
			"nested anonymous struct",
			"// T is a thing.\ntype T struct {\n\tA struct {\n\t\tB int\n\t} // outer.\n}\n",
			[]string{"field T.A.B has no comment"},
		},
		{
			"anonymous struct behind a slice",
			"// T is a thing.\ntype T struct {\n\tA []struct {\n\t\tB int\n\t} // outer.\n}\n",
			[]string{"field T.A.B has no comment"},
		},
		{
			"interface methods",
			"// R reads.\ntype R interface {\n\tRead() error\n}\n",
			[]string{"interface method R.Read has no comment"},
		},
		{
			"documented interface method",
			"// R reads.\ntype R interface {\n\tRead() error // reads one thing.\n}\n",
			nil,
		},
		{
			"a test file is exempt",
			"// T is a thing.\ntype T struct {\n\tA int\n}\n",
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "f.go"
			if strings.Contains(tc.name, "test file") {
				name = "f_test.go"
			}
			assertFindings(t, checkSrc(t, name, header+"\n"+tc.src), tc.want)
		})
	}
}
