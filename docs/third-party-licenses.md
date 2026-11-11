# Ruptiq — Third-Party License Review

This review identifies license texts for software Ruptiq builds or runs. It is
not a legal opinion, and it does not say that redistribution already complies
with those texts. Ruptiq's own [MIT License](../LICENSE) applies to this
project. It does not replace the license of a dependency.

The repository commit is `1dffdc18dd6cc82541751878a76e5f9b728999b3`
(`Merge pull request #23 from RajK158/chore/add-mit-license`).

## A. Scope

Inspected:

- `go.mod`, `go.sum`, and `go list -m all`.
- The non-test import graph of `./cmd/ruptiq-pair`, `./cmd/ruptiq-diagnose`,
  and `./cmd/ruptiq-validate-runs`.
- The non-test and test import graphs of `./...`, so a module used only by
  another program in this repository can be told from one used by those three
  commands.
- `go mod why -m` for modules that appear in `go list -m all` but not in the
  Ruptiq import graph.
- The `LICENSE`, `LICENSE.txt`, and `NOTICE` files in the module cache for each
  imported third-party module, at the version `go list -m` selected.
  `go mod download` retrieved those exact versions. It did not change `go.mod`
  or `go.sum`.
- `LICENSE.md` at the `v2.3.0` tag of `github.com/grafana/k6`.
- `internal/clidist/clidist.go`, which chooses the files in a CLI archive.
- [docs/release-readiness.md](release-readiness.md).

Not inspected:

- License files for modules that `go list -m all` names but this repository
  does not import. Those licenses are marked not reviewed.
- Other files in the k6 tag besides `LICENSE.md`.
- Whether a statically linked Go binary is a "derivative work" or a "substantial
  portion" under any of these licenses. The sections below quote what the
  license files say. They do not apply that wording to Ruptiq as a legal
  conclusion.
- A full scan of `go.sum` history, or vulnerability reports.

## B. Dependency inventory

`go.mod` directly requires three modules. Seven more are listed there as
indirect. `go list -deps` on the three packaged commands imports five
third-party modules. `go list -deps ./...` imports those five plus five more,
through `demo/regression-lab` and the checkout Inventory Service. Both use
`github.com/prometheus/client_golang/prometheus`. The three packaged commands
do not.

`go list -test -deps ./...` imports the same ten modules. Ruptiq's own tests
do not import an eleventh module.

`go list -m all` also names 23 modules that those import graphs do not use.
They are listed after the table. They are not described as part of the
packaged binaries.

