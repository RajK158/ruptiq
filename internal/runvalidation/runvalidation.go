// Package runvalidation compares two saved diagnosis directories. It checks
// that each directory is an authentic ruptiq-diagnose result, then calls
// fixvalidation.Compare. It does not calculate throughput, error ratios,
// histogram quantiles, or rankings, and it does not run a workload.
//
// A finished comparison, including not_comparable and insufficient_evidence,
// means the saved analyses were judged. It is not proof that a code or
// deployment change caused the difference, and this package does not apply
// either change.
package runvalidation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
)

// SchemaVersion is the current Manifest format.
const SchemaVersion = 1

// Artifact file names written inside the output directory.
const (
	FileValidation = "validation.json"
	FileManifest   = "manifest.json"
)

// Config selects two diagnosis directories and the output directory.
// A zero Validation is replaced by fixvalidation.DefaultConfig. Set every
// field of Validation when any of its fields is set.
type Config struct {
	BeforeDir     string
	AfterDir      string
	OutputDir     string
	MaxInputBytes int64
	Validation    fixvalidation.Config
}

// WithDefaults fills a zero input limit and a zero comparison config.
func (c Config) WithDefaults() Config {
	if c.MaxInputBytes == 0 {
		c.MaxInputBytes = analysis.DefaultMaxInputBytes
	}
	if c.Validation == (fixvalidation.Config{}) {
		c.Validation = fixvalidation.DefaultConfig()
	}
	return c
}

// ValidationError is a problem with the requested directories or thresholds.
// The inputs were not accepted and no comparison was written.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid run validation: " + strings.Join(e.Problems, "; ")
}

// InputError means a diagnosis directory could not be read or is not an
// authentic, consistent diagnosis. Nothing is rewritten to repair it.
type InputError struct {
	Problems []string
}

func (e *InputError) Error() string {
	return "invalid diagnosis input: " + strings.Join(e.Problems, "; ")
}

