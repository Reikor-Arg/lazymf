package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testWatcher(t *testing.T, onSleep func(n int)) watcher {
	t.Helper()
	n := 0
	return watcher{poll: time.Second, grace: 5 * time.Second, now: time.Now, dir: filepath.Join(t.TempDir(), "state"), sleep: func(time.Duration) {
		n++
		if onSleep != nil {
			onSleep(n)
		}
	}}
}

const liveJSON = `{"session_id":"s1","hook_event_name":"Stop","background_tasks":[{"id":"a","type":"subagent","status":"running"}]}`

func TestClaudeNoLiveJobsIsSilent(t *testing.T) {
	w := testWatcher(t, nil)
	in := `{"session_id":"s1","background_tasks":[{"id":"a","type":"shell","status":"completed"},{"id":"b","type":"monitor","status":"running"}]}`
	var out, errb bytes.Buffer
	if code := run([]string{"hook", "claude", "--interval", "3"}, strings.NewReader(in), &out, &errb, w); code != 0 || errb.Len() != 0 || out.Len() != 0 {
		t.Fatalf("code=%d err=%q out=%q", code, errb.String(), out.String())
	}
}

func TestClaudeReminderExit2(t *testing.T) {
	w := testWatcher(t, nil)
	var out, errb bytes.Buffer
	code := run([]string{"hook", "claude", "--interval", "3"}, strings.NewReader(liveJSON), &out, &errb, w)
	if code != 2 || strings.TrimSpace(errb.String()) != defaultMessage {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestClaudeDedupNewerRunWins(t *testing.T) {
	w := testWatcher(t, nil)
	// A second hook run starts while the first sleeps: it takes over the token.
	w.sleep = func(time.Duration) {
		if _, err := os.Stat(filepath.Join(w.dir, "claude-s1.token")); err == nil {
			_ = os.WriteFile(filepath.Join(w.dir, "claude-s1.token"), []byte("newer"), 0o600)
		}
	}
	var out, errb bytes.Buffer
	code := run([]string{"hook", "claude", "--interval", "5"}, strings.NewReader(liveJSON), &out, &errb, w)
	if code != 0 || errb.Len() != 0 {
		t.Fatalf("superseded run must be silent: code=%d err=%q", code, errb.String())
	}
}

func TestSessionIDSanitized(t *testing.T) {
	w := testWatcher(t, nil)
	in := `{"session_id":"../../evil","background_tasks":[{"type":"shell","status":"running"}]}`
	var out, errb bytes.Buffer
	run([]string{"hook", "claude", "--interval", "1"}, strings.NewReader(in), &out, &errb, w)
	entries, err := os.ReadDir(filepath.Dir(w.dir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("state escaped its directory: %v %v", entries, err)
	}
	if got := safeID("../../evil"); strings.ContainsAny(got, "./\\") {
		t.Fatalf("unsafe id %q", got)
	}
}

func TestRegistryBlockingMode(t *testing.T) {
	w := testWatcher(t, nil)
	start := `{"session_id":"c1","hook_event_name":"SubagentStart","agent_id":"x1"}`
	stop := `{"session_id":"c1","hook_event_name":"Stop"}`
	var out, errb bytes.Buffer
	if code := run([]string{"hook", "codex"}, strings.NewReader(stop), &out, &errb, w); code != 0 || out.Len() != 0 {
		t.Fatalf("no jobs must be silent: %d %q", code, out.String())
	}
	run([]string{"hook", "codex"}, strings.NewReader(start), &out, &errb, w)
	code := run([]string{"hook", "codex", "--interval", "2"}, strings.NewReader(stop), &out, &errb, w)
	var d map[string]string
	if code != 0 || json.Unmarshal(out.Bytes(), &d) != nil || d["decision"] != "block" || d["reason"] != defaultMessage {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
	// Subagent ends: silent again.
	run([]string{"hook", "codex"}, strings.NewReader(`{"session_id":"c1","hook_event_name":"SubagentStop","agent_id":"x1"}`), &out, &errb, w)
	out.Reset()
	run([]string{"hook", "codex"}, strings.NewReader(stop), &out, &errb, w)
	if out.Len() != 0 {
		t.Fatalf("expected silence, got %q", out.String())
	}
}

func TestCopilotRegistryByEventFlag(t *testing.T) {
	w := testWatcher(t, nil)
	var out, errb bytes.Buffer
	run([]string{"hook", "copilot", "--event", "subagentStart"}, strings.NewReader(`{"sessionId":"k"}`), &out, &errb, w)
	if n := registryCount(registryPath(w.dir, "copilot", "k")); n != 1 {
		t.Fatalf("count=%d", n)
	}
	run([]string{"hook", "copilot", "--event", "subagentStop"}, strings.NewReader(`{"sessionId":"k","agentId":"zz"}`), &out, &errb, w)
	if n := registryCount(registryPath(w.dir, "copilot", "k")); n != 0 {
		t.Fatalf("count=%d", n)
	}
}

func TestRegistryConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.jobs")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = registryAdd(p, string(rune('a'+i)), time.Now())
		}(i)
	}
	wg.Wait()
	if n := registryCount(p); n != 20 {
		t.Fatalf("count=%d", n)
	}
}

func TestInstallUninstallIdempotent(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := `{"model":"x","hooks":{"Stop":[{"hooks":[{"type":"command","command":"other.sh"}]}]}}`
	if err := os.WriteFile(settings, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--home", home, "--interval", "30", "--mode", "rewake"}
	var out, errb bytes.Buffer
	if code := runInstall(args, &out, &errb, true); code != 0 {
		t.Fatalf("install: %d %s", code, errb.String())
	}
	first, _ := os.ReadFile(settings)
	if code := runInstall(args, &out, &errb, true); code != 0 {
		t.Fatalf("reinstall: %d", code)
	}
	second, _ := os.ReadFile(settings)
	if !bytes.Equal(first, second) {
		t.Fatal("install is not idempotent")
	}
	s := string(first)
	if strings.Count(s, "lazymf") != 1 && strings.Count(s, " hook claude") != 1 {
		t.Fatalf("duplicated hook: %s", s)
	}
	if !strings.Contains(s, "other.sh") || !strings.Contains(s, `"asyncRewake": true`) || !strings.Contains(s, `"timeout": 86400`) {
		t.Fatalf("unexpected settings: %s", s)
	}
	if bak, err := os.ReadFile(settings + backupSuffix); err != nil || string(bak) != orig {
		t.Fatalf("backup missing or wrong: %v", err)
	}
	codexFile := filepath.Join(home, ".codex", "hooks.json")
	if b, err := os.ReadFile(codexFile); err != nil || strings.Count(string(b), " hook codex") != 4 {
		t.Fatalf("codex hooks: %v %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot")); err == nil {
		t.Fatal("undetected tool must be skipped")
	}

	if code := runInstall([]string{"--home", home}, &out, &errb, false); code != 0 {
		t.Fatalf("uninstall: %d %s", code, errb.String())
	}
	after, _ := os.ReadFile(settings)
	if strings.Contains(string(after), "lazymf") || !strings.Contains(string(after), "other.sh") || !strings.Contains(string(after), `"model"`) {
		t.Fatalf("uninstall touched more or less than its own: %s", after)
	}
	if code := runInstall([]string{"--home", home}, &out, &errb, false); code != 0 {
		t.Fatalf("second uninstall: %d", code)
	}
}

func TestCopilotInstallUninstall(t *testing.T) {
	home := t.TempDir()
	var out, errb bytes.Buffer
	if code := runInstall([]string{"--home", home, "--tool", "copilot"}, &out, &errb, true); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if _, err := os.Stat(copilotFile(home)); err != nil {
		t.Fatal(err)
	}
	if code := runInstall([]string{"--home", home, "--tool", "copilot"}, &out, &errb, false); code != 0 {
		t.Fatal("uninstall failed")
	}
	if _, err := os.Stat(copilotFile(home)); err == nil {
		t.Fatal("file not removed")
	}
}

func TestClaudeSilentWhenSupersededDuringGrace(t *testing.T) {
	w := testWatcher(t, nil)
	w.sleep = func(d time.Duration) {
		if d == w.grace {
			_ = os.WriteFile(filepath.Join(w.dir, "claude-s1.token"), []byte("newer"), 0o600)
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"hook", "claude", "--interval", "3"}, strings.NewReader(liveJSON), &out, &errb, w); code != 0 || errb.Len() != 0 {
		t.Fatalf("must be silent: code=%d err=%q", code, errb.String())
	}
}

func TestRegistryEntriesExpire(t *testing.T) {
	w := testWatcher(t, nil)
	base := time.Now()
	w.now = func() time.Time { return base }
	var out, errb bytes.Buffer
	for i := 0; i < 2; i++ { // Copilot: no id, one stamp per add
		run([]string{"hook", "copilot", "--event", "subagentStart"}, strings.NewReader(`{"sessionId":"k"}`), &out, &errb, w)
	}
	reg := registryPath(w.dir, "copilot", "k")
	if n := registryCount(reg); n != 2 {
		t.Fatalf("count=%d", n)
	}
	w.now = func() time.Time { return base.Add(61 * time.Minute) }
	if code := run([]string{"hook", "copilot", "--event", "agentStop"}, strings.NewReader(`{"sessionId":"k"}`), &out, &errb, w); code != 0 || out.Len() != 0 {
		t.Fatalf("expired entries must be silent: %d %q", code, out.String())
	}
	if n := registryCount(reg); n != 0 {
		t.Fatalf("registry not rewritten: %d", n)
	}
	w.now = func() time.Time { return base }
	run([]string{"hook", "codex", "--event", "subagentStart"}, strings.NewReader(`{"session_id":"c","agent_id":"a"}`), &out, &errb, w)
	cp := registryPath(w.dir, "codex", "c")
	if n := registryLive(cp, base.Add(90*time.Minute), 2*time.Hour); n != 1 {
		t.Fatalf("n=%d", n)
	}
	if n := registryLive(cp, base.Add(90*time.Minute), time.Hour); n != 0 {
		t.Fatalf("n=%d", n)
	}
}

func touch(t *testing.T, p string, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func remind(t *testing.T, w watcher, in string, extra ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	args := append([]string{"hook", "claude", "--interval", "3"}, extra...)
	if code := run(args, strings.NewReader(in), &out, &errb, w); code != 2 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	return errb.String()
}

func TestReminderJobList(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMPDIR", tmp)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w := testWatcher(t, nil)
	w.now = func() time.Time { return now }
	proj := filepath.Join(t.TempDir(), "proj")
	tp := filepath.Join(proj, "s1.jsonl")
	touch(t, filepath.Join(proj, "s1", "subagents", "agent-a7ff577f21f8e4191.jsonl"), now.Add(-30*time.Second))
	touch(t, filepath.Join(proj, "s1", "subagents", "agent-oldagent123.jsonl"), now.Add(-14*time.Minute))
	touch(t, filepath.Join(tmp, "claude", "proj", "s1", "tasks", "bkjgc247c.output"), now)
	in, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": tp, "background_tasks": []map[string]string{
		{"id": "a7ff577f21f8e4191", "type": "subagent", "status": "running", "description": "fresh\nagent"},
		{"id": "oldagent123", "type": "subagent", "status": "running", "description": "tests compras"},
		{"id": "bkjgc247c", "type": "shell", "status": "running", "description": "sleep 75"},
		{"id": "../evil", "type": "subagent", "status": "running", "description": "bad id"},
		{"id": "wf1", "type": "workflow", "status": "running", "description": "wf"},
		{"id": "done1", "type": "shell", "status": "completed", "description": "finished"},
	}})
	got := remind(t, w, string(in))
	want := defaultMessage + "\n" +
		"- subagent \"fresh agent\" (a7ff577f): last output 0 min ago\n" +
		"- subagent \"tests compras\" (oldagent): last output 14 min ago - likely stuck\n" +
		"- shell \"sleep 75\" (bkjgc247): last output 0 min ago\n" +
		"- subagent \"bad id\" (unknown): last activity unknown\n" +
		"- workflow \"wf\" (wf1): last activity unknown\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// --stuck raises the threshold.
	if got := remind(t, w, string(in), "--stuck", "20"); strings.Contains(got, "likely stuck") {
		t.Fatalf("--stuck 20 must not flag 14 min: %s", got)
	}
}

