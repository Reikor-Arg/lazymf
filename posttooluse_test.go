package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func postRun(t *testing.T, w watcher) string {
	t.Helper()
	var out, errb bytes.Buffer
	in := `{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"wait_agent"}`
	if code := run([]string{"hook", "codex", "--event", "posttooluse", "--interval", "60"}, strings.NewReader(in), &out, &errb, w); code != 0 {
		t.Fatalf("code %d %s", code, errb.String())
	}
	return out.String()
}

func clockAt(w *watcher, base time.Time, secs int) {
	w.now = func() time.Time { return base.Add(time.Duration(secs) * time.Second) }
}

func addJob(t *testing.T, w watcher, id string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := registryAdd(registryPath(w.dir, "codex", "s1"), id, at); err != nil {
		t.Fatal(err)
	}
}

func TestPostToolUseLadder(t *testing.T) {
	w, _ := escWatcher(t, nil)
	t.Setenv("CODEX_HOME", t.TempDir())
	base := time.Now()
	clockAt(&w, base, 0)
	addJob(t, w, "a", base)
	if got := postRun(t, w); got != "" {
		t.Fatalf("too early must be silent: %q", got)
	}
	clockAt(&w, base, 61)
	got := postRun(t, w)
	if !strings.Contains(got, `"hookEventName":"PostToolUse"`) || !strings.Contains(got, "additionalContext") || !strings.Contains(got, "(a)") {
		t.Fatalf("expected additionalContext JSON: %q", got)
	}
	if st := readState(statePath(w.dir, "codex", "s1")); st.Step != 2 || st.Last != base.Add(61*time.Second).Unix() {
		t.Fatalf("state %+v", st)
	}
	clockAt(&w, base, 61+119)
	if got := postRun(t, w); got != "" {
		t.Fatalf("step 2 needs 120s: %q", got)
	}
	clockAt(&w, base, 61+120)
	if got := postRun(t, w); got == "" {
		t.Fatal("step 2 should speak at 120s")
	}
}

func TestPostToolUseNewIDRestarts(t *testing.T) {
	w, _ := escWatcher(t, nil)
	t.Setenv("CODEX_HOME", t.TempDir())
	base := time.Now()
	clockAt(&w, base, 0)
	addJob(t, w, "a", base)
	clockAt(&w, base, 61)
	postRun(t, w)
	clockAt(&w, base, 100)
	addJob(t, w, "b", base.Add(100*time.Second))
	if got := postRun(t, w); got != "" {
		t.Fatalf("new id restarts the wait: %q", got)
	}
	st := readState(statePath(w.dir, "codex", "s1"))
	if st.Step != 1 || st.Since != base.Add(100*time.Second).Unix() || st.Last != 0 {
		t.Fatalf("state %+v", st)
	}
	clockAt(&w, base, 161)
	if got := postRun(t, w); got == "" {
		t.Fatal("should speak 60s after the new id")
	}
}

func TestPostToolUseNothingAlive(t *testing.T) {
	w, _ := escWatcher(t, nil)
	if got := postRun(t, w); got != "" {
		t.Fatalf("no jobs must be silent: %q", got)
	}
}

func TestStopUpdatesLast(t *testing.T) {
	w, _ := escWatcher(t, nil)
	addJob(t, w, "a", time.Now())
	var out, errb bytes.Buffer
	in := `{"session_id":"s1","hook_event_name":"Stop"}`
	if code := run([]string{"hook", "codex", "--interval", "1"}, strings.NewReader(in), &out, &errb, w); code != 0 {
		t.Fatalf("code %d", code)
	}
	if st := readState(statePath(w.dir, "codex", "s1")); st.Last == 0 || st.Since == 0 {
		t.Fatalf("Stop must set last/since: %+v", st)
	}
}
