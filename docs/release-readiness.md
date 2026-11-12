# Ruptiq — Release Readiness Audit

This is an inspection of the repository at `d1d124532efd60ada0e366b6d68a7820d0f72fb5`
(`Merge pull request #21 from RajK158/docs/developer-quickstart-demo`). It separates
what the code and CI already show from what is still unverified, and from decisions
the repository owner has not made.

It is not a production-readiness claim. The current CLI archives are unsigned
development builds. There is no official release.

GitHub metadata below was read from the public GitHub API on 11 November 2026.
The four Actions checks are the check runs on that merge commit. This audit did
not re-run the live performance experiment or the Go test suite.

## Status update — 11 November 2026

The sections below record the repository at `d1d124532efd60ada0e366b6d68a7820d0f72fb5`,
when it had no license file. After that audit the owner selected the MIT License.

- The owner selected MIT. The copyright line is `Copyright (c) 2026 RajK158`.
- A root `LICENSE` file has been added. It is the standard MIT text.
- CLI archives now include that file at the archive root with mode `0644`, beside
  `README.md` and the three executables. The archive note says the project is
  MIT-licensed and points at `LICENSE`.
- The MIT text is the license for this project. It does not state the terms of
  third-party dependencies. That review remains pending.
- An official versioned release, retention beyond the seven-day Actions artifact,
  and signing remain pending. Adding `LICENSE` does not publish a release.

## Status update — 11 November 2026, third-party licenses

The MIT update above left third-party review pending. License files for the
modules this repository imports have now been read. The notes are in
[docs/third-party-licenses.md](third-party-licenses.md).

- Ten imported modules have a license file at the version in `go.mod` and `go.sum`.
  Five of them are in the import graph of the packaged `ruptiq-pair`,
  `ruptiq-diagnose`, and `ruptiq-validate-runs` commands.
- Those five include Apache-2.0 modules that ship a `NOTICE` file, and BSD
  modules whose text asks for the copyright and disclaimer to travel with a
  binary distribution. The CLI archives still contain only Ruptiq's MIT
  `LICENSE`. This update does not decide whether that satisfies those texts.
- k6 v2.3.0, the version pinned in CI, is GNU AGPL version 3. The archives do
  not contain k6.
- Redistribution review stays open until the notices that belong with an
  official binary are chosen and added. No packaging change was made here.

## A. Current deliverables

Implemented and covered by code, tests, or a recorded run:

- **Guided workflow.** `ruptiq-pair` is three separate invocations: `-stage before`,
  `-stage after`, and `-stage compare`. Each process exits. The developer changes
  the service outside Ruptiq. Compare does not start k6. See `cmd/ruptiq-pair` and
  `internal/pairworkflow`.
- **Go orchestration and external k6.** Before and after start k6 as a separate
  process. The executable is `-k6` or `k6` on `PATH`. k6 is not in this repository.
- **Prometheus telemetry.** The experiment scrapes one metrics URL on an interval
  while the workload runs.
- **Offline analysis and investigation.** Saved samples become rates, 5xx shares,
  and estimated percentiles. The single-run investigator can report
  `changes_observed`, `no_material_change`, or `insufficient_evidence`. A completed
  diagnosis is not itself a performance finding.
- **SHA-256 provenance and manifests.** A comparison records the SHA-256 of each
  raw `analysis.json`. Evidence pointers name objects in the analysis file for
  the side they cite. Pair state is `baseline_captured`, `candidate_captured`, or
  `comparison_completed`.
- **Comparison integrity and overwrite protection.** `internal/fixvalidation`
  is the judgment for throughput, 5xx ratio, and median per-window p95. An
  exclusive `.ruptiq-pair-reservation` file is created with `O_EXCL` and mode
  `0600`. A finished stage is not overwritten. A refused overwrite exits 2.
- **Regression-lab demonstration.** `demo/regression-lab` is a loopback-only HTTP
  process with real CPU work and `GET /metrics`. The local walk-through is
  [docs/demo.md](demo.md). One Windows heavy-to-light run is recorded in the
  README as Task 12A. Those numbers are that run, not a release benchmark.
- **Cross-platform development archives.** `scripts/package-cli.go` builds
  `ruptiq-pair`, `ruptiq-diagnose`, and `ruptiq-validate-runs` for five
  GOOS/GOARCH pairs. Output is gitignored `dist/`.
