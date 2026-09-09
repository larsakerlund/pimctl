// The rules themselves: given one parsed file, what documentation is missing or
// misnamed. Nothing here touches the filesystem or decides which files to look
// at — walking the tree, parsing and reporting live in main.go — so every rule
// is testable from a source string.

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

// Finding is one documentation defect: where it is and what to say about it.
//
// A Finding holds no reference to the AST it came from, so findings from many
// files can be collected, sorted and printed after their parse trees are gone.
// The zero value is not meaningful; findings are only ever produced by [check].
type Finding struct {
	Pos     token.Position // resolved position, so the message can name file and line.
	Message string         // one line, no trailing period, phrased like a vet diagnostic.
}

// String renders a finding in the `file:line:col: message` form that compilers,
// go vet and editors' error matchers all understand.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s", f.Pos.Filename, f.Pos.Line, f.Pos.Column, f.Message)
}

// check reports every documentation defect in one parsed file, in source order.
//
// isTest relaxes the rules to the header alone: a test file must still say what
// it covers, but individual test functions need no doc comment, because a table
// test's cases describe it better than a sentence above it would. Generated
// files are skipped entirely by the caller, not here.
//
// The returned slice is empty, never nil, when the file is clean.
func check(fset *token.FileSet, file *ast.File, isTest bool) []Finding {
	findings := []Finding{}
	if f, ok := headerFinding(fset, file); ok {
		findings = append(findings, f)
	}
	if isTest {
		return findings
	}
	for _, decl := range file.Decls {
		findings = append(findings, declFindings(fset, decl)...)
	}
	return findings
}

// headerFinding reports the file's missing header comment, if it is missing.
//
// A header is any prose comment above the `package` clause, whether it is the
// package comment itself (doc.go, and the two `package main` files) or a plain
// block separated from `package` by a blank line, which is how every other file
// carries one without becoming a second package comment. Build constraints and
// other `//go:` directives do not count: they are instructions to the toolchain,
// not a description of the file.
//
// The second result is false when the file is fine.
func headerFinding(fset *token.FileSet, file *ast.File) (Finding, bool) {
	if hasHeaderComment(file) {
		return Finding{}, false
	}
	return Finding{
		Pos:     fset.Position(file.Package),
		Message: "file has no header comment above its package clause",
	}, true
}

// hasHeaderComment reports whether any prose comment precedes the `package`
// clause. file.Doc alone is not enough to ask about: it is set only when the
// comment touches `package` with no blank line, which is exactly the shape most
// files here deliberately avoid.
func hasHeaderComment(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			return false // comments are in source order; nothing left to look at.
		}
		if isProse(group) {
			return true
		}
	}
	return false
}

// isProse reports whether a comment group says anything to a reader, as opposed
// to consisting only of toolchain directives such as //go:build.
func isProse(group *ast.CommentGroup) bool {
	for _, c := range group.List {
		if !isDirective(c.Text) && strings.TrimSpace(strings.Trim(c.Text, "/*")) != "" {
			return true
		}
	}
	return false
}

// isDirective reports whether one comment is a toolchain directive.
//
// It mirrors the rule the go/ast package applies internally: a `//` comment
// whose text up to the first colon is a run of lower-case letters, digits and
// underscores — //go:build, //nolint:errcheck — plus the handful of spellings
// that predate that convention (//line, //extern, //export, `// +build`).
// Block comments are never directives.
func isDirective(text string) bool {
	body, ok := strings.CutPrefix(text, "//")
	if !ok {
		return false
	}
	for _, prefix := range []string{" +build", "line ", "extern ", "export "} {
		if strings.HasPrefix(body, prefix) {
			return true
		}
	}
	colon := strings.Index(body, ":")
	if colon <= 0 {
		return false
	}
	for _, b := range []byte(body[:colon]) {
		lower := b >= 'a' && b <= 'z'
		digit := b >= '0' && b <= '9'
		if !lower && !digit && b != '_' {
			return false
		}
	}
	return true
}

// declFindings reports the defects of one top-level declaration.
//
// Imports are skipped: `import` blocks carry no identifier to document. Anything
// else — func, type, const, var, exported or not — needs a doc comment that
// starts with the name it documents.
func declFindings(fset *token.FileSet, decl ast.Decl) []Finding {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return nameFindings(fset, d.Doc, d.Name, funcKind(d))
	case *ast.GenDecl:
		if d.Tok == token.IMPORT {
			return nil
		}
		return genDeclFindings(fset, d)
	default:
		return nil
	}
}

