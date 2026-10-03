// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import "github.com/spf13/cobra"

// newGuide returns a runnable "guide" command: it prints its own help and
// nothing else, has no session or filesystem effect, and exits 0 (an
// unknown subcommand under it, or an unknown language passed to it, exits 1
// through cobra's own "unknown command" handling).
func newGuide(use, short, long, example string, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use:                   use,
		Aliases:               aliases,
		Short:                 short,
		Long:                  long,
		Example:               example,
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE:                  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
}

// guideFooter is appended to every guide's Long text.
const guideFooter = "\n\nPrints this guide only: no effect on any session, never blocks; exits 0 (an unknown language exits 1)."

const vscodeLong = `How to use EyeDebugger from VS Code: watching or joining an agent's live eyedbg session, and
starting your own that an agent can join.

The extension is real and first-party: EyeDebugger, id eyedebugger.eyedbg, publisher eyedebugger,
debug type "eyedbg"; needs VS Code >= 1.100. It is not on any marketplace yet (pre-release only):
install its VSIX from a GitHub release, eyedebugger_<version>_vscode.vsix (alongside the CLI
archives and checksums.txt), with 'code --install-extension eyedebugger_<version>_vscode.vsix';
verify it first with 'gh attestation verify eyedebugger_<version>_vscode.vsix -R
EyeDebugger/eyedebugger'. It needs the eyedbg CLI itself, found on PATH (eyedbg.exe on Windows) or
at eyedbg.path in your user settings (a leading ~/ is your home directory; machine-scoped, so it
can't come from a workspace's settings).

Joining an agent's session: with eyedbg.autoJoin (default ask), the extension offers to join when
an agent starts debugging in this workspace (it polls 'eyedbg sessions --json' every 5s while the
window has focus, and never starts the daemon itself); autoJoin never asks twice for the same
session. Anytime, "EyeDebugger: Join Session…" (eyedbg.joinSession) or an attach configuration
join one by hand: {"type": "eyedbg", "request": "attach", "session": "s-7f3k"} (session omitted
picks from a list; the only session is picked without asking). Joining runs 'eyedbg dap
--as=human:NAME' as the debug adapter (NAME is eyedbg.clientName, default your OS user name).

Starting your own session (F5): a launch configuration's attributes, one per line: lang (required:
dotnet, python, go, c, cpp, rust, or a language your own adapter manifest adds, see 'eyedbg help
lang'), program (an already-built program; for dotnet a .dll or apphost, no build), project (a
dotnet project file or directory to build first), args (the program's arguments), cwd (relative
paths resolve against the workspace folder), env, stopOnEntry, noBuild (needs program), opts
(language options, as 'eyedbg start --opt NAME=VALUE'), leasePolicy (free, handoff or
human-priority; default free), exceptions (all, uncaught or none), adapter (dotnet only:
netcoredbg or sharpdbg), console (internalConsole, the default, or integratedTerminal; python
only for now, needs an eyedbg with dap.terminal: the program runs in a VS Code terminal, started
directly with no shell, for typing its input). Unknown keys (e.g. serverReadyAction,
preLaunchTask) are kept in the launch sent to eyedbg, which ignores them; VS Code acts on the ones
it understands itself. An agent can join a session you launched this way too.

Watching what happens: the EyeDebugger Activity view (Run and Debug) shows every client's steps,
breakpoints and control changes (eyedbg.activity.hide filters kinds); Clients shows who is in the
session and who holds control; the editor follows the agent's stops without taking your keyboard
focus (eyedbg.followAgent), keeping the frame you last focused until you click the top frame in
Call Stack.

Taking or giving control: the control lease decides who may run, step or pause the program
('eyedbg help lease'). From the editor: "EyeDebugger: Control…" (eyedbg.lease.menu) opens a menu of
"Take Control", "Request Control…", "Release Control", "Give Control to…", "Change Lease
Policy…" (also each on their own, on a debug session: eyedbg.lease.take, .request, .release,
.grant, .policy); in the Clients view, the same per client, inline. eyedbg.lease.afterTakeOver
(default ask) offers to switch a free-policy session to human-priority once you take control from
an agent, so it has to ask before taking it back. From the agent side: 'eyedbg lease request
--message "..."' asks; 'eyedbg lease grant human:NAME' hands it over; 'eyedbg events --wait --kind
lease' waits for it to change.

serverReadyAction (VS Code's own launch attribute, forwarded untouched) opens a browser once a
pattern matches the program's output; verified working through eyedbg for a Kestrel ASP.NET app
and a Flask app:

  -- launch.json: dotnet web app, open the browser once it's listening
  {
    "type": "eyedbg",
    "request": "launch",
    "name": "eyedbg: dotnet web",
    "lang": "dotnet",
    "project": "${workspaceFolder}/dotnetweb.csproj",
    "serverReadyAction": {
      "action": "openExternally",
      "pattern": "\\bNow listening on:\\s+(https?://\\S+)"
    }
  }

  -- launch.json: Flask app, open the browser once it's running
  {
    "type": "eyedbg",
    "request": "launch",
    "name": "eyedbg: flask app",
    "lang": "python",
    "opts": { "module": "flask" },
    "args": ["run", "--no-reload"],
    "serverReadyAction": {
      "action": "openExternally",
      "pattern": "Running on (https?://\\S+)"
    }
  }

Common mistakes: picking another extension's debug type instead of "eyedbg"; attach takes a
session, not a pid ('eyedbg attach <lang> --pid N' on the agent's side creates the session, then
join it; there is no pid attach from the editor); lang missing from a launch configuration
(INVALID_REQUEST); a relative program or project that isn't resolved against the workspace (use
${workspaceFolder}/...); no preLaunchTask is needed to build a dotnet project (project builds it
for you); console: integratedTerminal only works for python; an adapter that isn't installed yet
('eyedbg adapters install ...'); Stop ends a session you launched, but only disconnects (never
ends) one you joined; VS Code has no test launch for eyedbg — run 'eyedbg test dotnet ...' from
the agent's side and join it instead.

See 'eyedbg help lang' for what each language needs and its own launch.json shape.`

