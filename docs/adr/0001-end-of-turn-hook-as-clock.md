# The Watchdog's clock is each tool's end-of-turn hook

No external process can write into an idle AI session, and faking keystrokes into a terminal or editor breaks as soon as focus or the UI changes. So the Watchdog is not a separate daemon: it hooks into the event fired when the main AI ends its turn.

- **Claude Code**: `Stop` hook with `asyncRewake`. The turn ends normally and the session stays free for the user. The hook keeps running in the background: if `background_tasks` is empty it exits silently (0 tokens); otherwise it waits the interval and exits with code 2, which wakes the AI even when the session is idle. Chosen over blocking `Stop` so the user can keep typing and the 8-consecutive-continuation cap does not apply.
- **Tools without async wake-up** (Codex CLI, GitHub Copilot CLI): the end of the turn is blocked, within each tool's continuation cap. Their stop hook carries no job list, so a per-session registry is kept from their subagent start/stop hooks.

## Consequences

- Waiting is free; each Reminder costs a turn, which pays for the whole prompt prefix. With no live jobs the AI is never spoken to.
- Only tools with an end-of-turn hook that can block or wake, plus a way to know which jobs are alive, are supported. Surveyed on 2026-10-20: Claude Code, Codex CLI and GitHub Copilot CLI are supported. Gemini CLI, Cursor and Amp have a stop hook but no reliable way to detect live jobs. Cline, Roo Code, Windsurf/Devin and opencode have no usable stop hook. Kiro is unverified.
- GitHub Copilot's cap of 8 consecutive continuations is fixed: after 8 unanswered Reminders the session ends anyway.
- Installed with `lazymf install` / `lazymf uninstall`, which detect the tools present and add their hook without overwriting existing configuration.
- Written in Go: one binary per OS, no runtime, published for free on GitHub (Reikor-Arg/lazymf, MIT) so it also installs on servers.
