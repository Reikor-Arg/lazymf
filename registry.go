package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The registry is a per-session file with one "id<TAB>unix-seconds" line per job, kept for tools
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

func entryID(line string) string {
	id, _, _ := strings.Cut(line, "\t")
	return id
}

func registryAdd(path, id string, now time.Time) error {
	id = strings.Join(strings.Fields(id), "_")
	return withLock(path, func() error {
		lines := readLines(path)
		for _, l := range lines {
			if entryID(l) == id {
				return nil
			}
		}
		return writeLines(path, append(lines, id+"\t"+strconv.FormatInt(now.Unix(), 10)))
	})
}

// registryLive drops entries older than maxAge (rewriting the file) and returns
// how many remain. Entries without a timestamp are stamped with now.
func registryLive(path string, now time.Time, maxAge time.Duration) int {
	n := 0
	_ = withLock(path, func() error {
		lines := readLines(path)
		keep := make([]string, 0, len(lines))
		dirty := false
		for _, l := range lines {
			id, ts, ok := strings.Cut(l, "\t")
			sec, err := strconv.ParseInt(ts, 10, 64)
			if !ok || err != nil {
				l, dirty = id+"\t"+strconv.FormatInt(now.Unix(), 10), true
			} else if now.Sub(time.Unix(sec, 0)) > maxAge {
				dirty = true
				continue
			}
			keep = append(keep, l)
		}
		n = len(keep)
		if dirty {
			return writeLines(path, keep)
		}
		return nil
	})
	return n
}

// registryRemove drops id; if anyOne is true and id is absent it drops one entry.
func registryRemove(path, id string, anyOne bool) error {
	return withLock(path, func() error {
		lines := readLines(path)
		idx := -1
		for i, l := range lines {
			if id != "" && entryID(l) == id {
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
