# testdata/apps

Sample debuggee programs, one directory per language, used by the end-to-end
tests (docs/DESIGN.md §12-13). Go tooling ignores `testdata/`.

- `dotnet/console`: the loop the M2–M4 tests debug (`TestDebugConsoleApp`,
  `TestTwoClients`).
- `dotnet/breadth`: breakpoint kinds, exceptions and attaching; run it as
  `dotnet breadth.dll loop|throw|crash|wait MARKER`.
- `dotnet/tests`: an xunit v2 test project (VSTest) for `eyedbg test`; `Adds`
  passes and `Fails` fails. Restoring it needs network access to NuGet
  (Microsoft.NET.Test.Sdk, xunit, xunit.runner.visualstudio).
- `python/basic`: the Python e2e app (`drivers/generic`, `TestPython*`); run it
  as `python app.py loop|raise|child|wait`.

`dotnet/console` and `python/basic` are also used by `internal/e2e`'s CLI-level end-to-end test
(`TestCLI`), which drives `eyedbg`/`eyedbgd` as real binaries against them behind `EYEDBG_E2E=1`.

Lines the tests look up end in a `// marker: NAME` comment (`# marker: NAME`
in Python), so editing an app doesn't break line numbers in the tests. Tests copy an app to a temporary
directory before building it (`copyApp` in `drivers/dotnet`), so builds never
write here; `bin/`, `obj/` and `__pycache__/` are ignored anyway.
