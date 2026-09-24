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
- `c/basic`, `cpp/basic`, `rust/basic`: the lldb-dap e2e apps (`drivers/generic`, `TestLldb*`,
  `TestCppExceptions`, `TestCAttach`); compile with `cc -g -O0 -o app main.c`, `c++ -g -O0 -o app
  main.cpp` or `rustc -g -C opt-level=0 -o app main.rs`, then run as `./app loop|wait` (cpp also
  `throw`). `loop` prints "total 150" (150 = sum of `price(item) = item*10` over `{1,2,3,4,5}`),
  and "env VALUE" when `EYEDBG_SAMPLE` is set (proves `${envList}`, the shape lldb-dap <=19's
  launch `env` needs); `wait` prints "pid N" then sleeps, for attach.
- `go/basic`: the Delve e2e app (`drivers/generic`, `TestGo*`), its own module (`go 1.25`); `go run
  . loop|panic|wait` (`debug` mode builds it from the package directory; `exec` mode runs a binary
  built with `go build -gcflags=all=-N -l -o app .` first). `loop` prints "total 150" and "env
  VALUE" as the others; `panic` panics unhandled (exit 2), for `--exceptions uncaught`; `wait`
  prints "pid N" then sleeps, for attach. `func:main.price` is a function breakpoint target.

`dotnet/console`, `python/basic` and `c/basic` are also used by `internal/e2e`'s CLI-level
end-to-end test (`TestCLI`), which drives `eyedbg`/`eyedbgd` as real binaries against them behind
`EYEDBG_E2E=1`.

Lines the tests look up end in a `// marker: NAME` comment (`# marker: NAME`
in Python), so editing an app doesn't break line numbers in the tests. Tests copy an app to a temporary
directory before building it (`copyApp` in `drivers/dotnet`), so builds never
write here; `bin/`, `obj/` and `__pycache__/` are ignored anyway.
