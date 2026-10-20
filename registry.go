package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The registry is a per-session file with one job id per line, kept for tools
// whose stop hook does not list the live jobs (Codex, Copilot).

func registryPath(dir, tool, sid string) string {
	return filepath.Join(dir, tool+"-"+sid+".jobs")
}

func withLock(path string, fn func() error) error {
	lock := path + ".lock"
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			break
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > 10*time.Second {
			_ = os.Remove(lock) // stale lock from a crashed run
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("registry lock timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() { _ = os.Remove(lock) }()
	return fn()
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func registryAdd(path, id string) error {
	return withLock(path, func() error {
		lines := readLines(path)
		for _, l := range lines {
			if l == id {
				return nil
			}
		}
		return writeLines(path, append(lines, id))
	})
}

// registryRemove drops id; if anyOne is true and id is absent it drops one entry.
func registryRemove(path, id string, anyOne bool) error {
	return withLock(path, func() error {
		lines := readLines(path)
		idx := -1
		for i, l := range lines {
			if id != "" && l == id {
				idx = i
				break
			}
		}
		if idx < 0 && anyOne && len(lines) > 0 {
			idx = 0
		}
		if idx < 0 {
			return nil
		}
		return writeLines(path, append(lines[:idx], lines[idx+1:]...))
	})
}

func registryCount(path string) int { return len(readLines(path)) }