// Validate reports every problem with c, or nil. Call it on c.WithDefaults().
// It does not read the diagnosis artifacts.
func (c Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.BeforeDir) == "" || strings.TrimSpace(c.AfterDir) == "" || strings.TrimSpace(c.OutputDir) == "" {
		problems = append(problems, "-before-dir, -after-dir, and -out-dir are required")
	}
	for _, p := range []struct{ name, path string }{
		{"before directory", c.BeforeDir},
		{"after directory", c.AfterDir},
		{"output directory", c.OutputDir},
	} {
		if strings.ContainsRune(p.path, 0) {
			problems = append(problems, p.name+" contains a NUL byte")
		}
	}
	if c.MaxInputBytes < analysis.MinInputBytes || c.MaxInputBytes > analysis.MaxInputBytesLimit {
		problems = append(problems, fmt.Sprintf("max input bytes must be between %d and %d, got %d",
			analysis.MinInputBytes, analysis.MaxInputBytesLimit, c.MaxInputBytes))
	}
	if err := c.Validation.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 {
		if same, err := samePath(c.BeforeDir, c.AfterDir); err != nil {
			problems = append(problems, err.Error())
		} else if same {
			problems = append(problems, "before and after directories must be different")
		}
		for _, dir := range []string{c.BeforeDir, c.AfterDir} {
			same, err := samePath(c.OutputDir, dir)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			nested, err := overlaps(c.OutputDir, dir)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			if same || nested {
				problems = append(problems, "output directory must not be either input directory or be inside it")
			}
		}
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func samePath(a, b string) (bool, error) {
	aa, err := canonical(a)
	if err != nil {
		return false, err
	}
	bb, err := canonical(b)
	if err != nil {
		return false, err
	}
	if pathEqual(aa, bb) {
		return true, nil
	}
	ia, ea := os.Stat(aa)
	ib, eb := os.Stat(bb)
	return ea == nil && eb == nil && os.SameFile(ia, ib), nil
}

// overlaps reports whether one directory is inside the other after aliases
// are resolved. The output must not contain an input, and an input must not
// contain the output.
func overlaps(a, b string) (bool, error) {
	aa, err := canonical(a)
	if err != nil {
		return false, err
	}
	bb, err := canonical(b)
	if err != nil {
		return false, err
	}
	return hasPrefixPath(aa, bb) || hasPrefixPath(bb, aa), nil
}

func hasPrefixPath(parent, child string) bool {
	parent = strings.TrimRight(foldPath(parent), `/\`)
	child = foldPath(child)
	if parent == "" || child == parent {
		return false
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator)) ||
		strings.HasPrefix(child, parent+"/")
}

// canonical returns the absolute path with symlinks, junctions, and other
// existing aliases resolved. A final component that does not exist yet is
// kept, after its existing parent has been resolved.
func canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return canonicalAbs(filepath.Clean(abs), map[string]bool{})
}

func canonicalAbs(abs string, seen map[string]bool) (string, error) {
	key := foldPath(abs)
	if seen[key] {
		return "", fmt.Errorf("path alias cycle")
	}
	seen[key] = true
	var pending []string
	cur := abs
	for {
		if _, err := os.Lstat(cur); err == nil {
			if target, rerr := os.Readlink(cur); rerr == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(cur), target)
				}
				target, err = filepath.Abs(target)
				if err != nil {
					return "", err
				}
				resolved, err := canonicalAbs(filepath.Clean(target), seen)
				if err != nil {
					return "", err
				}
				for i := len(pending) - 1; i >= 0; i-- {
					resolved = filepath.Join(resolved, pending[i])
				}
				return filepath.Clean(resolved), nil
			}
			final, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			final, err = filepath.Abs(final)
			if err != nil {
				return "", err
			}
			final = filepath.Clean(final)
			for i := len(pending) - 1; i >= 0; i-- {
				final = filepath.Join(final, pending[i])
			}
			return filepath.Clean(final), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		pending = append(pending, filepath.Base(cur))
		cur = parent
	}
}

func pathEqual(a, b string) bool {
	if a == b {
		return true
	}
	return runtime.GOOS == "windows" && strings.EqualFold(a, b)
}

func foldPath(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

func absClean(p string) (string, error) {
	return canonical(p)
}

// Result is one finished comparison.
type Result struct {
	Dir      string
	Manifest Manifest
	Report   fixvalidation.Report
}

// Manifest is the versioned record of one comparison of two diagnosis
// directories. Labels are the words before and after, not machine paths.
type Manifest struct {
	SchemaVersion int        `json:"schema_version"`
	Versions      Versions   `json:"versions"`
	Outcome       string     `json:"outcome"`
	Comparable    bool       `json:"comparable"`
	Before        Input      `json:"before"`
	After         Input      `json:"after"`
	Artifacts     []Artifact `json:"artifacts"`
	Warnings      []string   `json:"warnings"`
	Limitations   []string   `json:"limitations"`
}

// Versions records the schemas used to read the inputs and write the outputs.
type Versions struct {
	RunValidation int `json:"run_validation_schema"`
	FixValidation int `json:"fix_validation_schema"`
	Diagnosis     int `json:"diagnosis_schema"`
	Analysis      int `json:"analysis_schema"`
}

// Input identifies one saved diagnosis by a stable label and the hash of
// the analysis file that was compared.
type Input struct {
	Label                 string `json:"label"`
	DiagnosisOutcome      string `json:"diagnosis_outcome"`
	ExperimentStatus      string `json:"experiment_status"`
	TelemetryCompleteness string `json:"telemetry_completeness"`
	AnalysisFile          string `json:"analysis_file"`
	AnalysisSHA256        string `json:"analysis_sha256"`
	InvestigationOutcome  string `json:"investigation_outcome,omitempty"`
}

// Artifact is one file this command wrote.
type Artifact struct {
	Name          string `json:"name"`
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
}

func currentVersions() Versions {
	return Versions{
		RunValidation: SchemaVersion,
		FixValidation: fixvalidation.SchemaVersion,
		Diagnosis:     diagnosis.SchemaVersion,
		Analysis:      analysis.SchemaVersion,
	}
}

func baseLimitations(warnings []string) []string {
	out := []string{
		"This command compared two saved diagnosis directories. It did not run a workload, start k6, contact an HTTP service, or change a deployment.",
		"The user, not Ruptiq, decides when a code or deployment change happens. This command does not apply a fix.",
		"A completed comparison is not proof that a software change caused the difference, and it is not a statistical significance test.",
		"not_comparable and insufficient_evidence are completed comparisons. They are not command failures.",
		"Evidence pointers are JSON pointers into the side's original analysis.json (the side is before or after). They are not pointers into validation.json, and k6 client metrics are not used as server measurements.",
		"The before and after labels in this manifest are names for the two inputs. They are not filesystem paths.",
	}
	if len(warnings) > 0 {
		out = append(out, "Comparability warnings: "+strings.Join(warnings, "; "))
	}
	return out
}

func wrapInput(label string, err error) error {
	if err == nil {
		return nil
	}
	var ierr *InputError
	if errors.As(err, &ierr) {
		problems := make([]string, len(ierr.Problems))
		for i, p := range ierr.Problems {
			problems[i] = label + ": " + p
		}
		return &InputError{Problems: problems}
	}
	return fmt.Errorf("%s: %w", label, err)
}
