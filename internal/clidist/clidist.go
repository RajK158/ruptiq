// Package clidist builds portable archives of the Ruptiq workflow commands.
// The archives are development builds. They are not a release and they are
// not signed.
package clidist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// ChecksumFile is written next to the archives. Its lines are the SHA-256
// of each archive, in the form accepted by sha256sum -c.
const ChecksumFile = "ruptiq-cli-SHA256SUMS.txt"

// ReadmeName is the short usage note stored in every archive.
const ReadmeName = "README.md"

// Command is one program copied into every archive.
type Command struct {
	Name       string
	ImportPath string
	Usage      string
}

// Commands are the workflow CLIs. Other Ruptiq commands stay in the module
// and are not removed from the repository.
var Commands = []Command{
	{Name: "ruptiq-pair", ImportPath: "./cmd/ruptiq-pair", Usage: "Usage: ruptiq-pair"},
	{Name: "ruptiq-diagnose", ImportPath: "./cmd/ruptiq-diagnose", Usage: "Usage: ruptiq-diagnose"},
	{Name: "ruptiq-validate-runs", ImportPath: "./cmd/ruptiq-validate-runs", Usage: "Usage: ruptiq-validate-runs"},
}

// Platform is one GOOS/GOARCH pair.
type Platform struct {
	GOOS   string
	GOARCH string
}

// Platforms are the development archives. The order is the checksum order.
func Platforms() []Platform {
	return []Platform{
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "windows", GOARCH: "amd64"},
	}
}

// ArchiveName is the development filename for p.
func (p Platform) ArchiveName() string {
	ext := ".tar.gz"
	if p.GOOS == "windows" {
		ext = ".zip"
	}
	return "ruptiq-cli-" + p.GOOS + "-" + p.GOARCH + ext
}

// Executable is the file name of command on p.
func (p Platform) Executable(command string) string {
	if p.GOOS == "windows" {
		return command + ".exe"
	}
	return command
}

// Contents are the root entries of p's archive, in archive order.
func (p Platform) Contents() []string {
	names := []string{ReadmeName}
	for _, command := range Commands {
		names = append(names, p.Executable(command.Name))
	}
	return names
}

// Result is one archive that passed format, content, and checksum checks.
type Result struct {
	Platform Platform
	Archive  string
	Size     int64
	SHA256   string
	HostTest string
}

// Run builds every platform, writes archives under outDir, and checks them.
// outDir is usually the repository's gitignored dist directory.
func Run(ctx context.Context, moduleRoot, outDir string) ([]Result, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := removeOwnedOutputs(outDir); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp("", "ruptiq-cli-stage-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	var results []Result
	var archives []string
	for _, platform := range Platforms() {
		dir := filepath.Join(staging, platform.GOOS+"-"+platform.GOARCH)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		readme := []byte(readmeText(platform))
		if err := os.WriteFile(filepath.Join(dir, ReadmeName), readme, 0o644); err != nil {
			return nil, err
		}
		for _, command := range Commands {
			dest := filepath.Join(dir, platform.Executable(command.Name))
			if err := build(ctx, moduleRoot, dest, command.ImportPath, platform); err != nil {
				return nil, err
			}
			format, arch, err := Identify(dest)
			if err != nil {
				return nil, err
			}
			wantFormat, wantArch, err := expectFormat(platform.GOOS, platform.GOARCH)
			if err != nil {
				return nil, err
			}
			if format != wantFormat || arch != wantArch {
				return nil, fmt.Errorf("%s is %s %s, want %s %s", dest, format, arch, wantFormat, wantArch)
			}
			fmt.Printf("%s/%s %s: %s %s\n", platform.GOOS, platform.GOARCH, command.Name, format, arch)
		}
		// An unrelated file in the staging directory must not be packaged.
		// This marker is the regression for that rule.
		if err := os.WriteFile(filepath.Join(dir, "unrelated-not-packaged.txt"), []byte("no"), 0o644); err != nil {
			return nil, err
		}
		archivePath := filepath.Join(outDir, platform.ArchiveName())
		if err := PackDirectory(dir, archivePath, platform); err != nil {
			return nil, err
		}
		if err := ValidateArchive(archivePath, platform.Contents()); err != nil {
			return nil, err
		}
		host := ""
		if platform.GOOS == runtime.GOOS && platform.GOARCH == runtime.GOARCH {
			for _, command := range Commands {
				path := filepath.Join(dir, platform.Executable(command.Name))
				if err := SmokeHelp(path, command.Usage); err != nil {
					return nil, err
				}
			}
			host = fmt.Sprintf("%s/%s help exited 2", runtime.GOOS, runtime.GOARCH)
			fmt.Printf("%s\n", host)
		}
		sum, size, err := fileSHA256(archivePath)
		if err != nil {
			return nil, err
		}
		results = append(results, Result{
			Platform: platform,
			Archive:  platform.ArchiveName(),
			Size:     size,
			SHA256:   sum,
			HostTest: host,
		})
		archives = append(archives, platform.ArchiveName())
		fmt.Printf("%s %d bytes %s\n", platform.ArchiveName(), size, sum)
	}
	sumPath := filepath.Join(outDir, ChecksumFile)
	if err := WriteChecksums(outDir, archives, sumPath); err != nil {
		return nil, err
	}
	if err := VerifyChecksums(sumPath); err != nil {
		return nil, err
	}
	fmt.Printf("verified %s\n", ChecksumFile)
	return results, nil
}

func removeOwnedOutputs(outDir string) error {
	for _, platform := range Platforms() {
		if err := os.Remove(filepath.Join(outDir, platform.ArchiveName())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(outDir, ChecksumFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func build(ctx context.Context, moduleRoot, dest, importPath string, platform Platform) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", dest, importPath)
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+platform.GOOS, "GOARCH="+platform.GOARCH)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s for %s/%s: %w\n%s", importPath, platform.GOOS, platform.GOARCH, err, output.String())
	}
	info, err := os.Stat(dest)
	if err != nil {
		return fmt.Errorf("build %s for %s/%s did not write %s: %w", importPath, platform.GOOS, platform.GOARCH, dest, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("build %s for %s/%s wrote an empty file", importPath, platform.GOOS, platform.GOARCH)
	}
	return nil
}

// SmokeHelp runs path -h. Help is invalid arguments and exits 2.
// It does not start a workload.
func SmokeHelp(path, usage string) error {
	cmd := exec.Command(path, "-h")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return fmt.Errorf("%s -h: %w", path, err)
		}
		code = exit.ExitCode()
	}
	if code != 2 {
		return fmt.Errorf("%s -h exited %d, want 2\n%s", path, code, out)
	}
	if !bytes.Contains(out, []byte(usage)) {
		return fmt.Errorf("%s -h did not print %q\n%s", path, usage, out)
	}
	return nil
}

// PackDirectory archives only the expected names from dir.
func PackDirectory(dir, dest string, platform Platform) error {
	var entries []Entry
	for _, name := range platform.Contents() {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("missing %s for %s: %w", name, platform.ArchiveName(), err)
		}
		mode := int64(0o644)
		if name != ReadmeName {
			mode = 0o755
		}
		entries = append(entries, Entry{Name: name, Body: body, Mode: mode})
	}
	if err := CreateArchive(dest, entries); err != nil {
		return err
	}
	return ValidateArchive(dest, platform.Contents())
}

