// Package forbiddenimports finds direct imports a repository has forbidden.
//
// A forbidden import is named by its import path; it also covers the
// packages below it. A package in the forbidden import's own tree may always
// import it (its implementation and its own tests). Any other package may
// import it only when an allow rule names that package: "<pattern> <import>",
// where the pattern is an import path, or a path ending in "/..." for that
// path and everything below it.
package forbiddenimports

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Rule allows packages matching Pattern to import Import.
type Rule struct {
	Pattern string
	Import  string
}

// Package is the part of `go list -json` the check reads.
type Package struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// Violation is one package importing one forbidden import directly.
type Violation struct {
	Package   string
	Import    string // the import as written
	Forbidden string // the forbidden entry it falls under
}

func (v Violation) String() string {
	return fmt.Sprintf("%s imports %s, which this repository forbids (%s); allow it explicitly in allowed_imports: %s %s", v.Package, v.Import, v.Forbidden, v.Package, v.Forbidden)
}

// ParseForbidden reads one import path per line (a trailing "/..." is
// allowed and means the same); blank lines and lines starting with # are
// ignored. An entry that is not a plain import path is an error: a check
// that silently forbids nothing is worse than a failing one.
func ParseForbidden(text string) ([]string, error) {
	var out []string
	for _, line := range lines(text) {
		path, err := importPath("forbidden_imports", line, line)
		if err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, nil
}

// importPath strips one optional trailing "/..." from raw and validates the
// result per Go import-path syntax (go help importpath).
func importPath(input, line, raw string) (string, error) {
	path := strings.TrimSuffix(raw, "/...")
	if !validImportPath(path) {
		return "", fmt.Errorf("%s line %q: %q is not an import path", input, line, path)
	}
	return path, nil
}

func validImportPath(path string) bool {
	if path == "" || strings.Contains(path, "...") {
		return false
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg[0] == '.' || seg[len(seg)-1] == '.' || seg[0] == '-' {
			return false
		}
		for i := range len(seg) {
			if !isImportPathChar(seg[i]) {
				return false
			}
		}
	}
	return true
}

func isImportPathChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '.' || c == '_' || c == '~' || c == '+'
}

// ParseRules reads "<pattern> <import>" per line; blank lines and lines
// starting with # are ignored.
func ParseRules(text string) ([]Rule, error) {
	var rules []Rule
	for _, line := range lines(text) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("allowed_imports line %q: want \"<package pattern> <import>\"", line)
		}
		if _, err := importPath("allowed_imports", line, fields[0]); err != nil {
			return nil, err
		}
		imp, err := importPath("allowed_imports", line, fields[1])
		if err != nil {
			return nil, err
		}
		rules = append(rules, Rule{Pattern: fields[0], Import: imp})
	}
	return rules, nil
}

func lines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// within reports whether path is root or below it.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// matches reports whether pkg matches pattern ("p" or "p/...").
func matches(pattern, pkg string) bool {
	if root, ok := strings.CutSuffix(pattern, "/..."); ok {
		return within(pkg, root)
	}
	return pkg == pattern
}

// Check returns every direct import of a forbidden path that no rule allows,
// sorted by package and import.
func Check(pkgs []Package, forbidden []string, rules []Rule) []Violation {
	var found []Violation
	for _, pkg := range pkgs {
		owner := pkg.ImportPath
		seen := map[string]bool{}
		for _, list := range [][]string{pkg.Imports, pkg.TestImports, pkg.XTestImports} {
			for _, imp := range list {
				if seen[imp] {
					continue
				}
				seen[imp] = true
				for _, f := range forbidden {
					if !within(imp, f) || within(owner, f) || allowed(rules, owner, imp) {
						continue
					}
					found = append(found, Violation{Package: owner, Import: imp, Forbidden: f})
					break // one violation per import, whichever entries it falls under
				}
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Package != found[j].Package {
			return found[i].Package < found[j].Package
		}
		return found[i].Import < found[j].Import
	})
	return found
}

// allowed reports whether a rule lets pkg import imp: the rule's import is
// imp or a path above it.
func allowed(rules []Rule, pkg, imp string) bool {
	for _, r := range rules {
		if within(imp, r.Import) && matches(r.Pattern, pkg) {
			return true
		}
	}
	return false
}

// DecodePackages reads the concatenated JSON objects `go list -json` prints.
func DecodePackages(r io.Reader) ([]Package, error) {
	dec := json.NewDecoder(bufio.NewReader(r))
	var pkgs []Package
	for {
		var p Package
		if err := dec.Decode(&p); err == io.EOF {
			return pkgs, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		pkgs = append(pkgs, p)
	}
}