func TestReminderJobListLimits(t *testing.T) {
	w := testWatcher(t, nil)
	var tasks []map[string]string
	for i := 0; i < 23; i++ {
		tasks = append(tasks, map[string]string{"id": "j" + string(rune('a'+i)), "type": "shell", "status": "running", "description": strings.Repeat("x", 200)})
	}
	in, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": filepath.Join(t.TempDir(), "p", "s1.jsonl"), "background_tasks": tasks})
	got := remind(t, w, string(in))
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 1+20+1 || lines[len(lines)-1] != "... and 3 more" {
		t.Fatalf("got %d lines: %s", len(lines), got)
	}
	if !strings.Contains(lines[1], `"`+strings.Repeat("x", 80)+`"`) {
		t.Fatalf("description not cut to 80: %s", lines[1])
	}
}

func TestReminderNoTranscriptPathNoList(t *testing.T) {
	w := testWatcher(t, nil)
	if got := remind(t, w, liveJSON); strings.TrimSpace(got) != defaultMessage || strings.Contains(got, "\n-") {
		t.Fatalf("unexpected list: %q", got)
	}
}

func TestStuckFlagInstall(t *testing.T) {
	if c := hookCommand("lazymf", "claude", 60, 60, 5, "", ""); strings.Contains(c, "--stuck") {
		t.Fatal(c)
	}
	if c := hookCommand("lazymf", "claude", 60, 60, 9, "", ""); !strings.HasSuffix(c, "--stuck 9") {
		t.Fatal(c)
	}
}

