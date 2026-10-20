package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const backupSuffix = ".bak-lazymf"

var supportedTools = []string{"claude", "codex", "copilot"}

func runInstall(args []string, stdout, stderr io.Writer, install bool) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tool := fs.String("tool", "all", "claude, codex, copilot or all")
	interval := fs.Int("interval", defaultInterval, "seconds between reminders")
	home := fs.String("home", "", "home directory to configure (default: the user's home)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *interval < 1 {
		fmt.Fprintln(stderr, "lazymf: --interval must be >= 1")
		return 2
	}
	if *home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "lazymf: %v\n", err)
			return 1
		}
		*home = h
	}
	tools := supportedTools
	explicit := *tool != "all"
	if explicit {
		if !contains(supportedTools, *tool) {
			fmt.Fprintf(stderr, "lazymf: unsupported tool %q (supported: %s)\n", *tool, strings.Join(supportedTools, ", "))
			return 2
		}
		tools = []string{*tool}
	}
	bin := ""
	if install {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(stderr, "lazymf: %v\n", err)
			return 1
		}
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		bin = filepath.ToSlash(exe)
	}
	code := 0
	for _, t := range tools {
		present := dirExists(filepath.Join(*home, toolDir(t)))
		if !present && !explicit {
			fmt.Fprintf(stdout, "%s: not detected, skipped\n", t)
			continue
		}
		var changed bool
		var err error
		if install {
			changed, err = installTool(*home, t, bin, *interval)
		} else {
			changed, err = uninstallTool(*home, t)
		}
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "%s: %v\n", t, err)
			code = 1
		case changed:
			fmt.Fprintf(stdout, "%s: updated\n", t)
		default:
			fmt.Fprintf(stdout, "%s: already up to date\n", t)
		}
	}
	return code
}

func toolDir(tool string) string { return "." + tool }

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func hookCommand(bin, tool string, interval int, event string) string {
	c := fmt.Sprintf(`"%s" hook %s --interval %d`, bin, tool, interval)
	if event != "" {
		c += " --event " + event
	}
	return c
}

// isOurs reports whether a hook command line was written by lazymf for this tool.
func isOurs(cmd, tool string) bool {
	return strings.Contains(strings.ToLower(cmd), "lazymf") && strings.Contains(cmd, " hook "+tool)
}

func installTool(home, tool, bin string, interval int) (bool, error) {
	switch tool {
	case "claude":
		entries := map[string]map[string]any{
			"Stop": {
				"type": "command", "command": hookCommand(bin, tool, interval, ""),
				"asyncRewake": true, "timeout": interval + 60,
			},
		}
		return editHooksJSON(filepath.Join(home, ".claude", "settings.json"), tool, entries, true)
	case "codex":
		cmd := hookCommand(bin, tool, interval, "")
		entries := map[string]map[string]any{
			"Stop":          {"type": "command", "command": cmd, "timeout": interval + 60},
			"SubagentStart": {"type": "command", "command": cmd, "timeout": 10},
			"SubagentStop":  {"type": "command", "command": cmd, "timeout": 10},
		}
		return editHooksJSON(filepath.Join(home, ".codex", "hooks.json"), tool, entries, true)
	case "copilot":
		return installCopilot(home, bin, interval)
	}
	return false, fmt.Errorf("unsupported tool %q", tool)
}

func uninstallTool(home, tool string) (bool, error) {
	switch tool {
	case "claude":
		return editHooksJSON(filepath.Join(home, ".claude", "settings.json"), tool, nil, false)
	case "codex":
		return editHooksJSON(filepath.Join(home, ".codex", "hooks.json"), tool, nil, false)
	case "copilot":
		p := copilotFile(home)
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !strings.Contains(string(b), "lazymf") {
			return false, fmt.Errorf("%s is not a lazymf file, left untouched", p)
		}
		return true, os.Remove(p)
	}
	return false, fmt.Errorf("unsupported tool %q", tool)
}

// editHooksJSON adds (install) or removes lazymf handlers in a Claude-style
// hooks file: {"hooks": {"<Event>": [{"hooks": [{type, command, ...}]}]}}.
// Everything else in the file is kept. Returns whether the file changed.
func editHooksJSON(path, tool string, entries map[string]map[string]any, install bool) (bool, error) {
	orig, err := os.ReadFile(path)
	root := map[string]any{}
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !install {
			return false, nil
		}
		orig = nil
	case err != nil:
		return false, err
	default:
		dec := json.NewDecoder(bytes.NewReader(orig))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			return false, fmt.Errorf("%s is not valid JSON, left untouched: %w", path, err)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		if root["hooks"] != nil {
			return false, fmt.Errorf("%s: unexpected \"hooks\" shape, left untouched", path)
		}
		hooks = map[string]any{}
	}
	for ev, v := range hooks {
		groups, ok := v.([]any)
		if !ok {
			continue
		}
		if left := stripOurs(groups, tool); len(left) > 0 {
			hooks[ev] = left
		} else {
			delete(hooks, ev)
		}
	}
	for ev, entry := range entries {
		groups, _ := hooks[ev].([]any)
		hooks[ev] = append(groups, map[string]any{"hooks": []any{entry}})
	}
	if len(hooks) > 0 {
		root["hooks"] = hooks
	} else {
		delete(root, "hooks")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return false, err
	}
	if bytes.Equal(buf.Bytes(), orig) {
		return false, nil
	}
	if err := backupOnce(path, orig); err != nil {
		return false, err
	}
	return true, writeFile0600(path, buf.Bytes())
}

// stripOurs removes lazymf handlers from matcher groups and drops groups left empty.
func stripOurs(groups []any, tool string) []any {
	var out []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			out = append(out, g)
			continue
		}
		hs, ok := gm["hooks"].([]any)
		if !ok {
			out = append(out, g)
			continue
		}
		var keep []any
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok {
				if c, _ := hm["command"].(string); isOurs(c, tool) {
					continue
				}
			}
			keep = append(keep, h)
		}
		if len(keep) == 0 {
			continue
		}
		gm["hooks"] = keep
		out = append(out, gm)
	}
	return out
}

func copilotFile(home string) string {
	return filepath.Join(home, ".copilot", "hooks", "lazymf.json")
}

// installCopilot owns a separate file: Copilot loads every *.json in ~/.copilot/hooks.
func installCopilot(home, bin string, interval int) (bool, error) {
	mk := func(event string, timeout int) []any {
		c := hookCommand(bin, "copilot", interval, event)
		return []any{map[string]any{
			"type": "command", "bash": c, "powershell": "& " + c, "timeoutSec": timeout,
		}}
	}
	doc := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"agentStop":     mk("agentStop", interval+60),
			"subagentStart": mk("subagentStart", 10),
			"subagentStop":  mk("subagentStop", 10),
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return false, err
	}
	p := copilotFile(home)
	orig, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if bytes.Equal(orig, buf.Bytes()) {
		return false, nil
	}
	if err := backupOnce(p, orig); err != nil {
		return false, err
	}
	return true, writeFile0600(p, buf.Bytes())
}

// backupOnce keeps the first pre-lazymf copy of a file as <file>.bak-lazymf.
func backupOnce(path string, orig []byte) error {
	if orig == nil {
		return nil
	}
	bak := path + backupSuffix
	if _, err := os.Stat(bak); err == nil {
		return nil
	}
	return os.WriteFile(bak, orig, 0o600)
}

func writeFile0600(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
