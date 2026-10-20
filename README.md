# lazymf

**Lazy MotherFucker** — or LazyMF, to keep it polite.

Your AI coding assistant hires lazy agents. They doze off, never report that they finished, and
never say they got stuck. The main AI forgets they exist until you ask about them hours later.
`lazymf` keeps nudging the main AI: *you have agents out there — go check what the hell they are
doing.*

When the main AI session launches subagents, background shell commands or workflows and then goes
quiet, nothing tells it they are still running. `lazymf` hooks into the end-of-turn hook of the
tool and, every N seconds (60 by default), sends one fixed reminder:

> Reminder: you have active background jobs (subagents, shell tasks or workflows). Check what they are doing; stop any that are not useful.

The AI decides what to do: look at the jobs, wait, or stop the ones that are stuck. `lazymf`
never kills anything and the reminder carries no list.

## How it works

- No background daemon. The clock is the tool's own end-of-turn hook.
- **Claude Code**: a `Stop` hook with `asyncRewake`. The turn ends normally and you can keep typing.
  The hook reads `background_tasks` from its stdin. No live subagent/shell/workflow: it exits
  silently. Otherwise it waits the interval (checking every 10 s) and exits with code 2, which wakes
  the AI even if the session is idle. Only the newest hook run per session speaks; older runs notice
  a newer token in `<user cache dir>/lazymf/<session>` and exit silently. Just before speaking it
  waits a short grace margin (5 s) and exits silently only if the token was replaced: that means
  a newer `Stop` will decide with fresh data.
- **Codex CLI, GitHub Copilot CLI**: the stop hook gives no job list, so `lazymf` keeps a per-session
  registry in the user cache directory (`SubagentStart` adds, `SubagentStop` removes). Each entry
  stores its start time; entries older than `--max-age` minutes (default 60) are dropped and the
  registry is rewritten, so a subagent that dies without firing its stop hook cannot block forever.
  With live entries it
  waits the interval and then blocks the end of the turn with the reminder. If the jobs finish while
  it waits, it stays silent.

Waiting is free. **Each reminder costs one model turn** (the whole context prefix is read again), so
with no live jobs `lazymf` never speaks, and a longer `--interval` means fewer paid turns.

## Supported tools

| Tool | Hook | Live-job source | Status |
|---|---|---|---|
| Claude Code | `Stop` + `asyncRewake` | `background_tasks` in the hook input | Supported |
| Codex CLI | `Stop` / `SubagentStart` / `SubagentStop` | registry file | Supported (see caveats) |
| GitHub Copilot CLI | `agentStop` / `subagentStart` / `subagentStop` | registry file | Supported (see caveats) |
| Gemini CLI | `AfterAgent` | none | Not yet supported: the docs list no subagent lifecycle hook and no job list, so live jobs cannot be detected |
| Cursor | `stop` + `followup_message` | none reliable | Not yet supported: `subagentStop` carries no subagent id nor a session key to match its `subagentStart` |
| Amp | plugin `agent.end` | none | Not supported: the plugin API has no subagent lifecycle event, its `Task` subagents run inside the turn (`tool.call` until `tool.result`), and the only documented background construct is a thread a plugin itself creates; so there is no job list to read and, most likely, nothing alive when the turn ends (not verified against a live install) |
| Cline, Roo Code, Windsurf/Devin, opencode | no usable stop hook | | Not supported |

Caveats, not verified against a live install:

- Codex and Copilot docs do not say whether their subagents run in the background after the turn
  ends. If they run synchronously, the reminder never fires there (harmless).
- Codex requires reviewing/trusting new hooks (`/hooks` in the CLI) before they run.
- Copilot ends the turn anyway after 8 consecutive blocks.
- A subagent that dies without firing its stop hook stays in the registry until `--max-age` expires it.
  A legitimately longer-running subagent is also dropped after that time: raise `--max-age`.

## Install

Download the binary for your system from the
[Releases](https://github.com/Reikor-Arg/lazymf/releases) page (`lazymf-<os>-<arch>`), make it
executable and run:

```
lazymf install                       # every detected tool, 60 s interval
lazymf install --tool claude --interval 120
lazymf install --tool codex --max-age 180   # registry entries (Codex, Copilot) expire after 180 min
lazymf install --home /some/home     # configure another home directory
```

`install` detects `~/.claude`, `~/.codex` and `~/.copilot`, adds its hooks using the absolute path of
the binary, keeps your other hooks, does not duplicate itself on re-runs (re-running with another
`--interval` updates it) and saves the first original of each file it edits as `<file>.bak-lazymf`.
Copilot gets its own file, `~/.copilot/hooks/lazymf.json`. Do not move the binary afterwards; run
`install` again if you do.

`--max-age MIN` (default 60) is accepted by `hook` and `install` and only affects Codex and Copilot.

The reminder text can be changed by adding `--message "..."` to the hook command.

## Uninstall

```
lazymf uninstall [--tool X]
```

Removes only the hooks `lazymf` added (and the Copilot file). Everything else is left as it was.

## Build

```
go test ./... && go build -ldflags "-X main.version=v0.1.0" .
```

Go standard library only. MIT licensed.