func metaInput(t *testing.T, metaA string, tasks ...map[string]string) string {
	t.Helper()
	proj := filepath.Join(t.TempDir(), "proj")
	if metaA != "" {
		p := filepath.Join(proj, "s1", "subagents", "agent-a.meta.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(metaA), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": filepath.Join(proj, "s1.jsonl"), "background_tasks": tasks})
	return string(in)
}

func TestFinishedSubagentAloneIsSilent(t *testing.T) {
	in := metaInput(t, `{"agentType":"general-purpose","finished":true}`,
		map[string]string{"id": "a", "type": "subagent", "status": "running"})
	var out, errb bytes.Buffer
	if code := run([]string{"hook", "claude", "--interval", "3"}, strings.NewReader(in), &out, &errb, testWatcher(t, nil)); code != 0 || errb.Len() != 0 || out.Len() != 0 {
		t.Fatalf("code=%d err=%q out=%q", code, errb.String(), out.String())
	}
	var out2, errb2 bytes.Buffer
	if code := run([]string{"hook", "claude", "--mode", "block", "--interval", "3"}, strings.NewReader(in), &out2, &errb2, testWatcher(t, nil)); code != 0 || errb2.Len() != 0 || out2.Len() != 0 {
		t.Fatalf("block: code=%d err=%q out=%q", code, errb2.String(), out2.String())
	}
}

func TestFinishedSubagentPlusShellListsOnlyShell(t *testing.T) {
	in := metaInput(t, `{"finished":true}`,
		map[string]string{"id": "a", "type": "subagent", "status": "running", "description": "gone"},
		map[string]string{"id": "sh1", "type": "shell", "status": "running", "description": "alive"})
	got := remind(t, testWatcher(t, nil), in)
	if strings.Contains(got, "gone") || !strings.Contains(got, `shell "alive"`) {
		t.Fatalf("got: %s", got)
	}
}

func TestInvalidMetaCountsAsLive(t *testing.T) {
	for _, meta := range []string{`{not json`, `{"finished":false}`, ""} {
		in := metaInput(t, meta, map[string]string{"id": "a", "type": "subagent", "status": "running", "description": "still"})
		if got := remind(t, testWatcher(t, nil), in); !strings.Contains(got, `subagent "still"`) {
			t.Fatalf("meta=%q got: %s", meta, got)
		}
	}
}
