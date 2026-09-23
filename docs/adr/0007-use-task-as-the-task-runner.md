---
status: accepted
date: 2026-09-23
decision-makers: Ijat (@ijat)
---

# Use Task as the task runner

## Context and Problem Statement

Contributors need a single, memorable entry point for building, testing, linting and releasing
(`build`, `test:race`, `lint`, `snapshot`, ...), including the ldflags-heavy build command. The
project targets Windows, macOS and Linux contributors equally (docs/DESIGN.md §12). What runs these
developer tasks?

## Decision Drivers

* Windows contributors should not need `make`, a POSIX shell, or WSL.
* One command should mirror what CI runs, to catch problems locally first.
* Minimal extra CI surface: adding a task-runner action to every CI job is undesirable.

## Considered Options

* GNU Make.
* Task (go-task/task), `Taskfile.yml`.
* just.
* mage.
* No task runner; document raw commands only.

## Decision Outcome

Chosen option: "Task (go-task/task) v3", via `Taskfile.yml`. `task ci` mirrors what
`.github/workflows/ci.yml` runs, and both files carry "keep in sync" comments. **CI itself calls
`go` and the pinned tool binaries directly, not Task**, to avoid an extra install step and reduce
supply-chain surface in workflows.

### Consequences

* Good, because Windows contributors get a real single entry point without installing `make` or a
  POSIX shell (`winget install Task.Task`, `scoop install task`, `choco install go-task`, or
  `go install github.com/go-task/task/v3/cmd/task@latest`).
* Good, because `task ci` gives a local mirror of CI's checks.
* Bad, because `Taskfile.yml` and `ci.yml` must be kept in sync by hand (both call out to raw `go`
  and pinned tool commands, and each has a "keep in sync" comment).
* Bad, because it adds one more tool for contributors to install, on top of Go and golangci-lint.

## Pros and Cons of the Options

### GNU Make

* Good, because it's extremely common in OSS Go projects.
* Bad, because it isn't present on Windows by default, macOS ships an old 3.81, and recipes
  typically assume a POSIX `sh` — all friction for Windows and .NET-background contributors.

### Task (go-task/task)

* Good, because its embedded shell (mvdan/sh) runs the same recipes on Windows without requiring
  `sh`.
* Good, because `platforms:` lets a task or command be restricted per OS when needed.
* Bad, because `GOOS=x go build`-style env-var prefixes and the `sh:` dynamic var were exercised on
  macOS during planning but not verified on Windows (documented as an assumption; CI does not
  depend on Task, so this doesn't block CI).

### just

* Good, because of a simple, readable syntax.
* Bad, because it still needs a `sh`-compatible shell on Windows for non-trivial recipes.

### mage

* Good, because tasks are plain Go, no new DSL.
* Bad, because it is more verbose for simple tasks and less familiar to casual contributors.

### No task runner

* Good, because it removes one more tool to install.
* Bad, because contributors would hand-type long `ldflags`-bearing build commands, and there is no
  single documented entry point.

## More Information

* Task: https://taskfile.dev
* CI/Taskfile parity: `.github/workflows/ci.yml`, `Taskfile.yml` (both carry "keep in sync"
  comments)