const vscodeExample = `  code --install-extension eyedebugger_<version>_vscode.vsix
  gh attestation verify eyedebugger_<version>_vscode.vsix -R EyeDebugger/eyedebugger

  -- launch.json: join a running eyedbg session (the picker asks if there is more than one)
  {
    "type": "eyedbg",
    "request": "attach",
    "name": "Join eyedbg session"
  }

  -- launch.json: launch and stop at the first line
  {
    "type": "eyedbg",
    "request": "launch",
    "name": "app.py (eyedbg)",
    "lang": "python",
    "program": "${workspaceFolder}/app.py",
    "stopOnEntry": true
  }

  eyedbg lease request --message "let me step through parse()"   # ask VS Code's user for control
  eyedbg events --wait --kind lease                              # wait for the answer`

func newVSCodeGuide() *cobra.Command {
	return newGuide("vscode", "How to use the EyeDebugger VS Code extension", vscodeLong+guideFooter, vscodeExample)
}

const langLong = `Which languages eyedbg debugs, what each needs installed, and where its own guide is.

  LANGUAGE  ADAPTER              INSTALL                            GUIDE
  dotnet    netcoredbg/SharpDbg  eyedbg adapters install netcoredbg  eyedbg help lang dotnet
  python    debugpy              eyedbg adapters install python      eyedbg help lang python
  go        Delve                eyedbg adapters install delve       eyedbg help lang go
  c         lldb-dap             your OS's LLVM/Clang package        eyedbg help lang native
  cpp       lldb-dap             your OS's LLVM/Clang package        eyedbg help lang native
  rust      lldb-dap             your OS's LLVM/Clang package        eyedbg help lang native

'eyedbg adapters ls' lists these (and any you added) with their versions and options; 'eyedbg
adapters doctor' checks what is actually usable on this machine.

Not supported: Android, iOS and .NET MAUI mobile targets, WebAssembly/Blazor WASM, JavaScript,
TypeScript and Node.js, Java and Kotlin, Ruby, PHP, and a process on another machine — use that
platform's own debugger. Your own adapter manifest can add a DAP adapter for a language eyedbg
doesn't bundle (docs/adapter-manifests.md); once loaded it works the same way, through 'eyedbg
start <language>', 'eyedbg adapters install' and 'eyedbg adapters doctor'.`

