package clidist

import (
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"os"
)

// Format is the container of a built CLI executable.
type Format string

const (
	FormatELF   Format = "elf"
	FormatMachO Format = "macho"
	FormatPE    Format = "pe"
)

// Identify reads a binary header and reports its container and CPU.
// It does not execute the file. arm64 and amd64 are the only accepted CPUs.
func Identify(path string) (Format, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	format, arch, err := identifyBytes(b)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	return format, arch, nil
}

func identifyBytes(b []byte) (Format, string, error) {
	if format, arch, ok := identifyELF(b); ok {
		return format, arch, nil
	}
	if format, arch, ok := identifyMachO(b); ok {
		return format, arch, nil
	}
	if format, arch, ok := identifyPE(b); ok {
		return format, arch, nil
	}
	return "", "", fmt.Errorf("unrecognized executable format")
}

func identifyELF(b []byte) (Format, string, bool) {
	if len(b) < 20 || b[0] != 0x7f || b[1] != 'E' || b[2] != 'L' || b[3] != 'F' {
		return "", "", false
	}
	if elf.Class(b[4]) != elf.ELFCLASS64 || elf.Data(b[5]) != elf.ELFDATA2LSB {
		return "", "", false
	}
	machine := elf.Machine(binary.LittleEndian.Uint16(b[18:]))
	switch machine {
	case elf.EM_X86_64:
		return FormatELF, "amd64", true
	case elf.EM_AARCH64:
		return FormatELF, "arm64", true
	default:
		return "", "", false
	}
}

func identifyMachO(b []byte) (Format, string, bool) {
	if len(b) < 8 {
		return "", "", false
	}
	magic := binary.LittleEndian.Uint32(b[0:4])
	if magic != macho.Magic64 {
		return "", "", false
	}
	switch binary.LittleEndian.Uint32(b[4:8]) {
	case uint32(macho.CpuAmd64):
		return FormatMachO, "amd64", true
	case uint32(macho.CpuArm64):
		return FormatMachO, "arm64", true
	default:
		return "", "", false
	}
}

func identifyPE(b []byte) (Format, string, bool) {
	if len(b) < 0x40 || b[0] != 'M' || b[1] != 'Z' {
		return "", "", false
	}
	off := binary.LittleEndian.Uint32(b[0x3c:])
	if int(off) < 0 || int(off)+6 > len(b) {
		return "", "", false
	}
	if string(b[off:off+4]) != "PE\x00\x00" {
		return "", "", false
	}
	switch binary.LittleEndian.Uint16(b[off+4:]) {
	case 0x8664: // IMAGE_FILE_MACHINE_AMD64
		return FormatPE, "amd64", true
	case 0xAA64: // IMAGE_FILE_MACHINE_ARM64
		return FormatPE, "arm64", true
	default:
		return "", "", false
	}
}

// expectFormat is the container and CPU a GOOS/GOARCH build must produce.
func expectFormat(goos, goarch string) (Format, string, error) {
	var format Format
	switch goos {
	case "linux":
		format = FormatELF
	case "darwin":
		format = FormatMachO
	case "windows":
		format = FormatPE
	default:
		return "", "", fmt.Errorf("unsupported GOOS %s", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", "", fmt.Errorf("unsupported GOARCH %s", goarch)
	}
	return format, goarch, nil
}
