// Command forbidden-imports fails when a package of the module in
// CI_MODULE_DIR imports, directly, an import listed in CI_FORBIDDEN_IMPORTS
// that CI_ALLOWED_IMPORTS does not allow for it (see package
// forbiddenimports). It is the go_lint job's "Check forbidden imports" step.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/strongo/go-ci-action/internal/forbiddenimports"
)

var workflowCommandEscaper = strings.NewReplacer(
	"%", "%25",
	"\r", "%0D",
	"\n", "%0A",
)

func emitError(w io.Writer, title, message string) {
	_, _ = fmt.Fprintf(w, "::error title=%s::%s\n", title, workflowCommandEscaper.Replace(message))
}

func main() {
	os.Exit(run(os.Getenv, goList, os.Stdout, os.Stderr))
}

// goList runs `go list -json ./...` (test imports included) in dir.
func goList(dir string) ([]byte, error) {
	cmd := exec.Command("go", "list", "-json=ImportPath,Imports,TestImports,XTestImports", "./...")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./... in %s: %w: %s", dir, err, stderr.String())
	}
	return out, nil
}

func run(getenv func(string) string, list func(string) ([]byte, error), stdout, stderr io.Writer) int {
	forbidden, err := forbiddenimports.ParseForbidden(getenv("CI_FORBIDDEN_IMPORTS"))
	if err != nil {
		emitError(stderr, "Invalid forbidden_imports", err.Error())
		return 2
	}
	if len(forbidden) == 0 {
		_, _ = fmt.Fprintln(stdout, "No forbidden imports configured.")
		return 0
	}
	rules, err := forbiddenimports.ParseRules(getenv("CI_ALLOWED_IMPORTS"))
	if err != nil {
		emitError(stderr, "Invalid allowed_imports", err.Error())
		return 2
	}
	dir := getenv("CI_MODULE_DIR")
	if dir == "" {
		dir = "."
	}
	out, err := list(dir)
	if err != nil {
		emitError(stderr, "Forbidden imports check failed", err.Error())
		return 2
	}
	pkgs, err := forbiddenimports.DecodePackages(bytes.NewReader(out))
	if err != nil {
		emitError(stderr, "Forbidden imports check failed", err.Error())
		return 2
	}
	violations := forbiddenimports.Check(pkgs, forbidden, rules)
	for _, v := range violations {
		emitError(stderr, "Forbidden import", v.String())
	}
	if len(violations) > 0 {
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "No forbidden imports in %d packages.\n", len(pkgs))
	return 0
}