// funcKind names a declaration for the message: "method" reads better than
// "func" when the reader has to find it again in a file of both.
func funcKind(d *ast.FuncDecl) string {
	if d.Recv != nil {
		return "method"
	}
	return "func"
}

// genDeclFindings reports the defects of one type, const or var declaration,
// grouped or not.
//
// A parenthesised group may be documented as a whole — the block comment above
// a set of related constants describes them better than one sentence each would
// — but then every member still has to carry a line of its own, a trailing
// comment being enough. Without that rule a group doc licensed any number of
// undocumented names underneath it.
//
// An ungrouped declaration is documented through the declaration, which is
// where Go's parser puts the comment. A multi-name line — `var a, b = 1, 2` —
// is documented by that one comment, which must open with one of the names.
func genDeclFindings(fset *token.FileSet, d *ast.GenDecl) []Finding {
	if len(specNames(d)) == 0 {
		return nil // a group of blanks only; there is nothing to name.
	}
	var findings []Finding
	for _, spec := range d.Specs {
		findings = append(findings, specFindings(fset, d, spec)...)
		findings = append(findings, memberFindings(fset, spec)...)
	}
	return findings
}

// specFindings reports the defects of one spec: whether it is documented at
// all, and whether whatever documents it opens with one of its own names.
//
// Where the documentation may live depends on the shape. Outside a group the
// declaration's own comment is the spec's. Inside one, a spec may carry its own
// doc comment, or lean on the group's — in which case it must still say
// something for itself, and a trailing comment counts.
func specFindings(fset *token.FileSet, d *ast.GenDecl, spec ast.Spec) []Finding {
	names := specIdents(spec)
	if len(names) == 0 {
		return nil
	}
	grouped := d.Lparen.IsValid()
	doc := specDoc(spec)
	if !grouped && !documented(doc) {
		doc = d.Doc
	}
	if documented(doc) {
		return specNameFindings(fset, d, spec, doc)
	}
	if grouped && documented(d.Doc) && hasTrailingComment(spec) {
		return nil // the group explains the set, the line explains itself.
	}
	var findings []Finding
	for _, name := range names {
		if name.Name == "_" {
			continue
		}
		findings = append(findings, undocumented(fset, name.Pos(), declKind(d), name.Name))
	}
	return findings
}

// specNameFindings checks a documented spec's comment against its names. A
// multi-name line is documented once, so opening with any one of its names is
// enough; Go's own convention would put the first there.
func specNameFindings(fset *token.FileSet, d *ast.GenDecl, spec ast.Spec, doc *ast.CommentGroup) []Finding {
	names := specIdents(spec)
	for _, name := range names {
		if name.Name == "_" {
			continue
		}
		if docStartsWithName(doc, name.Name) {
			return substanceFindings(fset, doc, name.Name, declKind(d))
		}
	}
	// No name matched: report against the first real one, which is the name Go
	// convention says the comment should have opened with.
	for _, name := range names {
		if name.Name != "_" {
			return nameFindings(fset, doc, name, declKind(d))
		}
	}
	return nil
}

// hasTrailingComment reports whether a spec carries the `// …` that follows it
// on the same line, which is how a member of a documented group says what it is
// without a paragraph of its own.
func hasTrailingComment(spec ast.Spec) bool {
	switch s := spec.(type) {
	case *ast.ValueSpec:
		return isProseGroup(s.Comment)
	case *ast.TypeSpec:
		return isProseGroup(s.Comment)
	default:
		return false
	}
}

// isProseGroup reports whether a comment group exists and says something.
func isProseGroup(group *ast.CommentGroup) bool {
	return group != nil && strings.TrimSpace(group.Text()) != ""
}

// memberFindings reports struct fields and interface methods with nothing said
// about them.
//
// CLAUDE.md asks for a comment on any field whose meaning is not self-evident,
// which no checker can judge, so the mechanical rule is stricter: every field
// and every interface method carries a comment, doc or trailing. Nested
// anonymous structs are walked too, because a JSON shape's inner fields are
// exactly where the meaning of a wire format hides.
func memberFindings(fset *token.FileSet, spec ast.Spec) []Finding {
	ts, ok := spec.(*ast.TypeSpec)
	if !ok {
		return nil
	}
	return fieldFindings(fset, ts.Name.Name, ts.Type)
}

