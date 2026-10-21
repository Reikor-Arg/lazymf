package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// escWatcher records the total time slept; nothing really waits.
func escWatcher(t *testing.T, onSleep func()) (watcher, *time.Duration) {
	t.Helper()
	var slept time.Duration
	w := watcher{poll: 10 * time.Second, grace: 5 * time.Second, now: time.Now, dir: filepath.Join(t.TempDir(), "state")}
	w.sleep = func(d time.Duration) {
		slept += d
		if onSleep != nil {
			onSleep()
		}
	}
	return w, &slept
}

func tasksJSON(ids ...string) string {
	var p []string
	for _, id := range ids {
		p = append(p, `{"id":"`+id+`","type":"shell","status":"running"}`)
	}
	return `{"session_id":"s1","hook_event_name":"Stop","background_tasks":[` + strings.Join(p, ",") + `]}`
}

// fire runs the claude hook and returns the wait it slept (grace margin excluded) and the exit code.
func fire(t *testing.T, w watcher, slept *time.Duration, in string) (time.Duration, int) {
	t.Helper()
	*slept = 0
	var out, errb bytes.Buffer
	code := run([]string{"hook", "claude", "--interval", "120"}, strings.NewReader(in), &out, &errb, w)
	if code == 2 {
		return *slept - w.grace, code
	}
	return *slept, code
}

func stateOf(w watcher, tool string) reminderState {
	return readState(statePath(w.dir, tool, "s1"))
}

func TestEscalationSameBatch(t *testing.T) {
	w, slept := escWatcher(t, nil)
	for i, want := range []time.Duration{2, 4, 6, 8} {
		got, code := fire(t, w, slept, tasksJSON("a"))
		if code != 2 || got != want*time.Minute {
			t.Fatalf("reminder %d: code=%d wait=%v, want %v", i+1, code, got, want*time.Minute)
		}
		if st := stateOf(w, "claude"); st.Step != i+2 || len(st.IDs) != 1 || st.IDs[0] != "a" {
			t.Fatalf("reminder %d: state %+v", i+1, st)
		}
	}
}

func TestEscalationNewIDResets(t *testing.T) {
	w, slept := escWatcher(t, nil)
	fire(t, w, slept, tasksJSON("a"))
	fire(t, w, slept, tasksJSON("a"))
	if got, _ := fire(t, w, slept, tasksJSON("a", "b")); got != 2*time.Minute {
		t.Fatalf("new id must reset to step 1, waited %v", got)
	}
	if st := stateOf(w, "claude"); st.Step != 2 || len(st.IDs) != 2 {
		t.Fatalf("state %+v", st)
	}
}

func TestEscalationFinishedJobKeepsLadder(t *testing.T) {
	w, slept := escWatcher(t, nil)
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeState(statePath(w.dir, "claude", "s1"), reminderState{IDs: []string{"a", "b"}, Step: 3}); err != nil {
		t.Fatal(err)
	}
	if got, _ := fire(t, w, slept, tasksJSON("a")); got != 6*time.Minute {
		t.Fatalf("finished job without a new one must keep the ladder, waited %v", got)
	}
	if st := stateOf(w, "claude"); st.Step != 4 || len(st.IDs) != 1 {
		t.Fatalf("state %+v", st)
	}
}

