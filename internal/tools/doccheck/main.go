// Finding the files and reporting on them: argument handling, the directory
// walk, parsing, the one whole-package rule (a single package comment), and the
// exit code. The rules that judge a file live in check.go.

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// exitFailure is the status returned when anything is undocumented. Any finding
// is a failure: the gate exists so a gap cannot be merged and argued about
// later, and a warning nobody has to act on would not do that.
const exitFailure = 1

// main runs the checker over the packages named on the command line, or over
// `./...` when none are named, and exits non-zero if anything is undocumented.
func main() {
	n, err := run(os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doccheck: %v\n", err)
		os.Exit(exitFailure)
	}
	if n > 0 {
		os.Exit(exitFailure)
	}
}

// run checks the given package patterns and returns how many findings it
// printed.
//
// Patterns are directories, with or without a trailing `/...` for "and
// everything below". An empty list means `./...`. Findings go to problems, one
// per line, sorted by file and position; a one-line summary goes to out. The
// error result is reserved for a pattern that cannot be walked or a file that
// cannot be parsed — a defect in the checker's input, not in the documentation.
func run(patterns []string, out, problems io.Writer) (int, error) {
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	paths, err := goFiles(patterns)
	if err != nil {
		return 0, err
	}
	findings, files, err := checkFiles(paths)
	if err != nil {
		return 0, err
	}
	for _, f := range findings {
		fmt.Fprintln(problems, f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(out, "doccheck: %d undocumented item(s) in %d file(s)\n", len(findings), files)
		return len(findings), nil
	}
	fmt.Fprintf(out, "doccheck: %d file(s) documented\n", files)
	return 0, nil
}

// checkFiles parses and checks a set of files, returning the findings in source
// order together with the number of files examined.
//
// Generated files are skipped: their contents are the generator's business.
// Beyond the per-file rules it applies the one rule that spans a package — that
// no directory declares its package comment twice — which is what keeps the
// `Package x …` sentence in doc.go and stops a file header from quietly
// becoming a second one.
func checkFiles(paths []string) ([]Finding, int, error) {
	fset := token.NewFileSet()
	var findings []Finding
	pkgDoc := map[string]string{} // directory -> the file that already documents it.
	examined := 0
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, 0, fmt.Errorf("parsing %s: %w", path, err)
		}
		if ast.IsGenerated(file) {
			continue
		}
		examined++
		findings = append(findings, check(fset, file, strings.HasSuffix(path, "_test.go"))...)
		if f, ok := duplicatePackageDoc(fset, file, path, pkgDoc); ok {
			findings = append(findings, f)
		}
	}
	slices.SortStableFunc(findings, func(a, b Finding) int { return comparePos(a.Pos, b.Pos) })
	return findings, examined, nil
}

// duplicatePackageDoc records that a file carries the package comment and
// reports the second and later files in the same directory that also do.
//
// Two package comments are not an error to the compiler; they are an error to a
// reader, because godoc concatenates them in an order nobody chose. seen is
// updated in place, mapping each directory to the first file that documented it.
func duplicatePackageDoc(fset *token.FileSet, file *ast.File, path string, seen map[string]string) (Finding, bool) {
	if file.Doc == nil {
		return Finding{}, false
	}
	dir := filepath.Dir(path)
	if first, ok := seen[dir]; ok {
		return Finding{
			Pos:     fset.Position(file.Doc.Pos()),
			Message: fmt.Sprintf("second package comment in this directory; %s already has one", filepath.Base(first)),
		}, true
	}
	seen[dir] = path
	return Finding{}, false
}

// comparePos orders findings the way a reader reads a build log: by file, then
// down the file.
func comparePos(a, b token.Position) int {
	if c := strings.Compare(a.Filename, b.Filename); c != 0 {
		return c
	}
	if a.Line != b.Line {
		return a.Line - b.Line
	}
	return a.Column - b.Column
}

// goFiles expands package patterns into the .go files to check, sorted so a run
// reports the same thing twice.
//
// Directories the Go tool itself ignores are skipped — anything named testdata,
// and anything beginning with a dot or an underscore — as is bin/, which holds
// build output rather than source.
func goFiles(patterns []string) ([]string, error) {
	var paths []string
	for _, pattern := range patterns {
		found, err := goFilesUnder(patternRoot(pattern))
		if err != nil {
			return nil, err
		}
		paths = append(paths, found...)
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// patternRoot splits a package pattern into the directory to start from and
// whether to descend. Patterns use forward slashes, as the go command's do:
// "./..." and "..." both mean "here and below", "internal/cli" means that one
// directory.
func patternRoot(pattern string) (root string, recursive bool) {
	root, recursive = strings.CutSuffix(pattern, "...")
	root = strings.TrimSuffix(root, "/")
	if root == "" {
		root = "."
	}
	return filepath.Clean(root), recursive
}

// goFilesUnder lists the .go files in one directory, descending into
// subdirectories only when recursive is set.
func goFilesUnder(root string, recursive bool) ([]string, error) {
	var paths []string
	//nolint:gosec // the root is a package pattern the operator typed; walking exactly that is the point
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if !recursive || skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", root, err)
	}
	return paths, nil
}

// skipDir reports whether a directory holds something other than the module's
// own source, using the same names the go command skips.
func skipDir(name string) bool {
	return name == "testdata" || name == "bin" || name == "vendor" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}