// fieldFindings walks one type expression, reporting undocumented members of
// any struct or interface inside it.
func fieldFindings(fset *token.FileSet, owner string, expr ast.Expr) []Finding {
	var findings []Finding
	switch t := expr.(type) {
	case *ast.StructType:
		for _, f := range t.Fields.List {
			findings = append(findings, oneFieldFindings(fset, owner, "field", f)...)
		}
	case *ast.InterfaceType:
		for _, f := range t.Methods.List {
			findings = append(findings, oneFieldFindings(fset, owner, "interface method", f)...)
		}
	}
	return findings
}

// oneFieldFindings reports one field or interface method, and walks whatever it
// is made of: a field can itself be an anonymous struct, or a slice, map,
// pointer or array of one.
func oneFieldFindings(fset *token.FileSet, owner, kind string, f *ast.Field) []Finding {
	var findings []Finding
	if !isProseGroup(f.Doc) && !isProseGroup(f.Comment) {
		findings = append(findings, Finding{
			Pos:     fset.Position(f.Pos()),
			Message: fmt.Sprintf("%s %s.%s has no comment", kind, owner, fieldName(f)),
		})
	}
	return append(findings, fieldFindings(fset, owner+"."+fieldName(f), underlying(f.Type))...)
}

// underlying strips the wrappers a struct can hide behind, so an anonymous
// struct in a slice or a pointer is still checked.
func underlying(expr ast.Expr) ast.Expr {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return underlying(t.X)
	case *ast.ArrayType:
		return underlying(t.Elt)
	case *ast.MapType:
		return underlying(t.Value)
	default:
		return expr
	}
}

// fieldName renders a field for a message: its name, the names joined when a
// line declares several, or the type when the field is embedded.
func fieldName(f *ast.Field) string {
	if len(f.Names) == 0 {
		return embeddedName(f.Type)
	}
	names := make([]string, 0, len(f.Names))
	for _, n := range f.Names {
		names = append(names, n.Name)
	}
	return strings.Join(names, ", ")
}

// embeddedName names an embedded field by its type, which is the only name it
// has.
func embeddedName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return embeddedName(t.X)
	case *ast.SelectorExpr:
		return embeddedName(t.X) + "." + t.Sel.Name
	default:
		return "embedded field"
	}
}

// nameFindings checks one declaration that is expected to be documented under
// its own name: that the doc comment says something, that it opens with that
// name, and that it says more than the name.
func nameFindings(fset *token.FileSet, doc *ast.CommentGroup, name *ast.Ident, kind string) []Finding {
	if name.Name == "_" {
		return nil // a blank identifier names nothing a reader can look up.
	}
	if !documented(doc) {
		return []Finding{undocumented(fset, name.Pos(), kind, name.Name)}
	}
	if !docStartsWithName(doc, name.Name) {
		return []Finding{{
			Pos:     fset.Position(doc.Pos()),
			Message: fmt.Sprintf("doc comment on %s %s should start with %q", kind, name.Name, name.Name),
		}}
	}
	return substanceFindings(fset, doc, name.Name, kind)
}

// documented reports whether a comment group says anything to a reader.
//
// A group can be non-nil and still say nothing: `//nolint:errcheck` or
// `//go:generate` above a declaration is attached as its doc comment, so a
// nil check alone let a suppression directive stand in for documentation.
// CommentGroup.Text drops directives, which is exactly the question being
// asked here.
func documented(doc *ast.CommentGroup) bool {
	return doc != nil && strings.TrimSpace(doc.Text()) != ""
}

// substanceFindings rejects a doc comment that satisfies the shape of the rule
// without doing its job: one that repeats the identifier and stops, or that
// trails off without ending a sentence.
//
// "// Timeout is the timeout." passes every mechanical check a linter makes and
// tells the next reader nothing; the point of the gate is that the second
// sentence is where the reason lives.
func substanceFindings(fset *token.FileSet, doc *ast.CommentGroup, name, kind string) []Finding {
	text := strings.TrimSpace(doc.Text())
	if len(meaningfulWords(text, name)) == 0 {
		return []Finding{{
			Pos: fset.Position(doc.Pos()),
			Message: fmt.Sprintf(
				"doc comment on %s %s says nothing beyond its name; say what it does and why",
				kind, name),
		}}
	}
	if !endsSentence(text) {
		return []Finding{{
			Pos:     fset.Position(doc.Pos()),
			Message: fmt.Sprintf("doc comment on %s %s does not end in a full stop", kind, name),
		}}
	}
	return nil
}

