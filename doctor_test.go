package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeCLIs writes one script per name that prints the given text for --version
// into a new directory and puts only that directory on PATH.
func fakeCLIs(t *testing.T, versions map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, out := range versions {
		if runtime.GOOS == "windows" {
			body := "@echo off\r\necho " + out + "\r\n"
			if err := os.WriteFile(filepath.Join(dir, name+".cmd"), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho '"+out+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".CMD;.EXE")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.280", "2.1.280", 0},
		{"2.1.301", "2.1.280", 1},
		{"2.1.9", "2.1.10", -1},
		{"2.2", "2.1.999", 1},
		{"2.1", "2.1.0", 0},
		{"26.1007.21434", "26.1007.9", 1},
	}
	for _, c := range cases {
		a, _ := parseVersion(c.a)
		b, _ := parseVersion(c.b)
		if got := compareVersions(a, b); got != c.want {
			t.Errorf("%s vs %s: got %d want %d", c.a, c.b, got, c.want)
		}
	}
	if v, ok := parseVersion("2.1.280 (Claude Code)"); !ok || len(v) != 3 || v[2] != 280 {
		t.Fatalf("parse: %v %v", v, ok)
	}
	if _, ok := parseVersion("garbage"); ok {
		t.Fatal("garbage must not parse")
	}
	if !versionAtLeast("2.1.304 (Claude Code)", minRewakeVersion) || versionAtLeast("2.1.100", minRewakeVersion) || versionAtLeast("", minRewakeVersion) {
		t.Fatal("versionAtLeast")
	}
}

func TestClaudeStrategyChoice(t *testing.T) {
	cases := []struct{ req, ver, want string }{
		{"auto", "2.1.304 (Claude Code)", "rewake"},
		{"auto", "2.1.280", "rewake"},
		{"auto", "2.1.200", "block"},
		{"auto", "", "block"},
		{"auto", "nonsense", "block"},
		{"block", "2.1.304", "block"},
		{"rewake", "2.1.1", "rewake"},
	}
	for _, c := range cases {
		if got := claudeStrategy(c.req, c.ver).Mode; got != c.want {
			t.Errorf("%s/%q: got %s want %s", c.req, c.ver, got, c.want)
		}
	}
}

func TestClaudeBlockModeOutput(t *testing.T) {
	w := testWatcher(t, nil)
	var out, errb bytes.Buffer
	code := run([]string{"hook", "claude", "--mode", "block", "--interval", "3"}, strings.NewReader(liveJSON), &out, &errb, w)
	var d map[string]string
	if code != 0 || errb.Len() != 0 || json.Unmarshal(out.Bytes(), &d) != nil || d["decision"] != "block" || d["reason"] != defaultMessage {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	// With a job list.
	in, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": filepath.Join(t.TempDir(), "p", "s1.jsonl"),
		"background_tasks": []map[string]string{{"id": "a1", "type": "subagent", "status": "running", "description": "work"}}})
	out.Reset()
	run([]string{"hook", "claude", "--mode", "block", "--interval", "3"}, strings.NewReader(string(in)), &out, &errb, w)
	if json.Unmarshal(out.Bytes(), &d) != nil || !strings.HasPrefix(d["reason"], defaultMessage+"\n- subagent \"work\" (a1): last activity unknown") {
		t.Fatalf("reason: %q", d["reason"])
	}
	// No live jobs: silent.
	out.Reset()
	quiet := `{"session_id":"s1","background_tasks":[{"id":"a","type":"shell","status":"completed"}]}`
	if code := run([]string{"hook", "claude", "--mode", "block"}, strings.NewReader(quiet), &out, &errb, w); code != 0 || out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("must be silent: %d %q", code, out.String())
	}
	// Superseded by a newer run: silent.
	w.sleep = func(time.Duration) {
		_ = os.WriteFile(filepath.Join(w.dir, "claude-s1.token"), []byte("newer"), 0o600)
	}
	if code := run([]string{"hook", "claude", "--mode", "block", "--interval", "3"}, strings.NewReader(liveJSON), &out, &errb, w); code != 0 || out.Len() != 0 {
		t.Fatalf("superseded must be silent: %d %q", code, out.String())
	}
	if code := run([]string{"hook", "claude", "--mode", "bogus"}, strings.NewReader(liveJSON), &out, &errb, w); code != 1 {
		t.Fatalf("bad mode accepted: %d", code)
	}
}

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEnvCapIdempotentAndUninstall(t *testing.T) {
	fakeCLIs(t, map[string]string{"claude": "2.1.100 (Claude Code)"})
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := `{"model":"x","env":{"FOO":"bar"}}`
	if err := os.WriteFile(settings, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	args := []string{"--home", home, "--tool", "claude"} // auto + old fake claude => block
	if code := runInstall(args, &out, &errb, true); code != 0 {
		t.Fatalf("install: %d %s", code, errb.String())
	}
	first, _ := os.ReadFile(settings)
	env := readJSON(t, settings)["env"].(map[string]any)
	if env[blockCapEnv] != "1000" || env["FOO"] != "bar" {
		t.Fatalf("env: %v", env)
	}
	s := string(first)
	if !strings.Contains(s, "--mode block") || strings.Contains(s, "asyncRewake") {
		t.Fatalf("block mode hook expected: %s", s)
	}
	if !strings.Contains(out.String(), "claude: block mode (claude 2.1.100 < 2.1.280)") {
		t.Fatalf("strategy not printed: %s", out.String())
	}
	if code := runInstall(args, &out, &errb, true); code != 0 {
		t.Fatal("reinstall failed")
	}
	if second, _ := os.ReadFile(settings); !bytes.Equal(first, second) {
		t.Fatalf("not idempotent:\n%s\n%s", first, second)
	}
	if code := runInstall([]string{"--home", home, "--tool", "claude"}, &out, &errb, false); code != 0 {
		t.Fatal("uninstall failed")
	}
	root := readJSON(t, settings)
	env = root["env"].(map[string]any)
	if _, ok := env[blockCapEnv]; ok || env["FOO"] != "bar" || root["model"] != "x" || root["hooks"] != nil {
		t.Fatalf("uninstall left or removed the wrong things: %v", root)
	}
}

func TestEnvCapUserValueSurvives(t *testing.T) {
	fakeCLIs(t, map[string]string{"claude": "2.1.100"})
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"env":{"`+blockCapEnv+`":"50"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	args := []string{"--home", home, "--tool", "claude"}
	runInstall(args, &out, &errb, true)
	if env := readJSON(t, settings)["env"].(map[string]any); env[blockCapEnv] != "50" {
		t.Fatalf("user value overwritten: %v", env)
	}
	runInstall(args, &out, &errb, false)
	if env := readJSON(t, settings)["env"].(map[string]any); env[blockCapEnv] != "50" {
		t.Fatalf("user value removed by uninstall: %v", env)
	}
}

func TestEnvCapDroppedWhenSwitchingToRewake(t *testing.T) {
	fakeCLIs(t, map[string]string{"claude": "2.1.304 (Claude Code)"})
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	runInstall([]string{"--home", home, "--tool", "claude", "--mode", "block"}, &out, &errb, true)
	settings := filepath.Join(home, ".claude", "settings.json")
	if env, _ := readJSON(t, settings)["env"].(map[string]any); env[blockCapEnv] != "1000" {
		t.Fatalf("cap missing: %v", env)
	}
	runInstall([]string{"--home", home, "--tool", "claude"}, &out, &errb, true) // auto + new claude => rewake
	root := readJSON(t, settings)
	if _, ok := root["env"]; ok {
		t.Fatalf("cap not removed: %v", root["env"])
	}
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), `"asyncRewake": true`) || strings.Contains(string(b), "--mode block") {
		t.Fatalf("rewake hook expected: %s", b)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanExtensions(t *testing.T) {
	home := t.TempDir()
	ext := filepath.Join(home, ".vscode", "extensions")
	writeFile(t, filepath.Join(ext, "anthropic.claude-code-2.1.9-win32-x64", "webview", "index.js"), "no marker here")
	writeFile(t, filepath.Join(ext, "anthropic.claude-code-2.1.10-win32-x64", "webview", "index.js"), "x agentMapAgents y")
	writeFile(t, filepath.Join(ext, "openai.chatgpt-26.1007.21434-win32-x64", "package.json"), "{}")
	writeFile(t, filepath.Join(ext, "openai.chatgpt-26.1007.9-win32-x64", "package.json"), "{}")
	writeFile(t, filepath.Join(ext, "openai.codex-audio-1.0.0", "x"), "")
	writeFile(t, filepath.Join(home, ".cursor", "extensions", "anthropic.claude-code-2.1.5", "webview", "index.js"), "old")
	writeFile(t, filepath.Join(home, ".windsurf", "extensions", "other.thing-1.0.0", "x"), "")
	got := scanExtensions(home)
	if len(got) != 2 {
		t.Fatalf("editors: %+v", got)
	}
	vs, cur := got[0], got[1]
	if vs.Editor != "VS Code" || vs.ClaudeVersion != "2.1.10" || vs.CodexVersion != "26.1007.21434" || vs.AgentMap != "found" {
		t.Fatalf("vscode: %+v", vs)
	}
	if cur.Editor != "Cursor" || cur.ClaudeVersion != "2.1.5" || cur.CodexVersion != "" || cur.AgentMap != "missing" {
		t.Fatalf("cursor: %+v", cur)
	}
}

func TestScanJetBrains(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "AppData", "Roaming", "JetBrains", "PyCharm2026.1", "plugins", "claude-code-jetbrains", "x"), "")
	writeFile(t, filepath.Join(home, "AppData", "Roaming", "JetBrains", "PyCharm2026.1", "plugins", "other", "x"), "")
	got := scanJetBrains(home)
	if len(got) != 1 || got[0] != "PyCharm2026.1: claude-code-jetbrains" {
		t.Fatalf("got %v", got)
	}
	if got := scanJetBrains(t.TempDir()); len(got) != 0 {
		t.Fatalf("nothing expected: %v", got)
	}
}

