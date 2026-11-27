package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mem = "github.com/dal-go/dalgo/adapters/dalgo2memory"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func listing(out string, err error) func(string) ([]byte, error) {
	return func(string) ([]byte, error) { return []byte(out), err }
}

func TestRun(t *testing.T) {
	bad := `{"ImportPath":"example.com/app/facade","TestImports":["` + mem + `"]}`
	for _, tc := range []struct {
		name    string
		env     map[string]string
		list    func(string) ([]byte, error)
		code    int
		inOut   string
		inError string
	}{
		{"nothing forbidden", map[string]string{}, listing("", nil), 0, "No forbidden imports configured", ""},
		{"violation", map[string]string{"CI_FORBIDDEN_IMPORTS": mem}, listing(bad, nil), 1, "", "::error title=Forbidden import::example.com/app/facade imports " + mem},
		{"allowed", map[string]string{"CI_FORBIDDEN_IMPORTS": mem, "CI_ALLOWED_IMPORTS": "example.com/app/... " + mem}, listing(bad, nil), 0, "No forbidden imports in 1 packages", ""},
		{"bad rule", map[string]string{"CI_FORBIDDEN_IMPORTS": mem, "CI_ALLOWED_IMPORTS": "x"}, listing(bad, nil), 2, "", "Invalid allowed_imports"},
		{"bad forbidden", map[string]string{"CI_FORBIDDEN_IMPORTS": mem + ", other"}, listing(bad, nil), 2, "", "Invalid forbidden_imports"},
		{"go list fails", map[string]string{"CI_FORBIDDEN_IMPORTS": mem}, listing("", errors.New("boom")), 2, "", "boom"},
		{"bad json", map[string]string{"CI_FORBIDDEN_IMPORTS": mem}, listing("{", nil), 2, "", "decode go list output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(env(tc.env), tc.list, &stdout, &stderr); code != tc.code {
				t.Errorf("exit %d, want %d; stderr: %s", code, tc.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.inOut) || !strings.Contains(stderr.String(), tc.inError) {
				t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}

// The real go list, on a throwaway module: a test import is found, and the
// module directory is honoured.
func TestGoList(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.22\n")
	write("a/a.go", "package a\n")
	write("a/a_test.go", "package a\n\nimport _ \"example.com/m/forbidden\"\n")
	write("forbidden/f.go", "package forbidden\n")
	var stdout, stderr bytes.Buffer
	code := run(env(map[string]string{"CI_FORBIDDEN_IMPORTS": "example.com/m/forbidden", "CI_MODULE_DIR": dir}), goList, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "example.com/m/a imports example.com/m/forbidden") {
		t.Errorf("exit %d, stderr %s", code, stderr.String())
	}
	if _, err := goList(filepath.Join(dir, "missing")); err == nil {
		t.Error("go list in a missing directory succeeded")
	}
}