// meaningfulWords returns the doc's words with the identifier, the articles and
// the handful of function words removed, so what is left is what the comment
// actually adds. "Timeout is the timeout." leaves nothing; "R reads." leaves
// one word, which is terse but is not nothing.
func meaningfulWords(text, name string) []string {
	var out []string
	for w := range strings.FieldsSeq(text) {
		word := strings.ToLower(trimWord(w))
		if strings.EqualFold(word, name) || isArticle(w) || stopWords[word] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// stopWords are the words that carry no information about what a declaration
// does. The list is deliberately short: it only has to catch a comment that
// restates its own identifier, not judge prose.
var stopWords = map[string]bool{
	"is": true, "are": true, "was": true, "be": true, "the": true,
	"a": true, "an": true, "of": true, "for": true, "to": true,
	"and": true, "or": true, "in": true, "on": true, "it": true,
	"this": true, "that": true,
}

// endsSentence reports whether the doc's last line closes properly.
//
// An indented line is a code block or a list continuation, where a full stop
// would be wrong, so those are accepted as they are; godot in .golangci.yml
// applies the same exemption.
func endsSentence(text string) bool {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	last := lines[len(lines)-1]
	if last != strings.TrimLeft(last, " \t") {
		return true // indented: a code block or a list item.
	}
	trimmed := strings.TrimRight(last, " \t")
	if trimmed == "" {
		return true
	}
	switch trimmed[len(trimmed)-1] {
	case '.', '!', '?', ':':
		return true
	default:
		return false
	}
}

// undocumented builds the "has no doc comment" finding, so its wording is
// written once and every kind of declaration reports it the same way.
func undocumented(fset *token.FileSet, pos token.Pos, kind, name string) Finding {
	return Finding{
		Pos:     fset.Position(pos),
		Message: fmt.Sprintf("%s %s has no doc comment", kind, name),
	}
}

// declKind names a declaration's token for the message text.
func declKind(d *ast.GenDecl) string {
	return strings.ToLower(d.Tok.String())
}

// specNames returns every non-blank identifier a declaration introduces, in
// source order.
func specNames(d *ast.GenDecl) []*ast.Ident {
	var names []*ast.Ident
	for _, spec := range d.Specs {
		for _, name := range specIdents(spec) {
			if name.Name != "_" {
				names = append(names, name)
			}
		}
	}
	return names
}

// specIdents returns the identifiers one spec declares: one for a type, one or
// more for a const or var line.
func specIdents(spec ast.Spec) []*ast.Ident {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return []*ast.Ident{s.Name}
	case *ast.ValueSpec:
		return s.Names
	default:
		return nil
	}
}

// specDoc returns the doc comment attached to a spec, or nil. Go attaches a
// comment to the spec inside a group and to the declaration outside one, so
// both places have to be asked.
func specDoc(spec ast.Spec) *ast.CommentGroup {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Doc
	case *ast.ValueSpec:
		return s.Doc
	default:
		return nil
	}
}

// docStartsWithName reports whether a doc comment opens with the identifier it
// documents.
//
// Go's convention is that a comment begins with the name, and an article in
// front of it is idiomatic — "A Palette decides…", "The ARM api-version…" — so
// the name may be the first or the second word. Surrounding brackets are
// tolerated because "[Row] is…" is a valid doc link to the thing being
// documented, and trailing punctuation because "Row, unlike…" is a sentence.
func docStartsWithName(doc *ast.CommentGroup, name string) bool {
	words := strings.Fields(doc.Text())
	if len(words) > 0 && trimWord(words[0]) == name {
		return true
	}
	const afterArticle = 2
	return len(words) >= afterArticle && isArticle(words[0]) && trimWord(words[1]) == name
}

// isArticle reports whether a word is an article Go doc comments conventionally
// put before the name they document.
func isArticle(word string) bool {
	switch word {
	case "A", "An", "The":
		return true
	default:
		return false
	}
}

// trimWord strips the brackets of a doc link and any trailing punctuation, so a
// word can be compared against a bare identifier.
func trimWord(word string) string {
	return strings.Trim(word, "[](),.:;'\"`")
}
