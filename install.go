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
	"runtime"
	"strings"
)

const backupSuffix = ".bak-lazymf"

// hookTimeout (seconds) is the Stop hook timeout. Reminders wait interval x step with no cap, so the
// tool must not kill the hook while it sleeps.
const hookTimeout = 86400

var supportedTools = []string{"claude", "codex", "copilot"}

func runInstall(args []string, stdout, stderr io.Writer, install bool) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tool := fs.String("tool", "all", "claude, codex, copilot or all")
	interval := fs.Int("interval", defaultInterval, "seconds between reminders")
	maxAge := fs.Int("max-age", defaultMaxAge, "minutes after which a registry entry (Codex, Copilot) is dropped")
	stuck := fs.Int("stuck", defaultStuck, "minutes without output after which a job is flagged as likely stuck (Claude Code)")
	home := fs.String("home", "", "home directory to configure (default: the user's home)")
	mode := fs.String("mode", "auto", "Claude Code mode: auto, rewake or block")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *interval < 1 || *maxAge < 1 || *stuck < 1 {
		fmt.Fprintln(stderr, "lazymf: --interval, --max-age and --stuck must be >= 1")
		return 2
	}
	if *mode != "auto" && *mode != "rewake" && *mode != "block" {
		fmt.Fprintln(stderr, "lazymf: --mode must be auto, rewake or block")
		return 2
	}
	explicitHome := *home != ""
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
	marker := filepath.Join(cacheDirFor(*home, explicitHome), "env-cap-set")
	claudeMode := "rewake"
	var strategies []string
	if install && contains(tools, "claude") && (explicit || dirExists(filepath.Join(*home, toolDir("claude")))) {
		st := claudeStrategy(*mode, cliVersion("claude").Version)
		claudeMode = st.Mode
		strategies = append(strategies, st.String())
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
			changed, err = installTool(*home, t, bin, *interval, *maxAge, *stuck, claudeMode, marker)
		} else {
			changed, err = uninstallTool(*home, t, marker)
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
	if install && code == 0 {
		fmt.Fprintln(stdout, "\nStrategy")
		for _, t := range tools {
			if t == "claude" && len(strategies) > 0 {
				fmt.Fprintln(stdout, "  "+strategies[0])
			} else if t != "claude" && (explicit || dirExists(filepath.Join(*home, toolDir(t)))) {
				fmt.Fprintln(stdout, "  "+registryStrategy(t).String())
			}
		}
		fmt.Fprintln(stdout, "\nRecommendations")
		printRecommendations(stdout, gather(*home, *mode))
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

func hookCommand(bin, tool string, interval, maxAge, stuck int, event, mode string) string {
	c := fmt.Sprintf(`"%s" hook %s --interval %d`, bin, tool, interval)
	if tool != "claude" && maxAge != defaultMaxAge {
		c += fmt.Sprintf(" --max-age %d", maxAge)
	}
	if tool == "claude" && stuck != defaultStuck {
		c += fmt.Sprintf(" --stuck %d", stuck)
	}
	if tool == "claude" && mode == "block" {
		c += " --mode block"
	}
	if event != "" {
		c += " --event " + event
	}
	return c
}

// codexCommand adapts the hook command to the shell Codex uses. On Windows Codex runs hook
// commands through PowerShell, which parses a leading quoted string as an expression ("hook: Stop
// Failed", the hook never runs), so the call operator is required. Elsewhere it is plain sh.
func codexCommand(c, goos string) string {
	if goos == "windows" {
		return "& " + c
	}
	return c
}

// isOurs reports whether a hook command line was written by lazymf for this tool.
func isOurs(cmd, tool string) bool {
	return strings.Contains(strings.ToLower(cmd), "lazymf") && strings.Contains(cmd, " hook "+tool)
}

func installTool(home, tool, bin string, interval, maxAge, stuck int, claudeMode, marker string) (bool, error) {
	switch tool {
	case "claude":
		stop := map[string]any{
			"type": "command", "command": hookCommand(bin, tool, interval, maxAge, stuck, "", claudeMode),
			"timeout": hookTimeout,
		}
		if claudeMode != "block" {
			stop["asyncRewake"] = true
		}
		return editClaudeSettings(home, marker, map[string]map[string]any{"Stop": stop}, true, claudeMode == "block")
	case "codex":
		cmd := codexCommand(hookCommand(bin, tool, interval, maxAge, stuck, "", ""), runtime.GOOS)
		entries := map[string]map[string]any{
			"Stop":          {"type": "command", "command": cmd, "timeout": hookTimeout},
			"SubagentStart": {"type": "command", "command": cmd, "timeout": 10},
			"SubagentStop":  {"type": "command", "command": cmd, "timeout": 10},
		}
		post := codexCommand(hookCommand(bin, tool, interval, maxAge, stuck, "posttooluse", ""), runtime.GOOS)
		entries["PostToolUse"] = map[string]any{"type": "command", "command": post, "timeout": 10, "matcher": ".*"}
		return editHooksJSON(filepath.Join(home, ".codex", "hooks.json"), tool, entries, true, nil)
	case "copilot":
		return installCopilot(home, bin, interval, maxAge, stuck)
	}
	return false, fmt.Errorf("unsupported tool %q", tool)
}

func uninstallTool(home, tool, marker string) (bool, error) {
	switch tool {
	case "claude":
		return editClaudeSettings(home, marker, nil, false, false)
	case "codex":
		return editHooksJSON(filepath.Join(home, ".codex", "hooks.json"), tool, nil, false, nil)
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
func editHooksJSON(path, tool string, entries map[string]map[string]any, install bool, envEdit func(map[string]any)) (bool, error) {
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
		group := map[string]any{}
		h := map[string]any{}
		for k, v := range entry {
			if k == "matcher" {
				group[k] = v // the matcher belongs to the group, not the handler
			} else {
				h[k] = v
			}
		}
		group["hooks"] = []any{h}
		hooks[ev] = append(groups, group)
	}
	if len(hooks) > 0 {
		root["hooks"] = hooks
	} else {
		delete(root, "hooks")
	}
	if envEdit != nil {
		envEdit(root)
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
func installCopilot(home, bin string, interval, maxAge, stuck int) (bool, error) {
	mk := func(event string, timeout int) []any {
		c := hookCommand(bin, "copilot", interval, maxAge, stuck, event, "")
		return []any{map[string]any{
			"type": "command", "bash": c, "powershell": "& " + c, "timeoutSec": timeout,
		}}
	}
	doc := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"agentStop":     mk("agentStop", hookTimeout),
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

// cacheDirFor is where lazymf keeps its own marker files. With --home it stays
// inside that home so nothing outside it is touched.
func cacheDirFor(home string, explicitHome bool) string {
	if !explicitHome {
		if base, err := os.UserCacheDir(); err == nil {
			return filepath.Join(base, "lazymf")
		}
	}
	return filepath.Join(home, ".cache", "lazymf")
}

// editClaudeSettings edits the hooks of ~/.claude/settings.json and, in block mode,
// sets CLAUDE_CODE_STOP_HOOK_BLOCK_CAP in its env. A marker file records that lazymf
// set it, so uninstall (or a switch back to rewake mode) removes only what it put there.
func editClaudeSettings(home, marker string, entries map[string]map[string]any, install, wantCap bool) (bool, error) {
	_, serr := os.Stat(marker)
	had := serr == nil
	mark := had
	edit := func(root map[string]any) {
		var env map[string]any
		if raw, exists := root["env"]; exists {
			var ok bool
			if env, ok = raw.(map[string]any); !ok {
				return // unexpected shape: leave it alone
			}
		}
		cur, present := env[blockCapEnv]
		switch {
		case wantCap && !present:
			if env == nil {
				env = map[string]any{}
				root["env"] = env
			}
			env[blockCapEnv] = blockCapValue
			mark = true
		case !wantCap && had:
			if present && cur == blockCapValue {
				delete(env, blockCapEnv)
				if len(env) == 0 {
					delete(root, "env")
				}
			}
			mark = false
		}
	}
	changed, err := editHooksJSON(filepath.Join(home, ".claude", "settings.json"), "claude", entries, install, edit)
	if err != nil {
		return false, err
	}
	switch {
	case mark && !had:
		err = writeFile0600(marker, []byte(blockCapEnv+"\n"))
	case !mark && had:
		err = os.Remove(marker)
	}
	return changed, err
}
