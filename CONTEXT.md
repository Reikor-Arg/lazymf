# lazymf

A watchdog that keeps the main AI from forgetting the subagents it launched.

## Language

**Watchdog**:
Keeps track of a Session's Watched jobs and reminds the main AI, on an escalating wait (interval x step: with a 120 s interval, 2, 4, 6, 8... minutes), that they are still alive.
_Avoid_: monitor, watcher, plugin

**Main AI**:
The AI session that launches subagents and decides what to do with them. It is the only one that receives Reminders.
_Avoid_: main agent, orchestrator

**Watched job**:
Anything launched in the background that has not finished yet: a subagent, a background shell command or a workflow run, whether started by the main AI or by its subagents. It joins the list when launched and leaves it on its own when it finishes.
_Avoid_: task, worker, process, watched agent

**Reminder**:
A fixed message to the main AI, sent after an escalating wait (interval x step; the step grows with each delivered Reminder and goes back to 1 when a new job appears), telling it to verify that each background job is actually progressing (last output time, last lines) and to stop and redo the stuck ones, not just confirm they are needed. Each one costs a turn. Below it, one line per live job with how long ago it last wrote output, measured by the Watchdog at no token cost, flagged "likely stuck" past a threshold.
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
