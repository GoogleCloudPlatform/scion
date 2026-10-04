// Command checkclitimezones fails when CLI code formats an absolute time
// with a layout that has no zone token.
//
// The scion CLI shows every absolute time in the display zone (the local
// zone, or --tz/--utc) and always prints the zone, through pkg/clitime. A
// layout such as "2006-01-02 15:04" prints a wall-clock time that the reader
// cannot place, so this check rejects it in CLI code.
//
// # Rules
//
// zoneless-layout: a string literal that is a Go time layout (it contains a
// date or clock reference token: 2006, 15:04, 03:04 or 3:04) and has no zone
// reference token (MST, Z07:00:00, Z07:00, Z0700, Z07, -07:00:00, -07:00,
// -0700 or -07).
//
// zoneless-const: a reference to one of the time package's layout constants
// that has no zone: ANSIC, Kitchen, Stamp, StampMilli, StampMicro, StampNano,
// DateTime, DateOnly and TimeOnly.
//
// # Exemptions
//
// The layout argument of time.Parse and time.ParseInLocation is exempt: it
// describes input, and a zoneless parse layout is how a caller states that
// the input carries no zone. Only a literal or constant written directly as
// that argument is exempt; a named constant that is also used to format is
// checked where it is declared.
//
// Test files (_test.go) and testdata directories are not scanned.
//
// # Blind spots
//
// The check is syntactic (go/ast, no type information). It does not see
// layouts built at run time (concatenation, fmt.Sprintf), and it does not
// check which zone a time is converted to before formatting. A zoned layout
// on a time that was not converted to the display zone still passes; the
// convention that CLI output goes through pkg/clitime covers that.
//
// # Usage
//
//	checkclitimezones [path ...]
//
// Each path is a directory (scanned recursively) or a single .go file,
// relative to the current directory. The default is the CLI scope: cmd and
// pkg/agent/list.go.
//
// Exit codes: 0 no violations, 1 violations, 3 could not analyse (parse
// error, unreadable path), 4 no candidate files.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultPaths is the CLI scope scanned when no path is given.
var DefaultPaths = []string{"cmd", "pkg/agent/list.go"}

// Finding is one violation.
type Finding struct {
	File   string
	Line   int
	Column int
	Rule   string
	Detail string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: %s", f.File, f.Line, f.Column, f.Rule, f.Detail)
}

var (
	// layoutRE matches a date or clock reference token, which marks a
	// string as a time layout.
	layoutRE = regexp.MustCompile(`2006|15:04|0?3:04`)
	// zoneRE matches any zone reference token. Longer forms come first so
	// the alternation documents them; any match is enough.
	zoneRE = regexp.MustCompile(`MST|Z07:00:00|Z07:00|Z0700|Z07|-07:00:00|-07:00|-0700|-07`)
)

// zonelessConsts are the time package layout constants without a zone.
var zonelessConsts = map[string]bool{
	"ANSIC":      true,
	"Kitchen":    true,
	"Stamp":      true,
	"StampMilli": true,
	"StampMicro": true,
	"StampNano":  true,
	"DateTime":   true,
	"DateOnly":   true,
	"TimeOnly":   true,
}

// IsZonelessLayout reports whether s is a time layout without a zone token.
func IsZonelessLayout(s string) bool {
	return layoutRE.MatchString(s) && !zoneRE.MatchString(s)
}

// CollectFiles returns the non-test .go files under paths, sorted.
func CollectFiles(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	add := func(p string) {
		p = filepath.ToSlash(filepath.Clean(p))
		if !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if isCandidate(root) {
				add(root)
			}
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || (path != root && strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if isCandidate(path) {
				add(path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

func isCandidate(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

// ScanFile parses one file and returns its findings.
func ScanFile(fset *token.FileSet, path string) ([]Finding, error) {
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	timeName := timeImportName(file)

	// Pass 1: the layout arguments of time.Parse/ParseInLocation.
	exempt := map[ast.Expr]bool{}
	if timeName != "" {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if name, ok := timeSelector(call.Fun, timeName); ok && (name == "Parse" || name == "ParseInLocation") {
				exempt[call.Args[0]] = true
			}
			return true
		})
	}

	// Pass 2: zoneless literals and constants.
	var findings []Finding
	report := func(n ast.Node, rule, detail string) {
		pos := fset.Position(n.Pos())
		findings = append(findings, Finding{
			File:   filepath.ToSlash(path),
			Line:   pos.Line,
			Column: pos.Column,
			Rule:   rule,
			Detail: detail,
		})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if ok && exempt[expr] {
			return false
		}
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(x.Value)
			if err != nil {
				return true
			}
			if IsZonelessLayout(s) {
				report(x, "zoneless-layout", fmt.Sprintf("time layout %s has no zone token; use clitime.Format (pkg/clitime) or add a zone token such as MST or -07:00", x.Value))
			}
		case *ast.SelectorExpr:
			if timeName == "" {
				return true
			}
			if name, ok := timeSelector(x, timeName); ok && zonelessConsts[name] {
				report(x, "zoneless-const", fmt.Sprintf("time.%s has no zone token; use clitime.Format (pkg/clitime) or a layout with a zone token", name))
			}
		}
		return true
	})
	return findings, nil
}

// timeImportName returns the name the file uses for package time, or "" if
// the file does not import it.
func timeImportName(file *ast.File) string {
	for _, imp := range file.Imports {
		if imp.Path.Value != `"time"` {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return ""
			}
			return imp.Name.Name
		}
		return "time"
	}
	return ""
}

// timeSelector reports whether expr is <timeName>.<Sel> and returns Sel.
func timeSelector(expr ast.Expr, timeName string) (string, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != timeName {
		return "", false
	}
	return sel.Sel.Name, true
}

// Scan scans every candidate file under paths.
func Scan(paths []string) (files []string, findings []Finding, err error) {
	files, err = CollectFiles(paths)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	for _, f := range files {
		ff, err := ScanFile(fset, f)
		if err != nil {
			return files, nil, err
		}
		findings = append(findings, ff...)
	}
	return files, findings, nil
}

func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		paths = DefaultPaths
	}
	files, findings, err := Scan(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-cli-time-zones: could not analyse: %v\n", err)
		os.Exit(3)
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "check-cli-time-zones: no candidate files under %s (wrong working directory?)\n", strings.Join(paths, ", "))
		os.Exit(4)
	}
	for _, f := range findings {
		fmt.Fprintln(os.Stderr, f.String())
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "check-cli-time-zones: %d violation(s) in %d file(s) scanned\n", len(findings), len(files))
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "check-cli-time-zones: OK, %d file(s) scanned, no zoneless time layouts\n", len(files))
}
