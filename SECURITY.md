# Security Policy

## Supported versions

There have been no releases yet; fixes land on `main`. After the first release, the latest minor
release gets security fixes.

## Report privately

Report vulnerabilities through GitHub private vulnerability reporting:
https://github.com/EyeDebugger/eyedebugger/security/advisories/new

Never use a public issue, pull request or discussion. Include the `eyedbg version` output, your
OS/arch, the debug adapter and its version, a reproduction, and the impact.

## What to expect

- The project is volunteer-maintained; we aim to acknowledge within 7 days.
- We coordinate the fix and disclosure date with the reporter.
- We credit the reporter unless they decline.

## In scope

- Access control of the daemon IPC endpoint and token file (docs/DESIGN.md §6, §11).
- Untrusted input: adapter messages, adapter manifests, downloaded adapters and checksum
  verification (docs/DESIGN.md §7).
- Eval side effects vs. the read-only default (docs/DESIGN.md §11).
- Redaction and session recordings (docs/DESIGN.md §5, §11).
- Build, CI and release supply chain.

## Out of scope

- Vulnerabilities in third-party adapters or runtimes (netcoredbg, SharpDbg, debugpy, …): report
  those upstream, and we'll help coordinate.
- Attacks that require already executing code as the same OS user as the daemon.
