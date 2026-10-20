# lazymf

**Lazy MotherFucker** — or LazyMF, to keep it polite.

Your AI coding assistant hires lazy agents. They doze off, never report that they finished, and
never say they got stuck. The main AI forgets they exist until you ask about them hours later.
`lazymf` keeps nudging the main AI: *you have agents out there — go check what the hell they are
doing.*

When the main AI session launches subagents, background shell commands or workflows and then goes
quiet, nothing tells it they are still running. `lazymf` hooks into the end-of-turn hook of the
tool and, every N seconds (60 by default), sends one fixed reminder:

> lazymf: background jobs (subagents, shell tasks or workflows) are still running. Do not just confirm they are needed - verify each one is actually progressing: check the last-modified time of its output file or transcript and read only its last few lines. A job with no new output for several minutes, or waiting for something that will never arrive (a notification, a background test run), is stuck: stop it and finish or relaunch the work yourself. Then report each job as progressing or stuck/stopped.

The AI decides what to do: look at the jobs, wait, or stop the ones that are stuck. `lazymf`
never kills anything. On Claude Code the reminder also lists each live job with how long ago it last
wrote output, measured by `lazymf` itself (no tokens spent):

```
- subagent "tests compras" (a7ff577f): last output 14 min ago - likely stuck
- shell "sleep 75" (bkjgc247): last output 0 min ago
```

## How it works

- No background daemon. The clock is the tool's own end-of-turn hook.
- **Claude Code**: a `Stop` hook with `asyncRewake`. The turn ends normally and you can keep typing.
  The hook reads `background_tasks` from its stdin. No live subagent/shell/workflow: it exits
  silently. Otherwise it waits the interval (checking every 10 s) and exits with code 2, which wakes
  the AI even if the session is idle. Only the newest hook run per session speaks; older runs notice
  a newer token in `<user cache dir>/lazymf/<session>` and exit silently. Just before speaking it
  waits a short grace margin (5 s) and exits silently only if the token was replaced: that means
  a newer `Stop` will decide with fresh data. When it speaks it appends one line per live job
  (max 20, then `... and N more`): the mtime of the subagent transcript
  (`<session dir>/<session>/subagents/agent-<id>.jsonl`) or of the background shell output file
  (`<temp>/claude/<project>/<session>/tasks/<id>.output`), computed at the moment of speaking. A job
  idle for `--stuck` minutes or more (default 5) is marked `likely stuck`; workflows and jobs
  without a file show `last activity unknown`. Job ids are accepted only if they match
  `^[A-Za-z0-9_-]{1,64}$`; if anything fails, the reminder goes out without the list.
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
lazymf install --tool claude --stuck 10     # flag jobs idle for 10+ min as likely stuck (default 5)
lazymf install --tool codex --max-age 180   # registry entries (Codex, Copilot) expire after 180 min
lazymf install --home /some/home     # configure another home directory
```

`install` detects `~/.claude`, `~/.codex` and `~/.copilot`, adds its hooks using the absolute path of
the binary, keeps your other hooks, does not duplicate itself on re-runs (re-running with another
`--interval` updates it) and saves the first original of each file it edits as `<file>.bak-lazymf`.
Copilot gets its own file, `~/.copilot/hooks/lazymf.json`. Do not move the binary afterwards; run
`install` again if you do.

`--max-age MIN` (default 60) is accepted by `hook` and `install` and only affects Codex and Copilot.

`--stuck MIN` (default 5) is accepted by `hook` and `install` and only affects Claude Code: the minutes
without output after which a job in the reminder list is marked `likely stuck`. `install` writes it
into the hook command only when it is not the default.

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
