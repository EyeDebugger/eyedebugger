---
status: accepted
date: 2026-09-23
decision-makers: Ijat (@ijat)
---

# Use cobra for the CLI

## Context and Problem Statement

The `eyedbg` CLI surface (docs/DESIGN.md §4) has nested subcommands (`bp add`, `bp ls`, `bp rm`),
long and short flag aliases (`--session`/`-s`), and forms like
`eyedbg bp add Foo.cs:12 --if "x>3"` that mix flags after positional-looking arguments. Which
framework, if any, should parse this?

## Decision Drivers

* Must support nested subcommands and mixed flag/positional ordering.
* Startup cost matters: the CLI is invoked repeatedly by agents (docs/DESIGN.md §2).
* Binary size is secondary to correctness and contributor familiarity.
* The dependency should be well-maintained and widely known to OSS contributors.

## Considered Options

* Standard library `flag` package.
* spf13/cobra (with pflag, no viper).
* urfave/cli v3.
* kong.

## Decision Outcome

Chosen option: "spf13/cobra v1.10.2, no viper", confined to `internal/cli` and `cmd/` via a
depguard rule in `.golangci.yml`. Cobra powers gh, kubectl and docker, giving contributors an
existing mental model, plus shell completion and doc generation for free. Env vars
(`EYEDBG_SESSION`, `EYEDBG_CLIENT`) are read explicitly where needed, so viper adds no value.

### Consequences

* Good, because nested commands and long/short flag aliases are handled by the framework instead of
  hand-rolled.
* Good, because the import boundary is enforced automatically (`.golangci.yml` depguard
  `cli-framework` rule).
* Neutral, because it measurably costs about 0.7 ms of startup time (≈5.3 ms vs ≈4.6 ms per run,
  200 runs × 3 rounds, darwin/arm64) and about 2.5 MB of binary size (4.2 MB vs 1.7 MB), against a
  minimal stdlib-`flag` binary.
* Bad, because it pulls in `pflag` (BSD-3-Clause) and, on Windows, `mousetrap` (Apache-2.0) as
  transitive dependencies.

## Pros and Cons of the Options

### Standard library `flag`

* Good, because it adds no dependency.
* Bad, because flag parsing stops at the first non-flag argument (`go doc flag`), which breaks
  forms like `eyedbg bp add Foo.cs:12 --if "x>3"` from docs/DESIGN.md §4.
* Bad, because nested subcommands and short/long aliases (`-s`/`--session`) would have to be
  hand-rolled.

### spf13/cobra

* Good, because it is the de facto standard for Go CLIs with nested commands (gh, kubectl, docker).
* Good, because of built-in shell completion and doc generation.
* Bad, because of the measured startup and binary-size cost above.

### urfave/cli v3

* Good, because it is a viable, maintained alternative with a simpler API.
* Bad, because fewer OSS contributors are already familiar with it compared to cobra.

### kong

* Good, because it derives the CLI from struct tags, which some find more declarative.
* Bad, because fewer OSS contributors are already familiar with it compared to cobra.

## More Information

* cobra: https://github.com/spf13/cobra (Apache-2.0)
* pflag: https://github.com/spf13/pflag (BSD-3-Clause)
* mousetrap: https://github.com/inconshreveable/mousetrap (Apache-2.0, Windows-only use)
* CLI surface: docs/DESIGN.md §4
* Import boundary enforcement: `.golangci.yml` (`depguard` → `cli-framework`)
