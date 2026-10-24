package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCodexCommandShell(t *testing.T) {
	c := `"C:/bin/lazymf.exe" hook codex --interval 60`
	if got := codexCommand(c, "windows"); got != "& "+c {
		t.Fatalf("windows needs the PowerShell call operator, got %q", got)
	}
	if got := codexCommand(c, "linux"); got != c {
		t.Fatalf("non-windows must stay plain, got %q", got)
	}
	if !isOurs(codexCommand(c, "windows"), "codex") {
		t.Fatal("a prefixed command must still be recognised as ours")
	}
}

func TestCodexInstallWritesShellCommandAndReplacesOldForm(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".codex", "hooks.json")
	old := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"\"C:/x/lazymf.exe\" hook codex --interval 60","timeout":180}]}]}}`
	if err := os.WriteFile(file, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runInstall([]string{"--home", home, "--tool", "codex"}, &out, &errb, true); code != 0 {
		t.Fatalf("install: %d %s", code, errb.String())
	}
	b, _ := os.ReadFile(file)
	s := string(b)
	if strings.Count(s, " hook codex") != 3 {
		t.Fatalf("old entry must be replaced, not duplicated: %s", s)
	}
	if runtime.GOOS == "windows" && strings.Count(s, `"& \"`) != 3 {
		t.Fatalf("every codex command needs the call operator on Windows: %s", s)
	}
}
