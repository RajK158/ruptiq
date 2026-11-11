package clidist

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Entry is one file stored at the root of a distribution archive.
type Entry struct {
	Name string
	Body []byte
	Mode int64
}

// CreateArchive writes a .zip or .tar.gz containing exactly entries.
// Names must be base names. A repeated name is an error, and a failed
// write removes the partial archive.
func CreateArchive(path string, entries []Entry) (err error) {
	if len(entries) == 0 {
		return fmt.Errorf("archive %s has no entries", path)
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if err := validEntryName(entry.Name); err != nil {
			return err
		}
		if _, ok := seen[entry.Name]; ok {
			return fmt.Errorf("duplicate archive entry %s", entry.Name)
		}
		seen[entry.Name] = struct{}{}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := f.Close()
		if err != nil {
			_ = os.Remove(path)
			return
		}
		err = closeErr
	}()

	switch {
	case strings.HasSuffix(path, ".zip"):
		err = writeZip(f, entries)
	case strings.HasSuffix(path, ".tar.gz"):
		err = writeTarGZ(f, entries)
	default:
		err = fmt.Errorf("unsupported archive %s", path)
	}
	return err
}

func writeZip(w io.Writer, entries []Entry) error {
	zw := zip.NewWriter(w)
	for _, entry := range entries {
		h := &zip.FileHeader{Name: entry.Name, Method: zip.Deflate}
		h.SetMode(os.FileMode(entry.Mode))
		h.Modified = time.Unix(0, 0).UTC()
		fw, err := zw.CreateHeader(h)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := fw.Write(entry.Body); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

func writeTarGZ(w io.Writer, entries []Entry) error {
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	for _, entry := range entries {
		hdr := &tar.Header{
			Name:    entry.Name,
			Mode:    entry.Mode,
			Size:    int64(len(entry.Body)),
			ModTime: time.Unix(0, 0).UTC(),
			Format:  tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = gw.Close()
			return err
		}
		if _, err := tw.Write(entry.Body); err != nil {
			_ = tw.Close()
			_ = gw.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return err
	}
	return gw.Close()
}

// ListArchive opens path and returns its file names.
// A duplicate name, a directory, or a nested path is an error.
func ListArchive(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var names []string
	switch {
	case strings.HasSuffix(path, ".zip"):
		names, err = listZip(f)
	case strings.HasSuffix(path, ".tar.gz"):
		names, err = listTarGZ(f)
	default:
		return nil, fmt.Errorf("unsupported archive %s", path)
	}
	if err != nil {
		return nil, err
	}
	return names, nil
}

func listZip(r io.ReaderAt) ([]string, error) {
	// zip.NewReader needs the size. Callers pass *os.File.
	f, ok := r.(*os.File)
	if !ok {
		return nil, fmt.Errorf("zip reader is not a file")
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(zr.File))
	names := make([]string, 0, len(zr.File))
	for _, file := range zr.File {
		if file.FileInfo().IsDir() || strings.HasSuffix(file.Name, "/") {
			return nil, fmt.Errorf("archive contains directory %s", file.Name)
		}
		if err := validEntryName(file.Name); err != nil {
			return nil, err
		}
		if _, ok := seen[file.Name]; ok {
			return nil, fmt.Errorf("duplicate archive entry %s", file.Name)
		}
		seen[file.Name] = struct{}{}
		names = append(names, file.Name)
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			_ = rc.Close()
			return nil, err
		}
		if err := rc.Close(); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func listTarGZ(r io.Reader) ([]string, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	seen := make(map[string]struct{})
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("archive entry %s is not a regular file", hdr.Name)
		}
		if err := validEntryName(hdr.Name); err != nil {
			return nil, err
		}
		if _, ok := seen[hdr.Name]; ok {
			return nil, fmt.Errorf("duplicate archive entry %s", hdr.Name)
		}
		seen[hdr.Name] = struct{}{}
		names = append(names, hdr.Name)
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// ValidateArchive checks that path opens and contains exactly want, with no extras.
func ValidateArchive(path string, want []string) error {
	got, err := ListArchive(path)
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("%s has %d entries, want %d (%s)", path, len(got), len(want), strings.Join(got, ", "))
	}
	have := make(map[string]struct{}, len(got))
	for _, name := range got {
		have[name] = struct{}{}
	}
	for _, name := range want {
		if _, ok := have[name]; !ok {
			return fmt.Errorf("%s is missing %s", path, name)
		}
	}
	return nil
}

func validEntryName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") || strings.Contains(name, `\`) || strings.Contains(name, ":") {
		return fmt.Errorf("refusing archive entry %q", name)
	}
	return nil
}

// ReadArchiveFile returns the bytes of one root entry.
func ReadArchiveFile(path, name string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if strings.HasSuffix(path, ".zip") {
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(f, info.Size())
		if err != nil {
			return nil, err
		}
		for _, file := range zr.File {
			if file.Name != name {
				continue
			}
			rc, err := file.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
		return nil, fmt.Errorf("%s is missing %s", path, name)
	}
	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != name {
			continue
		}
		return io.ReadAll(tr)
	}
	return nil, fmt.Errorf("%s is missing %s", path, name)
}

// ArchiveMagic reports the leading bytes so tests can tell zip from gzip.
func ArchiveMagic(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, 4)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}
