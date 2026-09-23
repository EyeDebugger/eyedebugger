<!--
Title: Conventional Commits, e.g. "feat(cli): add bp ls --mine". It becomes the squash-commit subject.
Every commit needs a human's Signed-off-by (git commit -s). See CONTRIBUTING.md.
-->

## What and why

## How it was tested

## Checklist

- [ ] Every commit is signed off by a human (`git commit -s`); AI agents did not add `Signed-off-by`
- [ ] `task ci` passes locally (or the equivalent commands in CONTRIBUTING.md)
- [ ] Tests added or updated; golden-file changes regenerated with `task test:golden` and reviewed
- [ ] Docs updated where behavior changed (README, docs/, an ADR for architectural decisions)
- [ ] No new dependency, or the description justifies it
- [ ] AI assistance, if any, is disclosed in a commit trailer (`Assisted-by:` or the tool's `Co-authored-by:`)
