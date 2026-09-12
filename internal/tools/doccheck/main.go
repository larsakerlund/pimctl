// Finding the files and reporting on them: the command line and its usage
// text, the directory walk, parsing, the one whole-package rule (a single
// package comment), and the exit code. The rules that judge a file live in
// check.go.

package main

import (
	"errors"
	"flag"
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

// The exit statuses, one per way a run can end. They are distinct so a Makefile
// or a script can tell "the code is undocumented" from "the command line was
// wrong" — the second is a defect in the caller, and repeating the run will not
// change it.
const (
	// exitFailure is the status when anything is undocumented, or when a
	// package cannot be walked or parsed. Any finding is a failure: the gate
	// exists so a gap cannot be merged and argued about later, and a warning
	// nobody has to act on would not do that.
	exitFailure = 1
	// exitUsage is the status for a flag the checker does not know, the value
	// the flag package and the go command use for the same thing.
	exitUsage = 2
)

// usage is what -h prints: the rules the checker applies, how packages are
// named, and what each exit status means. It lives next to the code that
// applies the exit statuses so the text and the numbers change together.
const usage = `usage: doccheck [packages...]

doccheck is the documentation gate ` + "`make doccheck`" + ` runs. It parses every
non-generated Go file in the named packages and reports:

  - a file with no header comment above its package clause;
  - a top-level declaration, exported or not, with no doc comment
    (test files are held to the header rule only);
  - a doc comment that does not start with the name it documents, says
    nothing beyond that name, or does not end in a full stop;
  - a struct field or interface method with no comment, in non-test files;
  - a package with more than one package comment.

Packages are directories, with or without a trailing /... for "and everything
below"; the default is ./... from the working directory. Findings go to stderr
as file:line:col: message, one per line, and a one-line summary goes to stdout.

Exit status:
  0  every file examined is documented
  1  at least one finding, or a package that cannot be walked or parsed
  2  a flag doccheck does not know
`

// usageError marks a command line the checker could not parse — a flag it does
// not know — so main can exit with exitUsage rather than exitFailure. It wraps
// the flag package's own error, whose wording names the offending flag.
type usageError struct {
	err error // the flag package's error, e.g. "flag provided but not defined: -x".
}

// Error reports the flag package's message and where to find the usage text.
func (e usageError) Error() string {
	return e.err.Error() + " (run doccheck -h for usage)"
}

// Unwrap exposes the flag package's error to errors.Is and errors.As.
func (e usageError) Unwrap() error {
	return e.err
}

// main runs the checker over the packages named on the command line, or over
// `./...` when none are named, and exits with the exitStatus of the result: 0
// when everything is documented or -h was asked for, exitFailure when anything
// is undocumented or unreadable, exitUsage for a flag it does not know.
func main() {
	n, err := run(os.Args[1:], os.Stdout, os.Stderr)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "doccheck: %v\n", err)
	}
	if code := exitStatus(n, err); code != 0 {
		os.Exit(code)
	}
}

// exitStatus maps a run's result to the process exit status: 0 for a clean run
// or a request for help (the usage text has already been printed), exitUsage
// for a usageError, exitFailure for any other error or for n > 0 findings.
func exitStatus(n int, err error) int {
	var usageErr usageError
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &usageErr):
		return exitUsage
	case err != nil, n > 0:
		return exitFailure
	default:
		return 0
	}
}

// run parses the command line, checks the packages it names and returns how
// many findings it printed.
//
// args is the command line without the program name: flags first, then
// package patterns. Patterns are directories, with or without a trailing
// `/...` for "and everything below"; none means `./...`. Findings go to
// problems, one per line, sorted by file and position; a one-line summary goes
// to out. The error result is flag.ErrHelp after the usage text has been
// written to out, a usageError for a flag it does not know, or an error for a
// pattern that cannot be walked or a file that cannot be parsed — a defect in
// the checker's input, not in the documentation.
func run(args []string, out, problems io.Writer) (int, error) {
	patterns, err := parseArgs(args, out)
	if err != nil {
		return 0, err
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

// parseArgs separates flags from package patterns and returns the patterns,
// defaulting to `./...` when none are given.
//
// The only flags are the flag package's own -h, -help and --help, which write
// usage to out and return flag.ErrHelp. Any other flag is a usageError. The
// flag package's own usage printing is switched off because it writes both
// cases to one stream, and help belongs on stdout while an error belongs on
// stderr.
func parseArgs(args []string, out io.Writer) ([]string, error) {
	flags := flag.NewFlagSet("doccheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return nil, err
		}
		return nil, usageError{err: err}
	}
	patterns := flags.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	return patterns, nil
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
