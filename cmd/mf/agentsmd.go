package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	blockBegin = "<!-- mf:begin -->"
	blockEnd   = "<!-- mf:end -->"
)

// agentBlock is the marked section mf maintains in AGENTS.md / CLAUDE.md.
const agentBlock = blockBegin + `
## Task tracking (mf / ManyForge Work Board)

This repo tracks work with ` + "`mf`" + `. Use it for ALL task tracking — not TodoWrite,
TaskCreate, or markdown TODO lists.

- ` + "`mf ready`" + ` — unblocked agent work; ` + "`mf show <key>`" + ` for details
- ` + "`mf claim <key>`" + ` before you start; ` + "`mf close <key> --reason \"…\"`" + ` when done
- ` + "`mf create \"title\" [-p 0-4] [-t bug] [--blocked-by <key>]`" + ` — prints the new key
- ` + "`mf note <key> \"text\"`" + ` — append progress notes

**Need a human?** When something needs a person — credentials, approvals, DNS,
payments, manual checks you can't automate — run
` + "`mf create --operator \"what to do and why\"`" + ` and continue with other work.

Run ` + "`mf prime`" + ` for the full workflow context.
` + blockEnd

// upsertBlock replaces the marked block in content, or appends it after a
// blank line. It is idempotent.
func upsertBlock(content, block string) (string, error) {
	begin := strings.Index(content, blockBegin)
	if begin < 0 {
		trimmed := strings.TrimRight(content, "\n")
		if trimmed == "" {
			return block + "\n", nil
		}
		return trimmed + "\n\n" + block + "\n", nil
	}
	end := strings.Index(content[begin:], blockEnd)
	if end < 0 {
		return "", fmt.Errorf("found %s without a matching %s", blockBegin, blockEnd)
	}
	end += begin + len(blockEnd)
	return content[:begin] + block + content[end:], nil
}

// upsertAgentDocs writes the block into whichever of AGENTS.md / CLAUDE.md
// exist at root (creating AGENTS.md if neither does) and returns the files it
// changed. Symlinks are resolved so a CLAUDE.md → AGENTS.md link is written
// through, not replaced.
func upsertAgentDocs(root string) ([]string, error) {
	var targets []string
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		real, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := insideRoot(root, real); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !slices.Contains(targets, real) {
			targets = append(targets, real)
		}
	}
	if len(targets) == 0 {
		targets = []string{filepath.Join(root, "AGENTS.md")}
	}
	var changed []string
	for _, path := range targets {
		perm := os.FileMode(0o644)
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return changed, err
		}
		if fi, err := os.Stat(path); err == nil {
			perm = fi.Mode().Perm()
		}
		next, err := upsertBlock(string(data), agentBlock)
		if err != nil {
			return changed, fmt.Errorf("%s: %w", path, err)
		}
		if next == string(data) {
			continue
		}
		if err := writeFileAtomic(path, []byte(next), perm); err != nil {
			return changed, err
		}
		changed = append(changed, path)
	}
	return changed, nil
}

const primeCommand = "mf prime"

// installClaudeHooks merges `mf prime` into the SessionStart and PreCompact
// hooks of a Claude Code settings.json, preserving every other key and hook.
// It reports whether the file changed.
func installClaudeHooks(root, path string) (bool, error) {
	if err := insideRootIfExists(root, path); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	next, changed, err := mergeClaudeHooks(data)
	if err != nil || !changed {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, writeFileAtomic(path, next, 0o644)
}

func mergeClaudeHooks(data []byte) ([]byte, bool, error) {
	settings := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return nil, false, fmt.Errorf("parse settings.json: %w", err)
		}
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if settings["hooks"] != nil && !ok {
		return nil, false, errors.New("settings.json: \"hooks\" is not an object")
	}
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	for _, event := range []string{"SessionStart", "PreCompact"} {
		entries, ok := hooks[event].([]any)
		if hooks[event] != nil && !ok {
			return nil, false, fmt.Errorf("settings.json: hooks.%s is not an array", event)
		}
		if hasPrimeHook(entries) {
			continue
		}
		hooks[event] = append(entries, map[string]any{
			"matcher": "",
			"hooks":   []any{map[string]any{"type": "command", "command": primeCommand}},
		})
		changed = true
	}
	if !changed {
		return data, false, nil
	}
	settings["hooks"] = hooks
	out, err := json.MarshalIndent(settings, "", "  ")
	return append(out, '\n'), true, err
}

func hasPrimeHook(entries []any) bool {
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			hook, _ := h.(map[string]any)
			if cmd, _ := hook["command"].(string); strings.HasPrefix(strings.TrimSpace(cmd), primeCommand) {
				return true
			}
		}
	}
	return false
}

// insideRoot rejects a resolved path outside the repo root. A cloned repo
// controls its own symlinks, so following CLAUDE.md -> ~/.bashrc (or a
// .claude -> ~ link) would turn `mf init` into a write anywhere in $HOME.
func insideRoot(root, real string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("resolves outside the repository (%s); refusing to write", real)
	}
	return nil
}

// insideRootIfExists applies insideRoot to the deepest existing ancestor of
// path (path itself when it exists), so a symlinked parent directory is
// caught before MkdirAll/write follow it.
func insideRootIfExists(root, path string) error {
	p := path
	for {
		if _, err := os.Lstat(p); err == nil {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return err
			}
			return insideRoot(root, real)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}