const langExample = `  eyedbg adapters ls
  eyedbg adapters doctor
  eyedbg help lang dotnet`

func newLangCommand() *cobra.Command {
	lang := newGuide("lang", "Which languages eyedbg debugs, and their own guides", langLong+guideFooter, langExample)

	lang.AddCommand(
		newGuide("dotnet", "Debug a .NET program with eyedbg", dotnetGuideLong+guideFooter, dotnetGuideExample, "csharp", "fsharp"),
		newGuide("python", "Debug a Python program with eyedbg", pythonGuideLong+guideFooter, pythonGuideExample, "py"),
		newGuide("go", "Debug a Go program with eyedbg", goGuideLong+guideFooter, goGuideExample, "golang"),
		newGuide("native", "Debug a C, C++ or Rust program with eyedbg", nativeGuideLong+guideFooter, nativeGuideExample, "c", "cpp", "rust"),
	)

	return lang
}

const dotnetGuideLong = `Debug a .NET program: console app, ASP.NET, tests or an already-running process, via netcoredbg
(the default: install once with 'eyedbg adapters install netcoredbg') or SharpDbg (opt-in,
--adapter sharpdbg; the default instead where netcoredbg has no build, currently Intel Macs). Under
SharpDbg, pause is refused and both set and eval run as full expressions; see 'eyedbg help
adapters install' for the licensing note.

Console app: --project builds a project file or a directory with exactly one project (default the
current directory) in Debug; --program instead runs an already-built .dll or apphost, no build.

ASP.NET: same as any console app (dotnet's Kestrel web server is just another console program to
eyedbg). It ignores launchSettings.json: without an explicit ASPNETCORE_ENVIRONMENT or
ASPNETCORE_URLS in --env (or launch.json's env), it runs as Production on Kestrel's own default
address. A launch.json serverReadyAction (VS Code) can open a browser once it is listening; see
'eyedbg help vscode'.

Tests: 'eyedbg test dotnet' runs the project's tests under the debugger (VSTest attaches to the
test host it starts; Microsoft.Testing.Platform and xUnit v3 projects are launched directly, like
'eyedbg start'); see 'eyedbg help test' for filters and framework selection.

Attach: 'eyedbg attach dotnet --pid N' debugs a .NET program that is already running (its runtime
must have started, and it must be yours, not already under a debugger); an agent typically does
this, then a human joins from VS Code with an attach launch.json ('eyedbg help vscode').

Containers: 'eyedbg attach dotnet --container NAME' debugs a .NET process in a running Linux
docker container with the container's own netcoredbg (install it for the image's architecture
first: 'eyedbg adapters install netcoredbg --platform linux/arm64', or linux/amd64), and 'eyedbg
compose attach|launch|wait|bp|stop' debugs every .NET service of a compose stack as one group (see
'eyedbg help compose'). In verified runs, line breakpoints did not bind when attached to a Release
image: 'eyedbg compose launch' builds Debug on this machine and launches the app under the debugger in its
container (it recreates the containers it names, and the service runs only while its session
does).

Beyond stepping and variables, 'eyedbg help dotnet' covers runtime counters, heap dumps, thread
captures and CPU traces, and the extension's own .NET views (Counters, Memory, Threads, CPU Trace)
show them live while you join a session.`

