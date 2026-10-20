package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// minRewakeVersion is the oldest Claude Code known to work with asyncRewake and
	// background_tasks. 2.1.280 is the version tested in real. The official changelog
	// only documents background_tasks (2.1.145) and a fix to asyncRewake (2.1.287); it
	// never says when asyncRewake was introduced.
	minRewakeVersion = "2.1.280"
	blockCapEnv      = "CLAUDE_CODE_STOP_HOOK_BLOCK_CAP"
	blockCapValue    = "1000"
	cliTimeout       = 5 * time.Second
	agentMapMarker   = "agentMapAgents"
)

var (
	verRe       = regexp.MustCompile(`\d+(\.\d+)+`)
	verPrefixRe = regexp.MustCompile(`^\d+(\.\d+)*`)
)

func parseVersion(s string) ([]int, bool) {
	m := verRe.FindString(s)
	if m == "" {
		return nil, false
	}
	var out []int
	for _, p := range strings.Split(m, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// compareVersions returns -1, 0 or 1; missing components count as 0.
func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// versionAtLeast reports whether the first version found in s is >= min.
func versionAtLeast(s, min string) bool {
	v, ok := parseVersion(s)
	m, ok2 := parseVersion(min)
	return ok && ok2 && compareVersions(v, m) >= 0
}

type cliInfo struct {
	Name    string
	Found   bool
	Version string // first line of `--version`, "" if it could not be read
}

// cliVersion looks the CLI up in PATH and runs `<name> --version` with a 5 s limit.
func cliVersion(name string) cliInfo {
	info := cliInfo{Name: name}
	path, err := exec.LookPath(name)
	if err != nil {
		return info
	}
	info.Found = true
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return info
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	info.Version = strings.TrimSpace(line)
	return info
}

// strategy is how lazymf covers one tool.
type strategy struct {
	Tool   string
	Mode   string // claude: "rewake" or "block"; codex/copilot: "block"
	Reason string
}

func (s strategy) String() string {
	return fmt.Sprintf("%s: %s mode (%s)", s.Tool, s.Mode, s.Reason)
}

// claudeStrategy picks the Claude Code mode. requested is auto, rewake or block.
func claudeStrategy(requested, version string) strategy {
	switch requested {
	case "rewake", "block":
		return strategy{"claude", requested, "forced with --mode"}
	}
	if _, ok := parseVersion(version); !ok {
		return strategy{"claude", "block", "claude version unknown"}
	}
	if versionAtLeast(version, minRewakeVersion) {
		return strategy{"claude", "rewake", fmt.Sprintf("claude %s >= %s", firstVersion(version), minRewakeVersion)}
	}
	return strategy{"claude", "block", fmt.Sprintf("claude %s < %s", firstVersion(version), minRewakeVersion)}
}

func firstVersion(s string) string { return verRe.FindString(s) }

func registryStrategy(tool string) strategy {
	return strategy{tool, "block", "stop hook blocks the turn, jobs tracked in a registry"}
}

type extInfo struct {
	Editor        string
	ClaudeVersion string
	ClaudeDir     string
	CodexVersion  string // openai.chatgpt
	AgentMap      string // "found", "missing" or "" when unknown / no extension
}

var editorDirs = []struct{ name, dir string }{
	{"VS Code", ".vscode"},
	{"VS Code Insiders", ".vscode-insiders"},
	{"Cursor", ".cursor"},
	{"Windsurf", ".windsurf"},
}

// highestExt returns the highest version among folders named <prefix><version>[-suffix].
func highestExt(entries []os.DirEntry, prefix string) (ver, dir string) {
	var best []int
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		m := verPrefixRe.FindString(e.Name()[len(prefix):])
		v, ok := parseVersion(m)
		if !ok {
			continue
		}
		if best == nil || compareVersions(v, best) > 0 {
			best, ver, dir = v, m, e.Name()
		}
	}
	return
}

// scanExtensions reads the extension folders of each editor from disk (no processes).
func scanExtensions(home string) []extInfo {
	var out []extInfo
	for _, ed := range editorDirs {
		root := filepath.Join(home, ed.dir, "extensions")
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		x := extInfo{Editor: ed.name}
		x.ClaudeVersion, x.ClaudeDir = highestExt(entries, "anthropic.claude-code-")
		x.CodexVersion, _ = highestExt(entries, "openai.chatgpt-")
		if x.ClaudeVersion == "" && x.CodexVersion == "" {
			continue
		}
		if x.ClaudeDir != "" {
			if b, err := os.ReadFile(filepath.Join(root, x.ClaudeDir, "webview", "index.js")); err == nil {
				x.AgentMap = "missing"
				if bytes.Contains(b, []byte(agentMapMarker)) {
					x.AgentMap = "found"
				}
			}
		}
		out = append(out, x)
	}
	return out
}

// scanJetBrains lists plugins with "claude" in the name under the usual JetBrains config roots.
func scanJetBrains(home string) []string {
	roots := []string{
		filepath.Join(home, "AppData", "Roaming", "JetBrains"),
		filepath.Join(home, ".config", "JetBrains"),
		filepath.Join(home, ".local", "share", "JetBrains"),
		filepath.Join(home, "Library", "Application Support", "JetBrains"),
	}
	var out []string
	for _, root := range roots {
		products, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, p := range products {
			if !p.IsDir() {
				continue
			}
			for _, base := range []string{filepath.Join(root, p.Name(), "plugins"), filepath.Join(root, p.Name())} {
				plugins, err := os.ReadDir(base)
				if err != nil {
					continue
				}
				for _, pl := range plugins {
					if strings.Contains(strings.ToLower(pl.Name()), "claude") {
						out = append(out, p.Name()+": "+pl.Name())
					}
				}
			}
		}
	}
	return out
}

type hookState struct {
	Installed bool
	Flags     string // arguments after "hook <tool>"
	Mode      string // claude only: "rewake" or "block"
	CapSet    bool   // claude only: CLAUDE_CODE_STOP_HOOK_BLOCK_CAP present in settings env
}

func collectCommands(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && (k == "command" || k == "bash") {
				*out = append(*out, s)
			} else {
				collectCommands(val, out)
			}
		}
	case []any:
		for _, val := range x {
			collectCommands(val, out)
		}
	}
}

