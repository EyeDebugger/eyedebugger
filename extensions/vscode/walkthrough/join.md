# Join the agent's session

When an agent runs `eyedbg start` for a program in this workspace, EyeDebugger asks whether to join: **Join** attaches VS Code to the agent's live session, **Ignore** never asks about that session again. Turn the question off with **eyedbg.autoJoin**: `never`.

You can also join any running session with **EyeDebugger: Join Session…**, or with F5 when there is no `launch.json`. Leaving (the stop button) never ends the agent's session.