- **CI.** `.github/workflows/ci.yml` has four jobs. On the PR #21 merge commit,
  GitHub reported all four check runs `completed` / `success`: `test`,
  `Docker checkout integration`, `Guided workflow integration`, and
  `CLI distribution builds`.

These commands stay in the module and are not in the archives: `ruptiq-api`,
`ruptiq-run`, `ruptiq-compare`, `ruptiq-telemetry`, `ruptiq-experiment`,
`ruptiq-analyze`, `ruptiq-investigate`, and `ruptiq-validate-fix`.

Not started, from the README status table: cancellation and compensation,
Order and Payment telemetry, root-cause diagnosis, run storage, and load ramps.
Graceful shutdown of the HTTP servers is implemented and is marked not yet
verified by a real Ctrl+C run. There is no GUI, hosted dashboard, or run database.
Ruptiq does not edit code, restart a service, or run a statistical significance test.

## B. Supported platforms

A passing cross-compile is not a runtime certification. Header checks read the
file format. They do not start the program.

The `CLI distribution builds` job on `d1d1245` runs `go run ./scripts` on
`ubuntu-latest`. That program builds all five targets with `CGO_ENABLED=0`,
`-trimpath`, and `-ldflags=-s -w`, then checks each binary's header before
packaging (`internal/clidist`). The same job executes only the extracted Linux
AMD64 binaries, and only with `-h` (exit 2 and the usage line).

| Platform | Cross-compiled | Format and CPU checked | Executed |
| --- | --- | --- | --- |
| Linux AMD64 | Yes, in that CI job | ELF, amd64 | Packaged binaries: `-h` only, in that CI job. The guided-workflow job separately builds `ruptiq-pair` from source on `ubuntu-latest` and runs before, after, and compare with real k6 against the loopback lab. That is a workflow smoke test, not a benchmark, and it does not execute the packaged archive. |
| Linux ARM64 | Yes, in that CI job | ELF, arm64 | No execution evidence. |
| macOS Intel | Yes, in that CI job | Mach-O, amd64 | No execution evidence. |
| macOS Apple Silicon | Yes, in that CI job | Mach-O, arm64 | No execution evidence. |
| Windows AMD64 | Yes, in that CI job | PE, amd64 | CI does not execute it. Task 12B.2 recorded one local `-h` of the three extracted `.exe` files on the Windows development machine (each exited 2). This audit did not repeat that run. Task 12A ran the guided workflow with `go run` on Windows, not from the zip. |

## C. Installation and dependencies

- **Source builds** need Go 1.27 or newer. That version is the `go` line in `go.mod`.
  `go.sum` pins the modules.
- **Prebuilt CLI executables** do not need Go. The install section of `README.md`
  says so. The archive README generated by `internal/clidist` says the same.
- **Live runs** need k6, installed separately. Development and the guided-workflow
  job use v2.3.0. The CI job downloads that version from the Grafana GitHub release
  and checks the pinned SHA-256. It does not fall back to a newer k6. The archives
  do not contain k6.
- **Offline comparison** does not need k6: `ruptiq-pair -stage compare` and
  `ruptiq-validate-runs` read saved diagnosis directories.
- **The regression lab** is source. `docs/demo.md` runs it with
  `go run ./demo/regression-lab`, so that demo needs Go even when the CLI is a
  prebuilt binary.
- **Archive layout.** Linux and macOS files are `ruptiq-cli-<goos>-<goarch>.tar.gz`.
  Windows is `ruptiq-cli-windows-amd64.zip`. Each archive contains `README.md` and
  the three executables (`.exe` on Windows). Names are development filenames, not
  a version. `dist/ruptiq-cli-SHA256SUMS.txt` is lowercase hex, two spaces, then
  the filename. Linux checks it with `sha256sum -c`. macOS checks it with
  `shasum -a 256 -c`.
- **Distribution channel.** The CI job uploads `ruptiq-cli-development` with
  `actions/upload-artifact@v7` and `retention-days: 7`. Downloading the Actions
  artifact wraps those files in another zip. There is no Git tag and no GitHub
  Release. The GitHub releases API returned an empty list.