func hookConfigPath(home, tool string) string {
	switch tool {
	case "claude":
		return filepath.Join(home, ".claude", "settings.json")
	case "codex":
		return filepath.Join(home, ".codex", "hooks.json")
	}
	return copilotFile(home)
}

// readHookState reads the tool's config file and reports whether lazymf's hook is in it.
func readHookState(home, tool string) hookState {
	var st hookState
	b, err := os.ReadFile(hookConfigPath(home, tool))
	if err != nil {
		return st
	}
	var root any
	if json.Unmarshal(b, &root) != nil {
		return st
	}
	var cmds []string
	collectCommands(root, &cmds)
	sort.Strings(cmds)
	for _, c := range cmds {
		if !isOurs(c, tool) {
			continue
		}
		_, rest, _ := strings.Cut(c, " hook "+tool)
		st.Installed, st.Flags = true, strings.TrimSpace(rest)
		break
	}
	if tool == "claude" && st.Installed {
		st.Mode = "rewake"
		if strings.Contains(st.Flags, "--mode block") {
			st.Mode = "block"
		}
		if m, ok := root.(map[string]any); ok {
			if env, ok := m["env"].(map[string]any); ok {
				_, st.CapSet = env[blockCapEnv]
			}
		}
	}
	return st
}

type diag struct {
	Home       string
	CLIs       []cliInfo
	Exts       []extInfo
	JetBrains  []string
	Strategies []strategy
	Hooks      map[string]hookState
	ToolDirs   map[string]bool
}

// gather reads the environment. claudeMode is the requested mode (auto, rewake, block).
func gather(home, claudeMode string) diag {
	d := diag{Home: home, Hooks: map[string]hookState{}, ToolDirs: map[string]bool{}}
	for _, t := range supportedTools {
		d.CLIs = append(d.CLIs, cliVersion(t))
		d.Hooks[t] = readHookState(home, t)
		d.ToolDirs[t] = dirExists(filepath.Join(home, toolDir(t)))
	}
	d.Exts = scanExtensions(home)
	d.JetBrains = scanJetBrains(home)
	d.Strategies = []strategy{claudeStrategy(claudeMode, d.CLIs[0].Version), registryStrategy("codex"), registryStrategy("copilot")}
	return d
}

func (d diag) cli(name string) cliInfo {
	for _, c := range d.CLIs {
		if c.Name == name {
			return c
		}
	}
	return cliInfo{Name: name}
}

