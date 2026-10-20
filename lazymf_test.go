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
	return watcher{poll: time.Second, dir: filepath.Join(t.TempDir(), "state"), sleep: func(time.Duration) {
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
			_ = registryAdd(p, string(rune('a'+i)))
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
	args := []string{"--home", home, "--interval", "30"}
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
	if !strings.Contains(s, "other.sh") || !strings.Contains(s, `"asyncRewake": true`) || !strings.Contains(s, `"timeout": 90`) {
		t.Fatalf("unexpected settings: %s", s)
	}
	if bak, err := os.ReadFile(settings + backupSuffix); err != nil || string(bak) != orig {
		t.Fatalf("backup missing or wrong: %v", err)
	}
	codexFile := filepath.Join(home, ".codex", "hooks.json")
	if b, err := os.ReadFile(codexFile); err != nil || strings.Count(string(b), " hook codex") != 3 {
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
