package pairworkflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func artifactPath(dir, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("artifact %q is not a file name", name)
	}
	cleanDir := filepath.Clean(dir)
	path := filepath.Join(cleanDir, name)
	rel, err := filepath.Rel(cleanDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("artifact %q escapes the pair directory", name)
	}
	return path, nil
}

func preparePairDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", &ValidationError{Problems: []string{"pair directory is required"}}
	}
	root, err := canonical(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
		root, err = canonical(root)
		if err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case !info.IsDir():
		return "", &ValidationError{Problems: []string{"pair directory is not a directory"}}
	}
	if err := checkLayout(root); err != nil {
		return "", err
	}
	return root, nil
}

// reservePairDir exclusively creates the pair reservation. The returned
// function removes that file. A reservation left by a crash is never removed
// because of its age or its recorded pid.
func reservePairDir(dir string) (func(), error) {
	path, err := artifactPath(dir, reservationFile)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, &ValidationError{Problems: []string{
				"pair directory is reserved by another pair workflow; if none is running, remove " + reservationFile + " manually",
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

// writeJSONFile replaces pair-manifest.json by writing a temporary file in
// the pair directory and renaming it into place. The replacement is not a
// transaction with the stage reports: those are written first, by the
// diagnosis or run-validation packages, and are left in place if this write
// fails. On Windows a rename does not replace an existing file, so the
// previous manifest is removed only after the temporary file is complete.
// A crash in that gap leaves the stage directories as the record.
func writeJSONFile(dir, name string, v any) error {
	if name != FileManifest {
		return fmt.Errorf("pair workflow writes only %s", FileManifest)
	}
	path, err := artifactPath(dir, name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".ruptiq-pair-*.json")
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
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}