const dotnetGuideExample = `  eyedbg start dotnet --bp Program.cs:12                    # build ./, stop at line 12
  eyedbg start dotnet --project src/Api/Api.csproj --env ASPNETCORE_URLS=http://localhost:5050
  eyedbg test dotnet Adds --bp 'CalculatorTests.cs@"Assert.Equal"'
  eyedbg attach dotnet --pid 4242
  eyedbg attach dotnet --container myapp-producer-1         # a .NET process in a docker container
  eyedbg compose attach                                     # every .NET service of the stack in ./

  -- launch.json: build and launch a .NET project, stop at the first line
  {
    "type": "eyedbg",
    "request": "launch",
    "name": ".NET project (eyedbg)",
    "lang": "dotnet",
    "project": "${workspaceFolder}",
    "stopOnEntry": true
  }

  -- launch.json: join a session an agent started with 'eyedbg attach dotnet --pid N'
  {
    "type": "eyedbg",
    "request": "attach",
    "name": "Join eyedbg session"
  }`

const pythonGuideLong = `Debug a Python program via debugpy (install once with 'eyedbg adapters install python', or use
your interpreter's own debugpy). No pid attach yet (UNSUPPORTED_BY_ADAPTER: debugpy needs gdb or
lldb for that): start the program instead, or join a session someone else started.

Script or module: --program runs a script; --opt module=NAME runs a module like 'python -m NAME'
(e.g. pytest, flask, or manage.py's package). Everything after "--" is the program's own arguments.

Interpreter: --opt python=PATH picks it (relative to the current directory), else EYEDBG_PYTHON,
else your active $VIRTUAL_ENV, else a .venv or venv near the working directory or the program,
else python3, python or 'py -3' on PATH; it needs 3.10+. --opt justMyCode=false also stops and
steps into library code.

Flask: --opt module=flask, args ["run", "--no-reload"] (the reloader's child process isn't
debugged, hence --no-reload); a serverReadyAction opens the browser once it's listening ('eyedbg
help vscode'). Django: --program manage.py, args ["runserver", "--noreload"], the same reason.

Tests: pytest runs as a module: 'eyedbg start python --opt module=pytest --bp
tests/test_x.py:8 -- -x tests/test_x.py' (there is no 'eyedbg test' for python).

A program that reads input (input(), an interactive prompt) needs a real terminal: from VS Code,
launch.json's console: integratedTerminal runs it in one (python only for now); see 'eyedbg help
vscode'.`

const pythonGuideExample = `  eyedbg start python --program app.py --bp app.py:12
  eyedbg start python --opt module=pytest --bp tests/test_x.py:8 -- -x tests/test_x.py
  eyedbg start python --opt module=flask --bp app.py:7 -- run --no-reload
  eyedbg start python --program manage.py --bp myapp/views.py:20 -- runserver --noreload

  -- launch.json: launch a script and stop at the first line
  {
    "type": "eyedbg",
    "request": "launch",
    "name": "app.py (eyedbg)",
    "lang": "python",
    "program": "${workspaceFolder}/app.py",
    "stopOnEntry": true
  }

  -- launch.json: join a session someone else started (e.g. an agent's)
  {
    "type": "eyedbg",
    "request": "attach",
    "name": "Join eyedbg session"
  }`

const goGuideLong = `Debug a Go program via Delve (install once with 'eyedbg adapters install delve', or dlv on PATH
or EYEDBG_DLV): eyedbg drives it over a private connection it dials in to, not stdio
(docs/adapter-manifests.md § Transports).

--program takes a package directory or a .go file; --opt mode chooses how: debug (the default)
builds and runs it with Delve itself; exec instead takes an already-built binary in --program
(build it yourself first, 'go build -gcflags="all=-N -l"', so breakpoints and locals stay
reliable); test runs the package's tests in --cwd, not --program's directory. --opt
buildFlags='FLAGS' adds extra 'go build' flags (e.g. -tags=integration). --cwd also sets where a
debug-mode build runs.

Attach: 'eyedbg attach go --pid N' debugs an already-running Go program (yours, subject to the
OS's own attach restrictions: Linux ptrace_scope, macOS task_for_pid); join it from VS Code with an
attach launch.json ('eyedbg help vscode').`

