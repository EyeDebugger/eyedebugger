# EyeDebugger for VS Code

Debug alongside an AI agent. [eyedbg](https://github.com/EyeDebugger/eyedebugger) is a CLI-first
debugger that agents drive from the shell; this extension lets you join the agent's live debug
session in VS Code — see where it stopped, its breakpoints, what it does — and take or ask for
control. Or start a session yourself with F5 and let the agent join you.

## Requirements

- `eyedbg` with editor support (`eyedbg version --json` lists `dap.collab`), on your `PATH` or set
  in `eyedbg.path`.
- The debug adapter for your language, as eyedbg needs it (`eyedbg adapters ls`).

## Join a session

When an agent runs `eyedbg start …`, open **Run and Debug**: the list has a "Join s-7f3k — …"
entry per running session. Or press F5 with no launch configuration, or run **EyeDebugger: Join
Session…**; with several sessions you pick one. In `launch.json`:

```json
{ "type": "eyedbg", "request": "attach", "name": "Join eyedbg session" }
```

Add `"session": "s-7f3k"` to join a given one. Disconnecting leaves the session running.

## Start a session (F5)

```json
{
  "type": "eyedbg",
  "request": "launch",
  "name": "Debug app.py",
  "lang": "python",
  "program": "${workspaceFolder}/app.py",
  "args": ["loop"],
  "stopOnEntry": true
}
```

This runs `eyedbg start` as you and joins the new session; the agent can join it too (`eyedbg
sessions`). Other properties: `project`, `cwd` (relative: to the workspace folder), `env`, `opts`
(language options), `noBuild`, `leasePolicy` (`free`, `handoff`, `human-priority`) and `exceptions`
(`all`, `uncaught`, `none`).
Stopping the debug session stops the eyedbg session.

## Control

Whoever holds a session's control lease drives it: continue, step, pause, set variables. The status
bar shows who holds it (you, the agent, nobody), the lease policy and pending requests; click it
for the actions the policy allows: take control, take it by force, request it, release it, give it
to someone, change the policy. When a step is refused because the agent has control, a notice
offers **Request Control** and **Take Over**. When the agent asks for control, you're asked
whether to give it. After you take control from an agent under the `free` policy (where the
agent's next run or step takes it back), the extension offers to switch the session to
`human-priority`, so the agent has to ask to get it back (`eyedbg.lease.afterTakeOver`).

## The agent's breakpoints

The agent's breakpoints show as red dots, with whose they are and what they do at the end of the
line (`⬥ agent · if total > 3`); hover the line for details. Removing such a red dot only hides it
from your editor. Right-click the line number for:

- **Copy as My Breakpoint** — a breakpoint of your own there, with the same condition.
- **Remove Breakpoint for Everyone…** — removes the agent's breakpoint from the session (after you
  confirm).

**EyeDebugger: Show Activity** lists what other clients do in the session.

## Settings

| Setting | Default | |
|---|---|---|
| `eyedbg.path` | `""` | Absolute path of `eyedbg` (`~/` allowed). Empty: `eyedbg` on `PATH`. User settings only. |
| `eyedbg.clientName` | `""` | You join as `human:NAME`. Empty: your OS user name. User settings only. |
| `eyedbg.lease.afterTakeOver` | `ask` | After taking control from an agent under `free`: `ask`, `humanPriority` or `keep`. |

## Security

- `eyedbg.path` and `eyedbg.clientName` can only be set in user settings, never by a workspace; the
  PATH search skips relative entries and the current directory. The extension runs `eyedbg`
  directly, never through a shell.
- The extension doesn't run in Restricted Mode: starting a session builds and runs the workspace's
  code.
- Text from a session (the agent's conditions, messages, names) is shown as plain text: nothing in
  it becomes a link or markup.

## Known limitations

- Restart re-joins the session; it doesn't restart the program.
- When a step is refused, VS Code shows its own error beside the extension's notice.