- **Signing.** The archives are unsigned. macOS builds are not notarized. A
  matching checksum does not make a file signed. `-s -w` strips symbols; it is
  not a signature.

## D. Security and artifact hygiene

This section lists controls found in the current tree and CI file. It is not a
penetration test and it is not an audit of the full Git history.

Verified in the current files:

- `.gitignore` ignores `/tmp/` (experiment reports), `/dist/` and `/build/`
  (local archives), Go binaries, `.env` and `.env.*` (with an allowance only for
  `.env.example`), `*.pem`, `*.key`, `*.p12`, `*.pfx`, `secrets/`, and
  `credentials*.json`. No `.env.example` is present.
- `.dockerignore` is an allow-list: `go.mod`, `go.sum`, and `demo/checkout/`.
  Checkout tests and `*.exe` stay out of the image context. Ruptiq Core and
  `.git` are not in that context.
- `.github/workflows/ci.yml` sets workflow `permissions: contents: read`. The
  CLI job sets the same permission again. The workflow does not reference
  `secrets.*`. The guided-workflow job does not upload reports. The CLI job
  uploads only the five archives and the checksum file, and it fails if those
  files are missing. The Docker job removes containers, volumes, and images
  at the end.
- Archive contents are the README and the three executables.
  `ValidateArchive` rejects extra names, nested paths, and duplicates.
  Packing reads that name list; a marker file written into the staging directory
  is not packaged.
- Saved target and metrics URLs pass through `workload.RedactURL`, which
  replaces user information and the query string. The pair fingerprint stores
  that redacted URL and a SHA-256 of the original string so a later stage can
  detect a change. The original URL is not written into the fingerprint. The
  hash is a consistency check, not a claim that a guessable URL is secret.
  CLI errors go through `workload.RedactText`.

Lightweight scan of the current tracked tree, without printing match text:

- `git ls-files` showed no `LICENSE`, `.env`, credential, key, `dist`, `tmp`,
  or executable paths.
- `git grep` for private-key headers and common cloud or GitHub token prefixes
  found no matches in tracked files.
- Redaction unit tests contain deliberate placeholder tokens. Those strings
  were not copied here and were not treated as repository credentials.

Not audited: every historical commit, dependency vulnerabilities, runtime
behavior of k6, host firewall or signing policy, and whether a future workflow
change could widen token permissions. `go.sum` records module checksums. This
audit did not run `govulncheck` or a license scanner.

## E. Test coverage

No coverage percentage is stated here. None was measured for this audit.

What the four successful jobs on `d1d1245` actually run:

| Job | What it verifies |
| --- | --- |
| `test` | `go test ./...`, `go vet ./...`, and `go test -race ./...` on `ubuntu-latest`, using the Go version in `go.mod`. |
| `Docker checkout integration` | `docker compose` builds the checkout demo, waits until healthy, and runs `scripts/checkout-smoke-test.sh`. It does not run k6 or `ruptiq-pair`. |
| `Guided workflow integration` | Pinned k6 v2.3.0, a loopback regression lab, and `ruptiq-pair` through `scripts/guided-workflow-smoke-test.sh`: baseline, rejected duration mismatch, candidate, offline compare, analysis hashes, evidence pointers, and a refused second compare. It does not require `improvement_observed` or any exact rate or latency. Reports are not uploaded. |
| `CLI distribution builds` | All five archives, `sha256sum -c`, header checks inside the packager, and Linux AMD64 `-h`. It does not start a workload. |

Not covered by those jobs:

- macOS runtime execution, Intel or Apple Silicon.
- Linux ARM64 runtime execution.
- Windows runtime execution in CI. The local Task 12B.2 `-h` check is not this job.
- A live workload started from the packaged archives.
- Repeated runs, statistical performance benchmarking, production traffic, or sustained scalability.
- Full security penetration testing.
- Signature or notarization checks. There is nothing signed to verify.
- A real Ctrl+C shutdown of `ruptiq-api` and the checkout servers. The README still marks that unverified.

## F. Licensing and redistribution

The bullets in this section describe the PR #21 merge, before a license file
existed. The status update above records the later MIT selection. This section
does not state a legal conclusion about third-party compliance.

- The GitHub repository `RajK158/ruptiq` is public (`visibility: public`).
- No `LICENSE` file is in the tree. The GitHub license API returned HTTP 404,
  and the repository metadata has no license object.