const goGuideExample = `  eyedbg start go --program . --cwd . --bp main.go:12
  eyedbg start go --program ./bin/app --opt mode=exec --bp main.go:12
  eyedbg start go --program . --opt mode=test --bp handler_test.go:9
  eyedbg attach go --pid 4242

  -- launch.json: build and launch a Go package, stop at the first line
  {
    "type": "eyedbg",
    "request": "launch",
    "name": "Go package (eyedbg)",
    "lang": "go",
    "program": "${workspaceFolder}",
    "cwd": "${workspaceFolder}",
    "stopOnEntry": true
  }

  -- launch.json: run a package's tests, stop at the first line
  {
    "type": "eyedbg",
    "request": "launch",
    "name": "Go tests (eyedbg)",
    "lang": "go",
    "program": "${workspaceFolder}/pkg",
    "cwd": "${workspaceFolder}/pkg",
    "opts": { "mode": "test" },
    "stopOnEntry": true
  }

  -- launch.json: join a session an agent started with 'eyedbg attach go --pid N'
  {
    "type": "eyedbg",
    "request": "attach",
    "name": "Join eyedbg session"
  }`

const nativeGuideLong = `Debug a C, C++ or Rust program via LLVM's lldb-dap (your OS's LLVM/Clang package; found on PATH,
or set EYEDBG_LLDB_DAP). Nothing is built for you: compile with debug info first, e.g. 'cc -g -O0'
(C), 'c++ -g -O0' (C++) or 'rustc -g' / 'cargo build' (Rust), then --program the resulting binary.

Rust's String, Vec and HashMap show raw layouts unless LLDB's Rust formatters are imported (two
lines in ~/.lldbinit, from 'rustc --print sysroot'; docs/adapter-manifests.md has the exact lines).
If the binary was built under a symlinked directory (macOS's /tmp or /var is one), lldb-dap's
breakpoints won't bind: build outside a symlinked path, or resolve it first, e.g. 'cd "$(cd -P
dir && pwd)"'.

C++ exceptions: --exceptions all also stops on a throw, not just an uncaught one (see 'eyedbg bp
exceptions').

On Linux, --stop-on-entry (or a launch.json's stopOnEntry) shows as "exception (signal SIGSTOP)",
not "entry": lldb-dap's own way of pausing at the start, harmless; step or continue as usual.

Attach: 'eyedbg attach c|cpp|rust --pid N' debugs an already-running native process (yours, subject
to the OS's own attach restrictions: Linux ptrace_scope, macOS task_for_pid); join it from VS Code
with an attach launch.json ('eyedbg help vscode').`

const nativeGuideExample = `  cc -g -O0 -o bin/app main.c                     # compile with debug info first
  eyedbg start c --program ./bin/app --bp main.c:12
  cargo build                                        # compile with debug info first
  eyedbg start rust --program target/debug/app --bp src/main.rs:8
  eyedbg start cpp --program ./bin/app --bp main.cpp:20 --exceptions all
  eyedbg attach cpp --pid 4242

  -- launch.json: launch a built native binary, stop at the first line
  {
    "type": "eyedbg",
    "request": "launch",
    "name": "app (eyedbg)",
    "lang": "rust",
    "program": "${workspaceFolder}/target/debug/app",
    "stopOnEntry": true
  }

  -- launch.json: join a session an agent started with 'eyedbg attach rust --pid N'
  {
    "type": "eyedbg",
    "request": "attach",
    "name": "Join eyedbg session"
  }`
