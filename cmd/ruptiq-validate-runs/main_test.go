package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUsageAndRedaction(t *testing.T) {
	const userSecret = "s3cret-user"
	const querySecret = "s3cret-token"
	raw := "http://user:" + userSecret + "@127.0.0.1/work?token=" + querySecret
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("help exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") || !strings.Contains(stderr.String(), "-before-dir") {
		t.Fatalf("help = %s", stderr.String())
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "positional", args: []string{"-before-dir", t.TempDir(), "-after-dir", t.TempDir(), "-out-dir", t.TempDir(), raw}, want: "unexpected"},
		{name: "flag", args: []string{"-throughput-change", raw, "-before-dir", t.TempDir(), "-after-dir", t.TempDir(), "-out-dir", t.TempDir()}, want: "invalid"},
		{name: "config", args: []string{"-before-dir", raw, "-after-dir", t.TempDir(), "-out-dir", t.TempDir(), "-min-windows", "0"}, want: "min windows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, err bytes.Buffer
			code := run(tc.args, &out, &err)
			if code != exitUsage {
				t.Fatalf("exit = %d, stderr %s", code, err.String())
			}
			blob := out.String() + err.String()
			if strings.Contains(blob, userSecret) || strings.Contains(blob, querySecret) {
				t.Fatalf("secret leaked:\n%s", blob)
			}
			if !strings.Contains(blob, tc.want) {
				t.Fatalf("stderr = %s", err.String())
			}
		})
	}
}