| Module | Version | In `go.mod` | In the three CLIs | License file read | Source | Notice condition in that file |
| --- | --- | --- | --- | --- | --- | --- |
| `github.com/prometheus/client_model` | v0.6.3 | direct | yes | Apache License, Version 2.0, plus `NOTICE` | module cache `LICENSE` and `NOTICE` | Give recipients this license. Keep copyright and attribution notices. If a `NOTICE` file is part of the work, distribute its attribution notices with a derivative work. The `NOTICE` names The Prometheus Authors, 2012–2015, and SoundCloud Ltd. |
| `github.com/prometheus/common` | v0.73.1 | direct | yes | Apache License, Version 2.0, plus `NOTICE` | module cache `LICENSE` and `NOTICE` | Same Apache conditions. The `NOTICE` names The Prometheus Authors, 2015, and SoundCloud Ltd. |
| `github.com/munnerz/goautoneg` | v0.0.0-20191010083416-a7dc8b61c822 | indirect | yes | BSD 3-Clause | module cache `LICENSE` | Copyright (c) 2011, Open Knowledge Foundation Ltd. Source redistributions must keep the copyright, conditions, and disclaimer. Binary redistributions must reproduce them in documentation or other materials. The organization's name may not be used to endorse a product without written permission. |
| `github.com/pandatix/go-abnf` | v0.5.3 | indirect | yes | MIT | module cache `LICENSE` | Copyright (c) 2024 Lucas TESSON - PandatiX. Copies or substantial portions must include that copyright and the permission notice. |
| `google.golang.org/protobuf` | v1.36.12 | indirect | yes | BSD 3-Clause | module cache `LICENSE` | Copyright (c) 2018 The Go Authors. Same three BSD conditions as `goautoneg`. The endorsement clause names Google Inc. No `NOTICE` file was in the module root. |
| `github.com/prometheus/client_golang` | v1.25.0 | direct | no | Apache License, Version 2.0, plus `NOTICE` | module cache `LICENSE` and `NOTICE` | Same Apache conditions. The `NOTICE` names The Prometheus Authors, 2012–2015, SoundCloud Ltd., the `beorn7/perks` fork, and Go support for protocol buffers. Used by the regression lab and the Inventory Service, not by the three archived commands. |
| `github.com/prometheus/procfs` | v0.22.0 | indirect | no | Apache License, Version 2.0, plus `NOTICE` | module cache `LICENSE` and `NOTICE` | Same Apache conditions. The `NOTICE` names The Prometheus Authors, 2014–2015, and SoundCloud Ltd. Reached through `client_golang/prometheus`. |
| `github.com/beorn7/perks` | v1.0.1 | indirect | no | MIT | module cache `LICENSE` | Copyright (C) 2013 Blake Mizerany. Copies or substantial portions must include that copyright and the permission notice. No `NOTICE` file was in the module root. |
| `github.com/cespare/xxhash/v2` | v2.3.0 | indirect | no | MIT | module cache `LICENSE.txt` | Copyright (c) 2016 Caleb Spare. Same MIT condition. |
| `golang.org/x/sys` | v0.49.0 | indirect | no | BSD 3-Clause | module cache `LICENSE` | Copyright 2009 The Go Authors. Same three BSD conditions. The endorsement clause names Google LLC. Reached through `golang.org/x/sys/unix` from `client_golang/prometheus`. |

The four Apache `LICENSE` files are byte-identical. Their SHA-256 is
`c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4`.
Each begins with "Apache License, Version 2.0, January 2004" and includes the
appendix. Section 4 states conditions (a) through (d), including the `NOTICE`
paragraph quoted in the table. No separate `COPYING` file was present beside
these license files.

### Modules named by `go list -m all` and not imported

These versions were reported by `go list -m all`. They are absent from
`go list -deps ./...` and from `go list -test -deps ./...`. Their license
files were not read.

`go mod why -m` says the main module does not need
`github.com/alecthomas/kingpin/v2`, `github.com/golang/protobuf`, or
`golang.org/x/net`. The same command reaches `github.com/stretchr/testify`,
`github.com/google/go-cmp`, and `go.uber.org/goleak` only through test files
inside Prometheus modules (`expfmt.test`, `model.test`, and `prometheus.test`).
`go test ./...` does not build those dependency tests.

| Module | Version |
| --- | --- |
| `github.com/alecthomas/kingpin/v2` | v2.4.0 |
| `github.com/alecthomas/units` | v0.0.0-20240927000941-0f3dac36c52b |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 |
| `github.com/golang/protobuf` | v1.5.0 |
| `github.com/google/go-cmp` | v0.7.0 |
| `github.com/google/uuid` | v1.6.0 |
| `github.com/jpillora/backoff` | v1.0.0 |
| `github.com/json-iterator/go` | v1.1.12 |
| `github.com/julienschmidt/httprouter` | v1.3.0 |
| `github.com/klauspost/compress` | v1.20.1 |
| `github.com/kylelemons/godebug` | v1.1.0 |
| `github.com/modern-go/concurrent` | v0.0.0-20180306012644-bacd9c7ef1dd |
| `github.com/modern-go/reflect2` | v1.0.2 |
| `github.com/mwitkow/go-conntrack` | v0.0.0-20190716064945-2f068394615f |
| `github.com/stretchr/testify` | v1.12.1 |
| `github.com/xhit/go-str2duration/v2` | v2.1.0 |
| `go.uber.org/goleak` | v1.3.0 |
| `go.yaml.in/yaml/v2` | v2.4.4 |
| `go.yaml.in/yaml/v3` | v3.0.5 |
| `golang.org/x/net` | v0.61.0 |
| `golang.org/x/oauth2` | v0.37.0 |
| `golang.org/x/sync` | v0.22.0 |
| `golang.org/x/text` | v0.43.0 |

