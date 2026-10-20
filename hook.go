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
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	defaultInterval = 60
	pollStep        = 10 * time.Second
	graceStep       = 5 * time.Second
	defaultStuck    = 5 // minutes without output before a job is flagged
	maxJobLines     = 20
	defaultMaxAge   = 60 // minutes before a registry entry is considered dead
	defaultMessage  = "lazymf: background jobs (subagents, shell tasks or workflows) are still running. Do not just confirm they are needed - verify each one is actually progressing: check the last-modified time of its output file or transcript and read only its last few lines. A job with no new output for several minutes, or waiting for something that will never arrive (a notification, a background test run), is stuck: stop it and finish or relaunch the work yourself. Then report each job as progressing or stuck/stopped."
)

// watcher holds the clock so tests can run without real waiting.
type watcher struct {
	poll  time.Duration
	sleep func(time.Duration)
	dir   string // state directory
	grace time.Duration
	now   func() time.Time
}

func defaultWatcher() watcher {
	// Per-user dir, not the shared temp dir: avoids symlink planting by other users.
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return watcher{poll: pollStep, sleep: time.Sleep, grace: graceStep, now: time.Now, dir: filepath.Join(base, "lazymf")}
}

type bgTask struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description"`
}

type hookInput struct {
	SessionID       string   `json:"session_id"`
	SessionIDCamel  string   `json:"sessionId"`
	TranscriptPath  string   `json:"transcript_path"`
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
	maxAge := fs.Int("max-age", defaultMaxAge, "minutes after which a registry entry is dropped")
	stuck := fs.Int("stuck", defaultStuck, "minutes without output after which a job is flagged as likely stuck")
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
	if *stuck < 1 {
		fmt.Fprintln(stderr, "lazymf hook: --stuck must be >= 1")
		return 1
	}
	if *maxAge < 1 {
		fmt.Fprintln(stderr, "lazymf hook: --max-age must be >= 1")
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
		if err := registryAdd(reg, id, w.now()); err != nil {
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
		return registryLive(reg, w.now(), time.Duration(*maxAge)*time.Minute)
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
	if tool == "claude" {
		// Grace margin: new activity means a newer Stop will decide with fresh data.
		w.sleep(w.grace)
		if cur, err := os.ReadFile(tokPath); err != nil || string(cur) != token {
			return 0
		}
	}
	if cur, err := os.ReadFile(tokPath); err == nil && string(cur) == token {
		_ = os.Remove(tokPath)
	}

	if tool == "claude" {
		fmt.Fprintln(stderr, *message+jobList(in, w.now(), time.Duration(*stuck)*time.Minute))
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

var jobIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// lastActivity returns the mtime of the job's output file, or false if unknown.
// Ids that do not match jobIDRe never build a path.
func lastActivity(in hookInput, t bgTask) (time.Time, bool) {
	sid := firstNonEmpty(in.SessionID, in.SessionIDCamel)
	if in.TranscriptPath == "" || !jobIDRe.MatchString(t.ID) || !jobIDRe.MatchString(sid) || safeID(sid) != sid {
		return time.Time{}, false
	}
	tdir := filepath.Dir(in.TranscriptPath)
	paths := []string{filepath.Join(tdir, sid, "subagents", "agent-"+t.ID+".jsonl")}
	if strings.ToLower(strings.TrimSpace(t.Type)) == "shell" {
		paths = append([]string{filepath.Join(os.TempDir(), "claude", filepath.Base(tdir), sid, "tasks", t.ID+".output")}, paths...)
	}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			return fi.ModTime(), true
		}
	}
	return time.Time{}, false
}

// oneLine strips control characters and cuts the text to n runes.
func oneLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

// jobList returns one line per live job with its time since last output.
// Any failure yields "" so the reminder is never lost because of the list.
func jobList(in hookInput, now time.Time, stuck time.Duration) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	if in.TranscriptPath == "" {
		return ""
	}
	var lines []string
	live := 0
	for _, t := range in.BackgroundTasks {
		typ := strings.ToLower(strings.TrimSpace(t.Type))
		if !watchedTypes[typ] || terminalStatuses[strings.ToLower(strings.TrimSpace(t.Status))] {
			continue
		}
		live++
		if live > maxJobLines {
			continue
		}
		id := "unknown"
		if jobIDRe.MatchString(t.ID) {
			id = t.ID
			if len(id) > 8 {
				id = id[:8]
			}
		}
		line := fmt.Sprintf("- %s %q (%s): ", oneLine(typ, 20), oneLine(t.Description, 80), id)
		if mt, ok := lastActivity(in, t); ok {
			idle := now.Sub(mt)
			if idle < 0 {
				idle = 0
			}
			line += fmt.Sprintf("last output %d min ago", int(idle/time.Minute))
			if idle >= stuck {
				line += " - likely stuck"
			}
		} else {
			line += "last activity unknown"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	if live > maxJobLines {
		lines = append(lines, fmt.Sprintf("... and %d more", live-maxJobLines))
	}
	return "\n" + strings.Join(lines, "\n")
}