func (d diag) strategy(tool string) strategy {
	for _, s := range d.Strategies {
		if s.Tool == tool {
			return s
		}
	}
	return strategy{}
}

// recommendations returns concrete next steps; empty when everything is in order.
func (d diag) recommendations() []string {
	var r []string
	claude := d.cli("claude")
	ms := d.strategy("claude")
	switch {
	case claude.Found && claude.Version == "":
		r = append(r, "claude: could not read the CLI version; block mode is used. Run `claude --version` to check it works.")
	case claude.Found && ms.Mode == "block" && !versionAtLeast(claude.Version, minRewakeVersion):
		r = append(r, fmt.Sprintf("claude: update Claude Code to %s or newer to use the non-blocking rewake mode (now %s).", minRewakeVersion, firstVersion(claude.Version)))
	}
	for _, x := range d.Exts {
		if x.AgentMap == "missing" {
			r = append(r, fmt.Sprintf("%s: the Claude Code extension %s has no agents view (%q not found in webview/index.js); update the extension to get it.", x.Editor, x.ClaudeVersion, agentMapMarker))
		}
	}
	for _, t := range supportedTools {
		st := d.Hooks[t]
		present := d.ToolDirs[t] || d.cli(t).Found
		switch {
		case !st.Installed && present:
			r = append(r, fmt.Sprintf("%s: hook not installed; run `lazymf install --tool %s`.", t, t))
		case st.Installed && t == "claude" && st.Mode != ms.Mode:
			r = append(r, fmt.Sprintf("claude: installed hook uses %s mode but the strategy is %s; run `lazymf install --tool claude`.", st.Mode, ms.Mode))
		case st.Installed && t == "claude" && st.Mode == "block" && !st.CapSet:
			r = append(r, fmt.Sprintf("claude: %s is not set in settings.json env, so Claude Code stops after 8 consecutive blocks; run `lazymf install --tool claude`.", blockCapEnv))
		case st.Installed && t == "codex":
			r = append(r, "codex: approve the lazymf hooks with `/hooks` inside Codex, or they never run.")
		}
	}
	return r
}

func printDoctor(w io.Writer, d diag) {
	fmt.Fprintln(w, "lazymf doctor", version)
	fmt.Fprintln(w, "\nCLIs")
	for _, c := range d.CLIs {
		switch {
		case !c.Found:
			fmt.Fprintf(w, "  %-8s not found\n", c.Name)
		case c.Version == "":
			fmt.Fprintf(w, "  %-8s found, version unreadable\n", c.Name)
		default:
			fmt.Fprintf(w, "  %-8s %s\n", c.Name, c.Version)
		}
	}
	fmt.Fprintln(w, "\nEditors and extensions")
	if len(d.Exts) == 0 && len(d.JetBrains) == 0 {
		fmt.Fprintln(w, "  none found")
	}
	for _, x := range d.Exts {
		fmt.Fprintf(w, "  %s\n", x.Editor)
		if x.ClaudeVersion != "" {
			fmt.Fprintf(w, "    Claude Code extension %s (agents view: %s)\n", x.ClaudeVersion, orUnknown(x.AgentMap))
		}
		if x.CodexVersion != "" {
			fmt.Fprintf(w, "    Codex extension (openai.chatgpt) %s\n", x.CodexVersion)
		}
	}
	for _, j := range d.JetBrains {
		fmt.Fprintf(w, "  JetBrains plugin %s\n", j)
	}
	fmt.Fprintln(w, "\nStrategy and hook per tool")
	for _, s := range d.Strategies {
		fmt.Fprintf(w, "  %s\n", s)
		st := d.Hooks[s.Tool]
		if !st.Installed {
			fmt.Fprintln(w, "    hook: not installed")
			continue
		}
		fmt.Fprintf(w, "    hook: installed, flags: %s\n", orNone(st.Flags))
		if s.Tool == "claude" {
			fmt.Fprintf(w, "    installed mode: %s; %s in env: %v\n", st.Mode, blockCapEnv, st.CapSet)
		}
	}
	fmt.Fprintln(w, "\nRecommendations")
	printRecommendations(w, d)
}

func printRecommendations(w io.Writer, d diag) {
	rec := d.recommendations()
	if len(rec) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, r := range rec {
		fmt.Fprintf(w, "  - %s\n", r)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to inspect (default: the user's home)")
	if err := fs.Parse(args); err != nil {
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
	printDoctor(stdout, gather(*home, "auto"))
	return 0
}