- Public visibility lets people read the code. It does not by itself grant a
  general right to copy, modify, or redistribute it.
- An explicit licensing decision is still pending. Two different outcomes are
  possible: a permissive open-source license, or a source-available notice that
  does not grant an open-source license. Either choice belongs to the owner.
  Adding the file is a separate change.
- `go.mod` requires `github.com/prometheus/client_golang`,
  `github.com/prometheus/client_model`, and `github.com/prometheus/common`, plus
  the indirect modules in that file. `go.sum` pins their checksums. Each module
  has its own license. Those licenses were not reviewed for redistribution here.
  k6 is a separate program and is not bundled.

## G. Release blockers versus optional improvements

### Required decision before an official public release

The project license is no longer an open choice. The status update records the
MIT selection and the root `LICENSE` file. These decisions are still open:

- Decide what an official release is. The current artifact is an unsigned,
  unversioned Actions upload kept for seven days. Publishing that upload as a
  release, or creating a tag and a GitHub Release, is a decision. This audit
  does not make it.
- Before redistributing binaries, review the licenses of the Go modules those
  binaries include. This audit did not complete that review.

A missing GUI, run database, autonomous code fix, or statistical test is not a
blocker for a CLI whose README already says those things are absent.

### Strongly recommended validation

- Run the packaged binaries, not only `-h` on Linux AMD64, on every OS the
  release claims to support. Linux ARM64 and both macOS targets have no
  execution evidence. Windows CI does not execute the zip.
- Run the guided demo once from the packaged `ruptiq-pair` on each claimed OS,
  with the external k6 the user will actually install.
- If the release includes `ruptiq-api` or the checkout servers, verify the
  Ctrl+C and SIGTERM shutdown the README still marks unverified.
- Keep the release notes tied to a commit. The seven-day artifact name
  `ruptiq-cli-development` does not identify a version.

### Optional future enhancement

- A GUI or hosted dashboard.
- A persistent run database.
- Autonomous code changes or a proven root-cause claim.
- Statistical significance testing and repeated-run benchmarks.
- Order and Payment telemetry, cancellation and compensation, and load ramps.
- Code signing and Apple notarization, if the owner wants them beyond an
  unsigned MVP.
- Packaging the source-only commands that are not in the three-CLI archive.

## H. Release checklist

Checked items from the PR #21 merge have evidence in `d1d1245`, the workflow
file, or the GitHub check runs for that commit. The license item is checked
because the owner later selected MIT and this change adds `LICENSE`. Unchecked
items are still open. Third-party review and a versioned release are not resolved.

- [x] Main contains PR #21 at `d1d124532efd60ada0e366b6d68a7820d0f72fb5`.
- [x] `test`, `Docker checkout integration`, `Guided workflow integration`, and
      `CLI distribution builds` succeeded on that commit
      (`.github/workflows/ci.yml`).
- [x] Guided workflow behavior and limits are written in `README.md`,
      `docs/demo.md`, and `docs/architecture.md`.
- [x] Development archive names, checksums, and the seven-day upload are
      implemented in `internal/clidist` and `.github/workflows/ci.yml`.
- [x] The repository is public and has no GitHub Releases.
- [x] Owner selected the MIT License on 11 November 2026. Root `LICENSE` is
      added in this change. The `d1d1245` tree recorded above had no license file.
- [ ] Third-party license texts have been read
      ([docs/third-party-licenses.md](third-party-licenses.md)). The CLI archives
      still omit the Apache `NOTICE` files and the BSD notice text for modules
      linked into the three commands. That redistribution step is open.
- [ ] Owner decides whether to create a versioned, retained release, and whether
      the binaries will be signed.
- [ ] Packaged Linux ARM64 binaries have been executed on that OS.
- [ ] Packaged macOS Intel binaries have been executed on that OS.
- [ ] Packaged macOS Apple Silicon binaries have been executed on that OS.
- [ ] Packaged Windows binaries have been executed in CI, or a new recorded
      local run is accepted in place of CI.
- [ ] Any OS listed as supported has a guided before/after/compare run from the
      packaged CLI and an external k6, with the result labeled as that run.
- [ ] If servers are in the release scope, Ctrl+C shutdown has been verified.
