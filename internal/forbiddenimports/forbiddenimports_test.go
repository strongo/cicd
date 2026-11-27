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
	got := Check(pkgs, ParseForbidden(mem+"\n# comment\n"), rules)
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
	if got := ParseForbidden(" a/b/... \n\n# c\nd"); strings.Join(got, ",") != "a/b,d" {
		t.Errorf("ParseForbidden = %v", got)
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
