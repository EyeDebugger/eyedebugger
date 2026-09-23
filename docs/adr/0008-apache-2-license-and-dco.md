---
status: accepted
date: 2026-09-23
decision-makers: Ijat (@ijat)
---

# Apache-2.0 license and DCO

## Context and Problem Statement

EyeDebugger needs a license and a contribution-certification model before accepting outside
contributions. This is the project owner's decision, informed by common OSS practice.

## Decision Drivers

* Inbound contributions and outbound distribution should carry the same, permissive terms.
* Contribution certification should not require an external CLA process or signing tool.
* AI-assisted contributions are expected and need an explicit, honest disclosure convention (see
  the Linux kernel's `Documentation/process/coding-assistants.rst`: "AI agents MUST NOT add
  Signed-off-by tags. Only humans can legally certify the Developer Certificate of Origin (DCO).").

## Considered Options

* Apache-2.0 license with DCO (sign-off) certification.
* Apache-2.0 license with a Contributor License Agreement (CLA).
* MIT license with DCO certification.

## Decision Outcome

Chosen option: "Apache-2.0 with DCO", inbound = outbound. Contributors certify
[DCO 1.1](https://developercertificate.org/) with `Signed-off-by`, enforced by the DCO GitHub App
plus `.github/dco.yml`. Only humans sign off; AI agents never add `Signed-off-by` and instead
disclose assistance with an `Assisted-by:` trailer (`CONTRIBUTING.md`, `AGENTS.md`).

### Consequences

* Good, because Apache-2.0's explicit patent grant and NOTICE mechanism suit a project that expects
  corporate and individual contributors alike; redistributors must keep `NOTICE`.
* Good, because DCO sign-off needs no external tooling or paperwork beyond `git commit -s`.
* Good, because the DCO App skips merge commits and GitHub `Bot` authors (verified in its source),
  so Dependabot's commits pass even though its sign-off email differs from its author email.
* Bad, because the DCO App is a third-party hosted service; an outage blocks the required check
  (mitigated: admins can bypass, and it is installed on this repository only, not the whole org).
* Bad, because contributors keep their own copyright under "The EyeDebugger Authors" attribution,
  which is a lighter promise than a CLA's assignment or broad license grant.

## Pros and Cons of the Options

### Apache-2.0 + DCO

* Good, because of the explicit patent grant, useful for a tool that inspects and executes
  third-party code under debug.
* Good, because DCO sign-off is a one-line `git commit -s`, no external account or signature tool.
* Bad, because DCO enforcement (here, via a third-party GitHub App) still needs a working CI check.

### Apache-2.0 + CLA

* Good, because a CLA can grant broader rights (e.g. license changes) than DCO.
* Bad, because it adds friction (a separate signing flow) for first-time contributors and needs
  either a hosted CLA service or in-repo bot.

### MIT + DCO

* Good, because MIT is shorter and equally permissive for most purposes.
* Bad, because it lacks Apache-2.0's explicit patent grant, which matters more for a tool that
  executes and inspects other people's code.

## More Information

* DCO 1.1: https://developercertificate.org/
* DCO GitHub App: https://github.com/apps/dco
* Linux kernel AI-assistance policy: https://docs.kernel.org/process/coding-assistants.html
* Disclosure convention: `CONTRIBUTING.md` § AI-assisted contributions, `AGENTS.md` § Commits
