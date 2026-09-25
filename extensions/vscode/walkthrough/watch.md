# Watch what the agent does

Run and Debug has two EyeDebugger views. The first time, they are collapsed: expand them.

- **EyeDebugger Activity**: the agent's steps, runs, breakpoints and control changes, newest first. A step shows where it stopped; click it to open that line. **Filter…** hides kinds of entries, **Clear** empties the view.
- **EyeDebugger Clients**: who is in the session, who has control and when each was last seen. Right-click a client to request, take, release or give control.

**Follow the agent** (the eye in the Activity view's title, setting **eyedbg.followAgent**): when the agent's step stops, the editor shows the line without taking your keyboard focus. VS Code may keep showing the frame it last focused: click the top frame in Call Stack to see the agent's variables.