// WriteChecksums writes SHA-256 lines for names in dir.
func WriteChecksums(dir string, names []string, dest string) error {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var b strings.Builder
	seen := make(map[string]struct{}, len(sorted))
	for _, name := range sorted {
		if err := validEntryName(name); err != nil {
			return err
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("duplicate checksum entry %s", name)
		}
		seen[name] = struct{}{}
		sum, _, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}

// VerifyChecksums hashes every file named by dest and compares the digests.
func VerifyChecksums(dest string) error {
	text, err := os.ReadFile(dest)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dest)
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return fmt.Errorf("%s is empty", dest)
	}
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		hash, name, ok := strings.Cut(line, "  ")
		if !ok || len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
			return fmt.Errorf("malformed checksum line %q", line)
		}
		if err := validEntryName(name); err != nil {
			return err
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("duplicate checksum entry %s", name)
		}
		seen[name] = struct{}{}
		sum, _, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("checksum file %s: %w", name, err)
		}
		if sum != hash {
			return fmt.Errorf("%s sha256 %s, checksum file says %s", name, sum, hash)
		}
	}
	return nil
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if _, err := f.WriteTo(h); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), info.Size(), nil
}

func readmeText(p Platform) string {
	exe := ""
	if p.GOOS == "windows" {
		exe = ".exe"
	}
	return fmt.Sprintf(`# Ruptiq CLI (%s/%s)

This is an unsigned development build, not a versioned or signed release.
The repository has no license file, so this archive does not add one.

The executables are ruptiq-pair%s, ruptiq-diagnose%s, and ruptiq-validate-runs%s.
Go is not required to run them. k6 is not included.

ruptiq-pair -stage before and -stage after, and ruptiq-diagnose, start a live workload and need a separate k6 executable (-k6, or k6 on PATH).
ruptiq-pair -stage compare and ruptiq-validate-runs only read saved diagnosis directories. They do not need k6.

A first run prints usage and exits 2. Help is reported as invalid arguments:

    ruptiq-pair%s -h

Exit status: 0 the command finished, including a comparison that is not comparable or has insufficient evidence; 2 invalid arguments, help, a configuration mismatch, or a refusal to overwrite; 3 a stage or a saved file failed.

    ruptiq-pair%s -stage compare -pair-dir DIR

DIR must already contain the before and after diagnoses. Ruptiq does not apply the change between those stages.
`, p.GOOS, p.GOARCH, exe, exe, exe, exe, exe)
}
