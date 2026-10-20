# lazymf

A watchdog that keeps the main AI from forgetting the subagents it launched.

## Language

**Watchdog**:
Keeps track of a Session's Watched jobs and reminds the main AI, once per interval (60 seconds by default, configurable), that they are still alive.
_Avoid_: monitor, watcher, plugin

**Main AI**:
The AI session that launches subagents and decides what to do with them. It is the only one that receives Reminders.
_Avoid_: main agent, orchestrator

**Watched job**:
Anything launched in the background that has not finished yet: a subagent, a background shell command or a workflow run, whether started by the main AI or by its subagents. It joins the list when launched and leaves it on its own when it finishes.
_Avoid_: task, worker, process, watched agent

**Reminder**:
A fixed message to the main AI, one per interval: "you have active background jobs, check them; stop the ones that are not useful". Each one costs a turn. It carries no list and no data: the main AI finds out on its own which jobs they are.
_Avoid_: alert, notice, heartbeat

**Stuck job**:
A Watched job whose output stopped growing: it is alive but doing nothing. The main AI decides that and stops it, not the Watchdog.
_Avoid_: dead, hung

## Relationships

- A **Session** of the **Main AI** has zero or more **Watched jobs**
- The **Watchdog** sends **Reminders** while there is at least one **Watched job**
- The **Main AI** stops a **Stuck job**; the **Watchdog** only reports
- While there is a **Watched job**, the **Session** keeps being reminded: no exceptions, not even for a dev server left running on purpose

## Flagged ambiguities

- "kill the agent when it finishes": an agent that finished no longer exists. Resolved as: a finished agent leaves the list on its own, and a live agent that is not useful is stopped by the main AI.
