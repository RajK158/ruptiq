// Command package-cli writes development archives for ruptiq-pair,
// ruptiq-diagnose, and ruptiq-validate-runs.
//
//	go run ./scripts
//
// Output goes to dist/, which Git ignores. This does not create a release
// or a tag, and it does not need k6.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/RajK158/ruptiq/internal/clidist"
)

func main() {
	out := flag.String("out", "dist", "directory for archives and checksums, relative to the module root unless absolute")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %s\n", strings.Join(flag.Args(), " "))
		os.Exit(2)
	}
	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dest := *out
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(root, dest)
	}
	if _, err := clidist.Run(context.Background(), root, dest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func moduleRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", fmt.Errorf("go env GOMOD did not report a module file")
	}
	return filepath.Dir(mod), nil
}