func TestDoctorOutput(t *testing.T) {
	fakeCLIs(t, map[string]string{"claude": "2.1.100 (Claude Code)"})
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".vscode", "extensions", "anthropic.claude-code-2.1.1-win32-x64", "webview", "index.js"), "old")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runDoctor([]string{"--home", home}, &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"claude   2.1.100 (Claude Code)",
		"codex    not found",
		"copilot  not found",
		"Claude Code extension 2.1.1 (agents view: missing)",
		"claude: block mode (claude 2.1.100 < 2.1.280)",
		"update Claude Code to 2.1.280",
		"update the extension",
		"codex: hook not installed; run `lazymf install --tool codex`",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// After installing, the hook state shows up and the codex approval hint appears.
	runInstall([]string{"--home", home, "--tool", "codex"}, &out, &errb, true)
	out.Reset()
	runDoctor([]string{"--home", home}, &out, &errb)
	if s := out.String(); !strings.Contains(s, "hook: installed, flags: --interval 60") || !strings.Contains(s, "approve the lazymf hooks with `/hooks`") {
		t.Fatalf("after install:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Fatal("doctor must not create anything")
	}
}

func TestCLIVersionNotFound(t *testing.T) {
	fakeCLIs(t, map[string]string{"codex": "codex-cli 0.9.1"})
	if c := cliVersion("claude"); c.Found {
		t.Fatalf("claude must not be found: %+v", c)
	}
	if c := cliVersion("codex"); !c.Found || c.Version != "codex-cli 0.9.1" {
		t.Fatalf("codex: %+v", c)
	}
}
