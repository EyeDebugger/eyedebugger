---
name: eyedbg
description: Debug a running .NET or Python program from the shell with eyedbg — breakpoints, stepping, locals, eval, exception stops, attaching, and debugging a failing test. Use it when a bug survives one or two fix attempts, when you need a value the program really has at runtime, or to see where an exception or a wrong result comes from. Not for compile errors or questions that reading the code answers.
license: Apache-2.0
---

# eyedbg

A CLI debugger that agents and humans share. Every command is a one-shot call; a per-user daemon
keeps the session and starts by itself. `eyedbg help <command>` is authoritative;
`eyedbg help --all` prints every command's help in one read.

## When to reach for it

After a fix or two based on reading the code has failed: stop guessing and look at the real state.
Also to find where an exception starts, or why a test fails.

## The loop

```sh
eyedbg start dotnet --project src/App --bp 'Orders.cs@"var total ="'   # build, run, stop there
eyedbg run-until Orders.cs:42 --if 'i == 3'       # continue to where it matters
eyedbg vars --changed                             # what that move changed
eyedbg eval 'order.Items.Count'
eyedbg next                                       # or step-in, step-out, continue
eyedbg stop                                       # always, when done
```

- Execution commands (`continue`, `next`, `step-in`, `step-out`, `run-until`, `wait`) print where
  the program stopped and the locals that changed. Don't follow them with `eyedbg status`.
- They block up to `--timeout` (default 30s). A timeout is not an error: the program keeps
  running; `eyedbg wait` waits again, `eyedbg pause` breaks in.
- Prefer anchors to line numbers: `FILE@"TEXT"` is the one line containing TEXT (resolved when
  added). Also `FILE:LINE` and `func:NAME`.
- `eyedbg bp add Foo.cs:20 --log 'i={i}'` prints instead of stopping (read it with
  `eyedbg output`); `--hit 3`, `--hit '>=3'`, `--hit %2`; `--if EXPR`.
- `eyedbg bp exceptions uncaught` (or `all`) stops where an exception is thrown; the stop shows its
  type, message and stack.
- `eyedbg eval` refuses expressions that call methods or assign (`SIDE_EFFECTS`); add
  `--allow-side-effects` only when running that code is what you want. `eyedbg set total 0` changes
  a variable.

## Keep output small

- `--budget N` (tokens, default 2000) caps variables; cuts say how to see more
  (`eyedbg vars --expand order.Items`, `--depth 2`).
- `--dump none|changed|locals|stack` picks what a stop prints; `eyedbg output --tail 20`.
- `--json` on any command gives `"schema": 1` output.

## Sharing a session

- Humans may be in the same session, from VS Code or any DAP client (`eyedbg dap -s ID --as
  human:NAME`); their steps and breakpoints show in `eyedbg events`. `-s ID` picks a session.
- Several agents: give each a name, `--as agent:NAME` or `EYEDBG_CLIENT=agent:NAME`.
- Running, stepping and `set` need the control lease; under the default policy you take it by
  acting. `LEASE_HELD` (exit 2) means someone else holds it: don't `eyedbg lease take --force` over
  a human — ask. `eyedbg events --since N` shows what others did.
- Breakpoints are yours: `eyedbg bp ls --mine`; `eyedbg bp rm all` removes only yours.

## .NET

- Once per machine: `eyedbg adapters install netcoredbg`; check with `eyedbg adapters doctor`.
- `eyedbg start dotnet` builds the project in the current directory (`--project P`,
  `--program app.dll --no-build`). Program arguments go after `--`.
- A failing test: `eyedbg test dotnet 'Adds' --bp 'CalculatorTests.cs@"Assert.Equal"'` (VSTest;
  ends with `dotnet test`'s exit code). xUnit v3 is refused: run the test app with
  `eyedbg start dotnet --project tests --bp ... -- -method '*Adds'`.
- `eyedbg attach dotnet --pid N` (your own processes); `eyedbg stop` then detaches.
- Eval can't run lambdas or LINQ; property getters run anyway. `$exception` at an exception stop.
  An unhandled exception always stops. No .NET on Intel Macs or Windows on Arm.

## Python

- `eyedbg adapters install debugpy` once (or have debugpy in your environment).
- `eyedbg start python --program app.py -- ARGS`; a module:
  `eyedbg start python --opt module=pytest -- -x tests/test_x.py`.
- The interpreter is `--opt python=PATH`, `EYEDBG_PYTHON`, your active `$VIRTUAL_ENV`, the
  project's `.venv`/`venv` (on Windows only inside your profile; a project elsewhere needs
  `--opt python=PATH`), else `python3`/`python`/`py -3`. It runs where you ran eyedbg.
- `func:NAME` matches the bare function name. `attach` is not supported. Child processes aren't
  debugged.

## Exit codes

- 0 — success, including a wait that timed out (read the output)
- 1 — INVALID_REQUEST, SIDE_EFFECTS, ANCHOR_NOT_FOUND, ANCHOR_AMBIGUOUS
- 2 — NO_SESSION, NOT_STOPPED, NOT_RUNNING, SESSION_EXITED, LEASE_HELD, NOT_OWNER
- 3 — ADAPTER_NOT_INSTALLED, BUILD_FAILED, ATTACH_FAILED, NO_TEST_HOST (run the hint)
- 4 — ADAPTER_ERROR (the expression didn't evaluate), UNSUPPORTED_BY_ADAPTER

Errors print `[CODE]` and a `hint:` line — follow the hint. Finally: `eyedbg stop`.
