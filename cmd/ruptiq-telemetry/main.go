// Command ruptiq-telemetry reads one snapshot of a target's Prometheus
// metrics and prints a short summary. It reads once; it does not collect
// telemetry continuously or run a load test.
//
//	ruptiq-telemetry -url http://localhost:8082/metrics -out snapshot.json
//
// Exit status: 0 the snapshot was read, 1 it could not be read (or the -out
// file could not be written), 2 invalid arguments.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/RajK158/ruptiq/internal/telemetry"
)

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

type config struct {
	url     string
	out     string
	timeout time.Duration
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("ruptiq-telemetry", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-telemetry -url URL [-out FILE] [-timeout DURATION]")
		fmt.Fprintln(stderr, "Reads one snapshot of Prometheus metrics (text format) from URL.")
		fs.PrintDefaults()
	}
	var c config
	fs.StringVar(&c.url, "url", "", "Prometheus metrics endpoint, for example http://localhost:8082/metrics (required)")
	fs.StringVar(&c.out, "out", "", "also write the snapshot as JSON to this `file` (optional)")
	fs.DurationVar(&c.timeout, "timeout", telemetry.DefaultTimeout, "maximum time for the request and response")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	if c.url == "" {
		return config{}, errors.New("-url is required")
	}
	if c.timeout <= 0 {
		return config{}, fmt.Errorf("-timeout must be positive, got %v", c.timeout)
	}
	return c, nil
}

// run is main without process-level side effects, for tests.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-telemetry:", err)
		}
		return exitUsage
	}
	reader, err := telemetry.NewPrometheusReader(telemetry.PrometheusConfig{URL: c.url, Timeout: c.timeout})
	if err != nil {
		fmt.Fprintln(stderr, "ruptiq-telemetry:", err)
		return exitUsage
	}

	report := telemetry.Report{SchemaVersion: telemetry.ReportSchemaVersion, Endpoint: reader.Endpoint()}
	code := exitOK
	snapshot, err := reader.Read(ctx)
	if err != nil {
		report.Error = err.Error()
		fmt.Fprintln(stderr, "ruptiq-telemetry: no snapshot:", err)
		code = exitFailed
	} else {
		report.Snapshot = &snapshot
		if err := telemetry.WriteSummary(stdout, snapshot); err != nil {
			fmt.Fprintln(stderr, "ruptiq-telemetry:", err)
			code = exitFailed
		}
	}

	if c.out != "" {
		if err := writeReport(c.out, report); err != nil {
			fmt.Fprintln(stderr, "ruptiq-telemetry: writing snapshot:", err)
			return exitFailed
		}
		if report.Snapshot != nil {
			fmt.Fprintln(stdout, "\nFull snapshot written to", c.out)
		} else {
			fmt.Fprintln(stderr, "Failure recorded in", c.out)
		}
	}
	return code
}

// writeReport writes report as indented JSON through a temporary file in the
// same directory, so a reader never sees a partial file.
func writeReport(path string, report telemetry.Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".ruptiq-telemetry-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
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
	return os.Rename(tmpName, path)
}
