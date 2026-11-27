package forbiddenimports

import (
	"strings"
	"testing"
)

const mem = "github.com/dal-go/dalgo/adapters/dalgo2memory"

func TestCheck(t *testing.T) {
	pkgs := []Package{
		{ImportPath: "example.com/app/facade", TestImports: []string{mem}},                      // a test uses it
		{ImportPath: "example.com/app/db", Imports: []string{mem + "/internal/x", "fmt"}},       // a sub-package
		{ImportPath: "example.com/app/testing", Imports: []string{mem}},                         // allowed below
		{ImportPath: "example.com/app/testing/more", XTestImports: []string{mem, mem}},          // allowed by /...
		{ImportPath: mem, XTestImports: []string{mem}},                                          // its own tree
		{ImportPath: mem + "/sub", Imports: []string{mem}},                                      // its own tree
		{ImportPath: "example.com/app/clean", Imports: []string{"github.com/dal-go/dalgo/dal"}}, // not forbidden
		{ImportPath: "example.com/app/dalgo2memoryish", Imports: []string{mem + "ish"}},         // only a prefix
	}
	rules, err := ParseRules("# core may\nexample.com/app/testing/... " + mem + "\n\n")
	if err != nil {
		t.Fatal(err)
	}
	forbidden, err := ParseForbidden(mem + "\n# comment\n")
	if err != nil {
		t.Fatal(err)
	}
	got := Check(pkgs, forbidden, rules)
	var lines []string
	for _, v := range got {
		lines = append(lines, v.Package+" -> "+v.Import)
	}
	want := []string{
		"example.com/app/db -> " + mem + "/internal/x",
		"example.com/app/facade -> " + mem,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("violations:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if msg := got[0].String(); !strings.Contains(msg, `"example.com/app/db `+mem+`"`) || !strings.Contains(msg, "allowed_imports") {
		t.Errorf("message does not say how to allow it: %s", msg)
	}
}

func TestRuleForAnExactPackageOnly(t *testing.T) {
	rules := []Rule{{Pattern: "example.com/app/testing", Import: mem}}
	pkgs := []Package{
		{ImportPath: "example.com/app/testing", Imports: []string{mem}},
		{ImportPath: "example.com/app/testing/sub", Imports: []string{mem}},
	}
	got := Check(pkgs, []string{mem}, rules)
	if len(got) != 1 || got[0].Package != "example.com/app/testing/sub" {
		t.Errorf("an exact pattern must not allow sub-packages: %+v", got)
	}
}

func TestParse(t *testing.T) {
	if got, err := ParseForbidden(" a/b/... \n\n# c\nd"); err != nil || strings.Join(got, ",") != "a/b,d" {
		t.Errorf("ParseForbidden = %v, %v", got, err)
	}
	for _, bad := range []string{"a,b", "a b", "a # c", "./a", "/a", "a/", "...", "a/*", "a/.../b", "a...", "a/[x]"} {
		if _, err := ParseForbidden(bad); err == nil {
			t.Errorf("ParseForbidden(%q) accepted it", bad)
		}
	}
	for _, bad := range []string{"p/ x", "p ./x", "p x,y"} {
		if _, err := ParseRules(bad); err == nil {
			t.Errorf("ParseRules(%q) accepted it", bad)
		}
	}
	if _, err := ParseRules("only-one-field"); err == nil {
		t.Error("a rule with one field was accepted")
	}
	rules, err := ParseRules("p/... x/...")
	if err != nil || len(rules) != 1 || rules[0].Import != "x" {
		t.Errorf("ParseRules = %+v, %v", rules, err)
	}
}

func TestDecodePackages(t *testing.T) {
	pkgs, err := DecodePackages(strings.NewReader(`{"ImportPath":"a","Imports":["x"]}
{"ImportPath":"b","TestImports":["y"]}`))
	if err != nil || len(pkgs) != 2 || pkgs[1].TestImports[0] != "y" {
		t.Errorf("DecodePackages = %+v, %v", pkgs, err)
	}
	if _, err := DecodePackages(strings.NewReader(`{"ImportPath":`)); err == nil {
		t.Error("truncated JSON was accepted")
	}
}

// A rule names an import at, above or below the forbidden entry: it allows
// exactly the imports it covers.
func TestRulesCoverTheImportTheyName(t *testing.T) {
	pkgs := []Package{
		{ImportPath: "example.com/p", Imports: []string{"parent/child", "parent/child/deep", "parent/other"}},
	}
	for _, tc := range []struct {
		forbidden, rule string
		want            []string
	}{
		{"parent/child", "example.com/p parent/...", nil},                                           // a broader rule
		{"parent", "example.com/p parent/child", []string{"parent/other"}},                          // a narrower rule
		{"parent", "example.com/p parent/child/...", []string{"parent/other"}},                      // "/..." on the import
		{"parent", "", []string{"parent/child", "parent/child/deep", "parent/other"}},               // no rule
		{"parent\nparent/child", "", []string{"parent/child", "parent/child/deep", "parent/other"}}, // one violation per import
	} {
		forbidden, err := ParseForbidden(tc.forbidden)
		if err != nil {
			t.Fatal(err)
		}
		rules, err := ParseRules(tc.rule)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, v := range Check(pkgs, forbidden, rules) {
			got = append(got, v.Import)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("forbidden %q rule %q: %v, want %v", tc.forbidden, tc.rule, got, tc.want)
		}
	}
}

// A real package named foo_test is its own package, not foo's tests.
func TestPackageNamedLikeATestIsItself(t *testing.T) {
	pkgs := []Package{{ImportPath: "example.com/foo_test", Imports: []string{mem}}}
	rules := []Rule{{Pattern: "example.com/foo", Import: mem}}
	if got := Check(pkgs, []string{mem}, rules); len(got) != 1 || got[0].Package != "example.com/foo_test" {
		t.Errorf("a rule for foo allowed foo_test: %+v", got)
	}
	if got := Check([]Package{{ImportPath: mem + "_test", Imports: []string{mem}}}, []string{mem}, nil); len(got) != 1 {
		t.Errorf("a sibling %s_test was treated as the forbidden import's own tree: %+v", mem, got)
	}
}