func TestEscalationBadStateIsStepOne(t *testing.T) {
	w, slept := escWatcher(t, nil)
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(w.dir, "claude", "s1"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := fire(t, w, slept, tasksJSON("a")); got != 2*time.Minute {
		t.Fatalf("waited %v", got)
	}
}

func TestEscalationSilentHookLeavesStateAlone(t *testing.T) {
	w, slept := escWatcher(t, nil)
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp := statePath(w.dir, "claude", "s1")
	before := reminderState{IDs: []string{"a"}, Step: 3}
	if err := writeState(sp, before); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(sp)
	// Nothing live at start.
	if _, code := fire(t, w, slept, tasksJSON()); code != 0 {
		t.Fatalf("code=%d", code)
	}
	// Token replaced while waiting.
	w.sleep = func(time.Duration) {
		_ = os.WriteFile(filepath.Join(w.dir, "claude-s1.token"), []byte("newer"), 0o600)
	}
	if _, code := fire(t, w, slept, tasksJSON("a")); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if now, _ := os.ReadFile(sp); !bytes.Equal(now, raw) {
		t.Fatalf("state changed: %s -> %s", raw, now)
	}
}

func TestEscalationNothingAliveAtSpeakingDeletesState(t *testing.T) {
	w, slept := escWatcher(t, nil)
	// Claude: the only subagent is marked finished in its meta.json.
	proj := t.TempDir()
	tr := filepath.Join(proj, "s1.jsonl")
	sub := filepath.Join(proj, "s1", "subagents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "agent-a.meta.json"), []byte(`{"finished":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp := statePath(w.dir, "claude", "s1")
	_ = writeState(sp, reminderState{IDs: []string{"a"}, Step: 4})
	in := `{"session_id":"s1","transcript_path":"` + filepath.ToSlash(tr) + `","background_tasks":[{"id":"a","type":"subagent","status":"running"}]}`
	if _, code := fire(t, w, slept, in); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if _, err := os.Stat(sp); err == nil {
		t.Fatal("state must be deleted")
	}

	// Codex: the registry empties while the hook waits.
	w2, _ := escWatcher(t, nil)
	reg := registryPath(w2.dir, "codex", "s1")
	var out, errb bytes.Buffer
	run([]string{"hook", "codex"}, strings.NewReader(`{"session_id":"s1","hook_event_name":"SubagentStart","agent_id":"x"}`), &out, &errb, w2)
	_ = writeState(statePath(w2.dir, "codex", "s1"), reminderState{IDs: []string{"x"}, Step: 3})
	w2.sleep = func(time.Duration) { _ = registryRemove(reg, "x", false) }
	run([]string{"hook", "codex", "--interval", "60"}, strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`), &out, &errb, w2)
	if _, err := os.Stat(statePath(w2.dir, "codex", "s1")); err == nil || out.Len() != 0 {
		t.Fatalf("codex state must be deleted and the hook silent: %q", out.String())
	}
}

func TestEscalationRegistryTools(t *testing.T) {
	for _, tool := range []string{"codex", "copilot"} {
		w, slept := escWatcher(t, nil)
		var out, errb bytes.Buffer
		run([]string{"hook", tool}, strings.NewReader(`{"session_id":"s1","sessionId":"s1","hook_event_name":"SubagentStart","agent_id":"x"}`), &out, &errb, w)
		for i, want := range []time.Duration{60 * time.Second, 120 * time.Second} {
			*slept = 0
			out.Reset()
			run([]string{"hook", tool, "--interval", "60"}, strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`), &out, &errb, w)
			if *slept != want || !strings.Contains(out.String(), `"block"`) {
				t.Fatalf("%s reminder %d: waited %v, out=%q", tool, i+1, *slept, out.String())
			}
		}
		if st := stateOf(w, tool); st.Step != 3 || len(st.IDs) != 1 || st.IDs[0] != "x" {
			t.Fatalf("%s state %+v", tool, st)
		}
	}
}

func TestInstallLongTimeoutAndUpdate(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"\"/x/lazymf\" hook claude --interval 120","timeout":180,"asyncRewake":true}]}]}}`
	if err := os.WriteFile(settings, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".codex", ".copilot"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	if code := runInstall([]string{"--home", home, "--interval", "120", "--mode", "rewake"}, &out, &errb, true); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	for _, p := range []string{settings, filepath.Join(home, ".codex", "hooks.json"), copilotFile(home)} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, `"timeout": 180`) || strings.Count(s, "86400") < 1 {
			t.Fatalf("%s: timeout not updated: %s", p, s)
		}
	}
}