`go.sum` records checksums for the selected module graph. A checksum there
does not mean the packaged CLI contains that module.

## C. External tools

Grafana k6 is not a Go module in this repository. `ruptiq-pair` and
`ruptiq-diagnose` start it as a separate process. `internal/clidist` archive
contents are `README.md`, `LICENSE`, and the three executables. k6 is not one
of those files.

The license file read for this review is `LICENSE.md` at tag `v2.3.0`:
`https://github.com/grafana/k6/blob/v2.3.0/LICENSE.md`. That file is the
GNU Affero General Public License, version 3, 19 November 2007. The CI job
`Guided workflow integration` installs that same tag and checks its published
SHA-256. It does not copy k6 into the CLI archives.

This review did not read the rest of the k6 tag, and it did not download a k6
binary. If a future archive bundled k6, that archive would be distributing
AGPL-3.0 software together with MIT-licensed Ruptiq. The AGPL text includes
conditions for conveying the program and for source availability. Whether
running an unmodified k6 process, without shipping it, meets or avoids those
conditions is not decided here.

## D. Redistribution considerations

The license files that were read do not all use the MIT terms.

Supported by the text that was read:

- Each MIT file says the copyright notice and the permission notice must be
  included in all copies or substantial portions of that software.
- Each BSD file says a binary redistribution must reproduce the copyright
  notice, the conditions, and the disclaimer in documentation or other
  materials provided with the distribution, and it limits endorsement use of
  the named organization.
- Each Apache file, section 4, says recipients of a distributed derivative
  work receive a copy of that license, modified files are marked, source-form
  notices are retained, and attribution notices from a distributed `NOTICE`
  file are included in a `NOTICE`, in source or documentation, or in a display
  where third-party notices normally appear.

The current CLI archive does not contain those third-party license files or
`NOTICE` files. It contains Ruptiq's `LICENSE` only. This review does not
conclude that the omission violates a condition. It does record that the
texts above are not in the archive.

Not decided here:

- Whether a Go binary that contains compiled dependency code is a copy, a
  substantial portion, or a derivative work under each license.
- Whether shipping the module source from this repository, with `go.sum`
  pointing at the modules, already satisfies a source-form notice condition.
- Any conflict between AGPL-3.0 and MIT if k6 were later placed inside a
  Ruptiq archive.
- License obligations for the 23 modules that this repository does not import.

## E. Findings and remaining actions

Verified from the files named above: all ten imported third-party modules.
Nothing in that set was left without a readable license file. The 23 modules
that are not imported were not verified.

Already supplied: Ruptiq's MIT `LICENSE` in the repository and in each CLI
archive. The archive README says that MIT text does not state third-party
terms. No third-party `NOTICE` is included.

Before an official binary release, the archive for the three commands should
gain the license text, and where present the `NOTICE` text, for the five
modules in its import graph:

- `github.com/prometheus/client_model` v0.6.3
- `github.com/prometheus/common` v0.73.1
- `github.com/munnerz/goautoneg` v0.0.0-20191010083416-a7dc8b61c822
- `github.com/pandatix/go-abnf` v0.5.3
- `google.golang.org/protobuf` v1.36.12

If a later release also ships `demo/regression-lab` or the Inventory Service
binary, the same kind of notice is relevant for `client_golang`, `procfs`,
`perks`, `xxhash`, and `golang.org/x/sys` at the versions in the table.
Packaging was not changed in this review.

k6 stays an external AGPL-3.0 program at the v2.3.0 pin. Bundling it would be
a separate decision.

No module reviewed here was mislabeled as MIT when its file said Apache or
BSD. No compliance claim is made.
