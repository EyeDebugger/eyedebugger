# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Project scaffold following docs/DESIGN.md §12: `eyedbg`, `eyedbgd` and `eyedbg-mcp` binaries (only `version` is implemented).
- `eyedbg version` with text and `--json` (`"schema": 1`) output; version injected at link time.
- CI: lint, race-enabled tests on Linux, macOS and Windows, cross-builds for 6 OS/architecture targets, govulncheck, GoReleaser snapshot; Dependabot.
- Documentation: contributing guide (DCO), code conventions, maintainer guide, security policy, governance, code of conduct, ADRs 0001–0008.

[Unreleased]: https://github.com/EyeDebugger/eyedebugger/commits/main
