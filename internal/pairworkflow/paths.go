package pairworkflow

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func samePath(a, b string) (bool, error) {
	if pathEqual(a, b) {
		return true, nil
	}
	ia, ea := os.Stat(a)
	ib, eb := os.Stat(b)
	return ea == nil && eb == nil && os.SameFile(ia, ib), nil
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
// kept, after its existing parent has been resolved. Windows junctions are
// not os.ModeSymlink, so Readlink is tried before EvalSymlinks.
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

// checkLayout rejects stage paths that escape the pair directory or alias
// each other, including through a symlink or junction.
func checkLayout(root string) error {
	names := []string{DirBefore, DirAfter, DirComparison}
	resolved := make(map[string]string, len(names))
	var problems []string
	for _, name := range names {
		c, err := canonical(filepath.Join(root, name))
		if err != nil {
			problems = append(problems, name+": "+err.Error())
			continue
		}
		resolved[name] = c
		lexical := filepath.Join(root, name)
		if !pathEqual(c, lexical) || !hasPrefixPath(root, c) {
			problems = append(problems, name+" escapes the pair directory through a link or alias")
		}
	}
	pairs := [][2]string{{DirBefore, DirAfter}, {DirBefore, DirComparison}, {DirAfter, DirComparison}}
	for _, p := range pairs {
		a, b := resolved[p[0]], resolved[p[1]]
		if a == "" || b == "" {
			continue
		}
		same, err := samePath(a, b)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if same || hasPrefixPath(a, b) || hasPrefixPath(b, a) {
			problems = append(problems, p[0]+" and "+p[1]+" must be different directories inside the pair directory")
		}
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}
