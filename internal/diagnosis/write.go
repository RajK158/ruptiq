package diagnosis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// artifactPath joins a fixed file name onto dir and rejects a name that would
// leave that directory.
func artifactPath(dir, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("artifact %q is not a file name", name)
	}
	cleanDir := filepath.Clean(dir)
	path := filepath.Join(cleanDir, name)
	rel, err := filepath.Rel(cleanDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("artifact %q escapes the output directory", name)
	}
	return path, nil
}

// prepareDir creates dir when needed and refuses to start when any artifact
// is already there.
func prepareDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", &ValidationError{Problems: []string{"output directory is required"}}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case !info.IsDir():
		return "", &ValidationError{Problems: []string{"output path is not a directory"}}
	}
	var present []string
	for _, name := range []string{FileExperiment, FileAnalysis, FileInvestigation, FileManifest} {
		path, err := artifactPath(abs, name)
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(path); err == nil {
			present = append(present, name)
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	if len(present) > 0 {
		return "", &ValidationError{Problems: []string{
			"choose an empty directory or another path; output directory already contains " + strings.Join(present, ", "),
		}}
	}
	return abs, nil
}

// writeJSONFile writes v as indented JSON by renaming a temporary file in the
// same directory. A reader never sees a partial artifact, and an existing
// artifact is left unchanged.
func writeJSONFile(dir, name string, v any) error {
	path, err := artifactPath(dir, name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".ruptiq-diagnose-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}
