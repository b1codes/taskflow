package completeness

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ResolvedStack is a manifest stack with its vars and commands picked for the
// project as it is right now. Nothing is executed.
type ResolvedStack struct {
	ID   string
	Root string
	Env  map[string]string
	Vars map[string]string
	// Commands maps every command name the stack declares (or the overrides
	// pin) to its command line; "" means no candidate resolved.
	Commands map[string]string
	// Missing is set when the stack root doesn't exist; such a stack is
	// skipped and its rules produce no obligations.
	Missing  bool
	Warnings []string
}

// CommandNames returns the stack's command names, sorted.
func (rs *ResolvedStack) CommandNames() []string {
	names := make([]string, 0, len(rs.Commands))
	for n := range rs.Commands {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ResolveStacks resolves every stack in m, in manifest order.
func ResolveStacks(projectPath string, m *Manifest, o *Overrides) []ResolvedStack {
	if o == nil {
		o = &Overrides{}
	}
	out := make([]ResolvedStack, 0, len(m.Stacks))
	for i := range m.Stacks {
		out = append(out, resolveStack(projectPath, &m.Stacks[i], o.Stacks[m.Stacks[i].ID]))
	}
	return out
}

func resolveStack(projectPath string, s *Stack, o StackOverride) ResolvedStack {
	rs := ResolvedStack{
		ID:       s.ID,
		Root:     s.Root,
		Env:      s.Env,
		Vars:     map[string]string{},
		Commands: map[string]string{},
	}
	root := filepath.Join(projectPath, filepath.FromSlash(s.Root))
	if fi, err := statInRoot(projectPath, s.Root); err != nil || !fi.IsDir() {
		rs.Missing = true
		rs.Warnings = append(rs.Warnings, fmt.Sprintf("stack %s skipped: root %q does not exist", s.ID, s.Root))
		return rs
	}
	cond := &conditionEnv{root: root}

	for _, name := range sortedKeys(s.Vars) {
		if v, ok := o.Vars[name]; ok {
			rs.Vars[name] = v
			continue
		}
		for _, c := range s.Vars[name] {
			if cond.holds(c.If) {
				rs.Vars[name] = c.Value
				break
			}
		}
	}
	// Override vars the manifest doesn't declare (e.g. an Xcode scheme).
	for name, v := range o.Vars {
		rs.Vars[name] = v
	}

	names := map[string]bool{}
	for n := range s.Commands {
		names[n] = true
	}
	for n := range o.Commands {
		names[n] = true
	}
	for _, name := range sortedKeys(names) {
		if pinned, ok := o.Commands[name]; ok && pinned != "" {
			rs.Commands[name] = pinned
			continue
		}
		var missing []string
		for _, c := range s.Commands[name] {
			if !cond.holds(c.If) {
				continue
			}
			line, unset := render(c.Run, rs.Vars)
			if len(unset) > 0 {
				missing = append(missing, unset...)
				continue
			}
			rs.Commands[name] = line
			break
		}
		if rs.Commands[name] == "" {
			rs.Commands[name] = ""
			rs.Warnings = append(rs.Warnings, unresolvedWarning(s.ID, name, missing))
		}
	}
	return rs
}

// unresolvedWarning names the override that would fix an unresolved command.
func unresolvedWarning(stackID, command string, missingVars []string) string {
	if len(missingVars) > 0 {
		var keys []string
		seen := map[string]bool{}
		for _, v := range missingVars {
			if !seen[v] {
				seen[v] = true
				keys = append(keys, fmt.Sprintf("stacks.%s.vars.%s", stackID, v))
			}
		}
		return fmt.Sprintf("No %q command for stack %s: set %s in %s", command, stackID, strings.Join(keys, " and "), OverridesPath)
	}
	return fmt.Sprintf("No %q command for stack %s: none of its candidates apply; pin one with stacks.%s.commands.%s in %s", command, stackID, stackID, command, OverridesPath)
}

// conditionEnv evaluates conditions against one stack root, reading
// package.json at most once.
type conditionEnv struct {
	root    string
	scripts map[string]any
	read    bool
}

func (e *conditionEnv) holds(c *Condition) bool {
	if c == nil {
		return true
	}
	if c.Exists != "" {
		matches, err := globFS(e.root, c.Exists)
		if err != nil || len(matches) == 0 {
			return false
		}
	}
	if c.HasScript != "" {
		if _, ok := e.packageScripts()[c.HasScript]; !ok {
			return false
		}
	}
	if fc := c.FileContains; fc != nil {
		data, err := readInRoot(e.root, fc.File)
		if err != nil || !strings.Contains(string(data), fc.Text) {
			return false
		}
	}
	if len(c.Any) > 0 {
		matched := false
		for i := range c.Any {
			if e.holds(&c.Any[i]) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for i := range c.All {
		if !e.holds(&c.All[i]) {
			return false
		}
	}
	return true
}

func (e *conditionEnv) packageScripts() map[string]any {
	if !e.read {
		e.read = true
		var pkg struct {
			Scripts map[string]any `json:"scripts"`
		}
		if data, err := readInRoot(e.root, "package.json"); err == nil {
			_ = json.Unmarshal(data, &pkg)
		}
		e.scripts = pkg.Scripts
	}
	return e.scripts
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
