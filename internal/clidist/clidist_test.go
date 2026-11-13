package clidist

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveNames(t *testing.T) {
	want := []string{
		"ruptiq-cli-linux-amd64.tar.gz",
		"ruptiq-cli-linux-arm64.tar.gz",
		"ruptiq-cli-darwin-amd64.tar.gz",
		"ruptiq-cli-darwin-arm64.tar.gz",
		"ruptiq-cli-windows-amd64.zip",
	}
	got := make([]string, 0, len(want))
	for _, platform := range Platforms() {
		got = append(got, platform.ArchiveName())
		if platform.GOOS == "windows" && filepath.Ext(platform.ArchiveName()) != ".zip" {
			t.Fatalf("%s is not a zip", platform.ArchiveName())
		}
		if platform.GOOS != "windows" && !bytes.HasSuffix([]byte(platform.ArchiveName()), []byte(".tar.gz")) {
			t.Fatalf("%s is not a tar.gz", platform.ArchiveName())
		}
	}
	if stringsJoin(got) != stringsJoin(want) {
		t.Fatalf("archives %v, want %v", got, want)
	}
}

func TestExecutableNames(t *testing.T) {
	linux := Platform{GOOS: "linux", GOARCH: "amd64"}.Contents()
	darwin := Platform{GOOS: "darwin", GOARCH: "arm64"}.Contents()
	windows := Platform{GOOS: "windows", GOARCH: "amd64"}.Contents()
	if stringsJoin(linux) != "README.md LICENSE THIRD_PARTY_NOTICES.txt ruptiq-pair ruptiq-diagnose ruptiq-validate-runs" {
		t.Fatalf("linux contents %v", linux)
	}
	if stringsJoin(darwin) != stringsJoin(linux) {
		t.Fatalf("darwin contents %v", darwin)
	}
	if stringsJoin(windows) != "README.md LICENSE THIRD_PARTY_NOTICES.txt ruptiq-pair.exe ruptiq-diagnose.exe ruptiq-validate-runs.exe" {
		t.Fatalf("windows contents %v", windows)
	}
}

func TestPackOmitsUnrelatedAndRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	platform := Platform{GOOS: "linux", GOARCH: "amd64"}
	for _, name := range platform.Contents() {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), platform.ArchiveName())
	if err := PackDirectory(dir, dest, platform); err != nil {
		t.Fatal(err)
	}
	names, err := ListArchive(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name == "secret.txt" {
			t.Fatal("unrelated file was packaged")
		}
	}
	if err := ValidateArchive(dest, platform.Contents()); err != nil {
		t.Fatal(err)
	}
	magic, err := ArchiveMagic(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(magic) < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		t.Fatalf("tar.gz magic %x", magic)
	}
	modes := archiveModes(t, dest)
	if modes["README.md"] != 0o644 || modes["LICENSE"] != 0o644 || modes["THIRD_PARTY_NOTICES.txt"] != 0o644 || modes["ruptiq-pair"] != 0o755 || modes["ruptiq-diagnose"] != 0o755 || modes["ruptiq-validate-runs"] != 0o755 {
		t.Fatalf("tar modes %v", modes)
	}

	zipPlatform := Platform{GOOS: "windows", GOARCH: "amd64"}
	zipDir := t.TempDir()
	for _, name := range zipPlatform.Contents() {
		if err := os.WriteFile(filepath.Join(zipDir, name), []byte("zip"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	zipDest := filepath.Join(t.TempDir(), zipPlatform.ArchiveName())
	if err := PackDirectory(zipDir, zipDest, zipPlatform); err != nil {
		t.Fatal(err)
	}
	magic, err = ArchiveMagic(zipDest)
	if err != nil {
		t.Fatal(err)
	}
	if string(magic) != "PK\x03\x04" {
		t.Fatalf("zip magic %x", magic)
	}
	body, err := ReadArchiveFile(zipDest, "ruptiq-pair.exe")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "zip" {
		t.Fatalf("zip entry %q", body)
	}
	zipModes := zipFileModes(t, zipDest)
	if zipModes["LICENSE"] != 0o644 || zipModes["THIRD_PARTY_NOTICES.txt"] != 0o644 || zipModes["ruptiq-pair.exe"] != 0o755 {
		t.Fatalf("zip modes %v", zipModes)
	}

	if err := os.Remove(filepath.Join(dir, "LICENSE")); err != nil {
		t.Fatal(err)
	}
	missingLicense := filepath.Join(t.TempDir(), "missing-license.tar.gz")
	if err := PackDirectory(dir, missingLicense, platform); err == nil {
		t.Fatal("missing LICENSE was packaged")
	}
	if _, err := os.Stat(missingLicense); !os.IsNotExist(err) {
		t.Fatalf("partial archive exists: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("LICENSE"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(dir, NoticesName)); err != nil {
		t.Fatal(err)
	}
	missingNotices := filepath.Join(t.TempDir(), "missing-notices.tar.gz")
	if err := PackDirectory(dir, missingNotices, platform); err == nil {
		t.Fatal("missing notices file was packaged")
	}
	if _, err := os.Stat(missingNotices); !os.IsNotExist(err) {
		t.Fatalf("partial archive exists: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, NoticesName), []byte(NoticesName), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(dir, "ruptiq-pair")); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.tar.gz")
	if err := PackDirectory(dir, missing, platform); err == nil {
		t.Fatal("missing executable was packaged")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("partial archive exists: %v", err)
	}
}

func TestDuplicateAndExtraEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.zip")
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	if _, err := zw.Create("ruptiq-pair"); err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Create("ruptiq-pair"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListArchive(path); err == nil {
		t.Fatal("duplicate zip entry was accepted")
	}

	extra := filepath.Join(t.TempDir(), "extra.tar.gz")
	if err := CreateArchive(extra, []Entry{
		{Name: "README.md", Body: []byte("readme"), Mode: 0o644},
		{Name: "notes.txt", Body: []byte("extra"), Mode: 0o644},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateArchive(extra, []string{"README.md"}); err == nil {
		t.Fatal("extra archive entry was accepted")
	}
	if err := CreateArchive(filepath.Join(t.TempDir(), "bad.tar.gz"), []Entry{
		{Name: "../secret", Body: []byte("x"), Mode: 0o644},
	}); err == nil {
		t.Fatal("path entry was accepted")
	}
}

func TestChecksumRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.tar.gz"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.zip"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := filepath.Join(dir, ChecksumFile)
	if err := WriteChecksums(dir, []string{"b.zip", "a.tar.gz"}, sum); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(sum); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(sum)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(text, []byte("  a.tar.gz\n")) || !bytes.Contains(text, []byte("  b.zip\n")) {
		t.Fatalf("checksum file %q", text)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.tar.gz"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(sum); err == nil {
		t.Fatal("changed archive still matched its checksum")
	}
	if err := os.Remove(filepath.Join(dir, "b.zip")); err != nil {
		t.Fatal(err)
	}
	if err := WriteChecksums(dir, []string{"missing.tar.gz"}, sum); err == nil {
		t.Fatal("checksum was written for a missing archive")
	}
}

func TestIdentifyHeaders(t *testing.T) {
	cases := []struct {
		name   string
		body   []byte
		format Format
		arch   string
	}{
		{name: "elf-amd64", body: elfHeader(62), format: FormatELF, arch: "amd64"},
		{name: "elf-arm64", body: elfHeader(183), format: FormatELF, arch: "arm64"},
		{name: "macho-amd64", body: machoHeader(0x01000007), format: FormatMachO, arch: "amd64"},
		{name: "macho-arm64", body: machoHeader(0x0100000c), format: FormatMachO, arch: "arm64"},
		{name: "pe-amd64", body: peHeader(0x8664), format: FormatPE, arch: "amd64"},
		{name: "pe-arm64", body: peHeader(0xAA64), format: FormatPE, arch: "arm64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, tc.body, 0o644); err != nil {
				t.Fatal(err)
			}
			format, arch, err := Identify(path)
			if err != nil {
				t.Fatal(err)
			}
			if format != tc.format || arch != tc.arch {
				t.Fatalf("got %s %s, want %s %s", format, arch, tc.format, tc.arch)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "text")
	if err := os.WriteFile(path, []byte("not a binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Identify(path); err == nil {
		t.Fatal("text was accepted as an executable")
	}
}

func TestProjectLicenseBytes(t *testing.T) {
	license := []byte("Copyright (c) 2026 RajK158\nMIT License\n")
	notice := []byte("third-party-notice-bytes\n")
	for _, platform := range []Platform{
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "windows", GOARCH: "amd64"},
	} {
		dir := t.TempDir()
		for _, name := range platform.Contents() {
			body := []byte(name)
			mode := os.FileMode(0o755)
			if name == LicenseName {
				body = license
				mode = 0o644
			}
			if name == NoticesName {
				body = notice
				mode = 0o644
			}
			if name == ReadmeName {
				mode = 0o644
			}
			if err := os.WriteFile(filepath.Join(dir, name), body, mode); err != nil {
				t.Fatal(err)
			}
		}
		dest := filepath.Join(t.TempDir(), platform.ArchiveName())
		if err := PackDirectory(dir, dest, platform); err != nil {
			t.Fatal(err)
		}
		got, err := ReadArchiveFile(dest, LicenseName)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, license) {
			t.Fatalf("%s LICENSE bytes differ", platform.ArchiveName())
		}
		got, err = ReadArchiveFile(dest, NoticesName)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, notice) {
			t.Fatalf("%s notice bytes differ", platform.ArchiveName())
		}
	}
}

func TestReadProjectLicense(t *testing.T) {
	root := t.TempDir()
	if _, err := readProjectLicense(root); err == nil {
		t.Fatal("missing LICENSE was accepted")
	}
	if err := os.Mkdir(filepath.Join(root, LicenseName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectLicense(root); err == nil {
		t.Fatal("unreadable LICENSE was accepted")
	}
	if err := os.Remove(filepath.Join(root, LicenseName)); err != nil {
		t.Fatal(err)
	}
	want := []byte("Copyright (c) 2026 RajK158\n")
	if err := os.WriteFile(filepath.Join(root, LicenseName), want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readProjectLicense(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("license bytes %q", got)
	}
	text := readmeText(Platform{GOOS: "linux", GOARCH: "amd64"})
	if bytes.Contains([]byte(text), []byte("no license")) {
		t.Fatal("archive README still says the repository has no license")
	}
	if !bytes.Contains([]byte(text), []byte("MIT License")) || !bytes.Contains([]byte(text), []byte("LICENSE")) {
		t.Fatal("archive README does not name the included MIT license")
	}
	if bytes.Contains([]byte(text), []byte("third-party dependencies are MIT")) {
		t.Fatal("archive README claims third-party dependencies are MIT")
	}
	if !bytes.Contains([]byte(text), []byte("THIRD_PARTY_NOTICES.txt")) || !bytes.Contains([]byte(text), []byte("not bundled")) {
		t.Fatal("archive README does not describe the notice bundle and external k6")
	}
}

func TestReadProjectNotices(t *testing.T) {
	root := t.TempDir()
	if _, err := readProjectNotices(root); err == nil {
		t.Fatal("missing notices file was accepted")
	}
	if err := os.Mkdir(filepath.Join(root, NoticesName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectNotices(root); err == nil {
		t.Fatal("unreadable notices file was accepted")
	}
	if err := os.Remove(filepath.Join(root, NoticesName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, NoticesName), []byte(" \n\t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectNotices(root); err == nil {
		t.Fatal("empty notices file was accepted")
	}
	want := []byte("third-party-notice-bytes\n")
	if err := os.WriteFile(filepath.Join(root, NoticesName), want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readProjectNotices(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("notice bytes %q", got)
	}
}

func TestTrackedNoticesCoverPackagedModules(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), NoticesName))
	if err != nil {
		t.Fatal(err)
	}
	required := []string{
		"github.com/prometheus/client_model v0.6.3",
		"github.com/prometheus/common v0.73.1",
		"github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822",
		"github.com/pandatix/go-abnf v0.5.3",
		"google.golang.org/protobuf v1.36.12",
		"Apache License",
		"Version 2.0, January 2004",
		"Data model artifacts for Prometheus.",
		"Common libraries shared by Prometheus Go components.",
		"Open Knowledge Foundation Ltd.",
		"Lucas TESSON - PandatiX",
		"Copyright (c) 2018 The Go Authors.",
		"Permission is hereby granted",
		"Redistribution and use in source and binary forms",
	}
	for _, text := range required {
		if !bytes.Contains(body, []byte(text)) {
			t.Fatalf("tracked notices missing %q", text)
		}
	}
	absent := []string{
		"GNU AFFERO GENERAL PUBLIC LICENSE",
		"github.com/prometheus/client_golang v1.25.0",
		"github.com/prometheus/procfs v0.22.0",
		"github.com/beorn7/perks v1.0.1",
		"github.com/cespare/xxhash/v2 v2.3.0",
		"golang.org/x/sys v0.49.0",
	}
	for _, text := range absent {
		if bytes.Contains(body, []byte(text)) {
			t.Fatalf("tracked notices include %q", text)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func archiveModes(t *testing.T, path string) map[string]int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	modes := map[string]int64{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[hdr.Name] = hdr.Mode
	}
	return modes
}

func zipFileModes(t *testing.T, path string) map[string]int64 {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	modes := map[string]int64{}
	for _, file := range r.File {
		modes[file.Name] = int64(file.Mode().Perm())
	}
	return modes
}

func elfHeader(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[18:], machine)
	return b
}

func machoHeader(cpu uint32) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b[0:], 0xFEEDFACF)
	binary.LittleEndian.PutUint32(b[4:], cpu)
	return b
}

func peHeader(machine uint16) []byte {
	b := make([]byte, 0x80)
	b[0], b[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(b[0x3c:], 0x40)
	copy(b[0x40:], []byte{'P', 'E', 0, 0})
	binary.LittleEndian.PutUint16(b[0x44:], machine)
	return b
}

func stringsJoin(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += " "
		}
		out += part
	}
	return out
}
