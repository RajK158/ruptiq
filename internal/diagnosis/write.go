package diagnosis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// reservationFile is created with O_EXCL to own an output directory. It is not
// a report. A reservation left behind by a crashed process is never removed
// automatically, including not because of its age.
const reservationFile = ".ruptiq-diagnose-reservation"

// ensureOutputDir creates dir when needed and returns its absolute path.
func ensureOutputDir(dir string) (string, error) {
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
	return abs, nil
}

// reserveOutputDir exclusively creates the reservation file. The returned
// function removes that file. A second caller fails while the file exists.
func reserveOutputDir(dir string) (func(), error) {
	path, err := artifactPath(dir, reservationFile)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, &ValidationError{Problems: []string{
				"output directory is reserved by another diagnosis; if none is running, remove " + reservationFile + " manually",
			}}
		}
		return nil, err
	}
	_, werr := fmt.Fprintf(f, "pid %d\n", os.Getpid())
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(path)
		if werr != nil {
			return nil, werr
		}
		return nil, cerr
	}
	var once sync.Once
	return func() { once.Do(func() { os.Remove(path) }) }, nil
}

// rejectExistingArtifacts reports report files that are already in dir.
func rejectExistingArtifacts(dir string) error {
	var present []string
	for _, name := range []string{FileExperiment, FileAnalysis, FileInvestigation, FileManifest} {
		path, err := artifactPath(dir, name)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(path); err == nil {
			present = append(present, name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if len(present) == 0 {
		return nil
	}
	return &ValidationError{Problems: []string{
		"choose an empty directory or another path; output directory already contains " + strings.Join(present, ", "),
	}}
}

// writeJSONFile writes v as indented JSON by renaming a temporary file in the
// same directory. A reader never sees a partial artifact, and an existing
// artifact is left unchanged. Concurrent diagnosis processes are kept out by
// reserveOutputDir; this check still refuses to replace a file that is
// already present.
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
