package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultInterval = 60
	pollStep        = 10 * time.Second
	defaultMessage  = "Reminder: you have active background jobs (subagents, shell tasks or workflows). Check what they are doing; stop any that are not useful."
)

// watcher holds the clock so tests can run without real waiting.
type watcher struct {
	poll  time.Duration
	sleep func(time.Duration)
	dir   string // state directory
}

func defaultWatcher() watcher {
	return watcher{poll: pollStep, sleep: time.Sleep, dir: filepath.Join(os.TempDir(), "lazymf")}
}

type bgTask struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

type hookInput struct {
	SessionID       string   `json:"session_id"`
	SessionIDCamel  string   `json:"sessionId"`
	EventName       string   `json:"hook_event_name"`
	AgentID         string   `json:"agent_id"`
	AgentIDCamel    string   `json:"agentId"`
	BackgroundTasks []bgTask `json:"background_tasks"`
}

var terminalStatuses = map[string]bool{
	"completed": true, "complete": true, "done": true, "finished": true,
	"failed": true, "error": true, "errored": true, "killed": true,
	"stopped": true, "cancelled": true, "canceled": true, "exited": true,
	"terminated": true, "success": true, "succeeded": true, "aborted": true,
}

var watchedTypes = map[string]bool{"subagent": true, "shell": true, "workflow": true}

// liveClaudeJobs counts watched jobs (subagent, shell, workflow) that are not in a terminal state.
func liveClaudeJobs(tasks []bgTask) int {
	n := 0
	for _, t := range tasks {
		if watchedTypes[strings.ToLower(strings.TrimSpace(t.Type))] &&
			!terminalStatuses[strings.ToLower(strings.TrimSpace(t.Status))] {
			n++
		}
	}
	return n
}

// safeID makes a session id safe to use as a file name (no path traversal).
func safeID(id string) string {
	ok := id != "" && len(id) <= 128
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			ok = false
			break
		}
	}
	if ok {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "h" + hex.EncodeToString(sum[:16])
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b)), nil
}

func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer, w watcher) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "lazymf hook: missing tool name")
		return 1
	}
	tool := args[0]
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interval := fs.Int("interval", defaultInterval, "seconds between reminders")
	message := fs.String("message", defaultMessage, "reminder text")
	event := fs.String("event", "", "hook event name (overrides the one in the JSON)")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	if *interval < 1 {
		fmt.Fprintln(stderr, "lazymf hook: --interval must be >= 1")
		return 1
	}
	if tool != "claude" && tool != "codex" && tool != "copilot" {
		fmt.Fprintf(stderr, "lazymf hook: unsupported tool %q\n", tool)
		return 1
	}
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 8<<20)).Decode(&in); err != nil {
		fmt.Fprintf(stderr, "lazymf hook: invalid stdin JSON: %v\n", err)
		return 1
	}
	sid := safeID(firstNonEmpty(in.SessionID, in.SessionIDCamel))
	ev := strings.ToLower(*event)
	if ev == "" {
		ev = strings.ToLower(in.EventName)
	}

	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "lazymf hook: %v\n", err)
		return 1
	}
	reg := registryPath(w.dir, tool, sid)

	switch ev {
	case "subagentstart":
		id := firstNonEmpty(in.AgentID, in.AgentIDCamel)
		if id == "" {
			t, err := newToken()
			if err != nil {
				return 1
			}
			id = t
		}
		if err := registryAdd(reg, id); err != nil {
			fmt.Fprintf(stderr, "lazymf hook: %v\n", err)
			return 1
		}
		return 0
	case "subagentstop":
		id := firstNonEmpty(in.AgentID, in.AgentIDCamel)
		// Copilot gives no id at start, so any entry stands in for the one that ended.
		if err := registryRemove(reg, id, tool == "copilot"); err != nil {
			fmt.Fprintf(stderr, "lazymf hook: %v\n", err)
			return 1
		}
		return 0
	case "stop", "agentstop", "":
	default:
		return 0
	}

	live := func() int {
		if tool == "claude" {
			return liveClaudeJobs(in.BackgroundTasks)
		}
		return registryCount(reg)
	}
	if live() == 0 {
		return 0
	}

	// Deduplication: the newest hook run owns the token; older runs exit silently.
	token, err := newToken()
	if err != nil {
		fmt.Fprintf(stderr, "lazymf hook: %v\n", err)
		return 1
	}
	tokPath := filepath.Join(w.dir, tool+"-"+sid+".token")
	if err := os.WriteFile(tokPath, []byte(token), 0o600); err != nil {
		fmt.Fprintf(stderr, "lazymf hook: %v\n", err)
		return 1
	}
	total := time.Duration(*interval) * time.Second
	for waited := time.Duration(0); waited < total; {
		step := w.poll
		if total-waited < step {
			step = total - waited
		}
		w.sleep(step)
		waited += step
		cur, err := os.ReadFile(tokPath)
		if err != nil || string(cur) != token {
			return 0 // superseded by a newer run
		}
		if tool != "claude" && live() == 0 {
			_ = os.Remove(tokPath)
			return 0 // jobs finished while waiting: stay silent
		}
	}
	if cur, err := os.ReadFile(tokPath); err == nil && string(cur) == token {
		_ = os.Remove(tokPath)
	}

	if tool == "claude" {
		fmt.Fprintln(stderr, *message)
		return 2
	}
	out, err := json.Marshal(map[string]string{"decision": "block", "reason": *message})
	if err != nil {
		return 1
	}
	fmt.Fprintln(stdout, string(out))
	return 0
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
