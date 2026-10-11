package completeness

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// varRef matches a {name} placeholder in a pattern or template.
var varRef = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_-]*)\}`)

// Pattern is a compiled path pattern. Patterns use "/" separators and are
// relative to a stack root (or the project root for rules without a stack):
//
//	{var}  captures one path-segment fragment (no "/")
//	*      matches within one segment
//	**     matches any number of whole segments (including none)
type Pattern struct {
	src  string
	re   *regexp.Regexp
	vars []string // capture name per regexp group, in order
}

// CompilePattern compiles src. A var may appear more than once; every
// occurrence must capture the same value for the pattern to match.
func CompilePattern(src string) (*Pattern, error) {
	if src == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	if strings.HasPrefix(src, "/") {
		return nil, fmt.Errorf("pattern %q must be relative", src)
	}
	p := &Pattern{src: src}
	var b strings.Builder
	b.WriteString("^")
	segs := strings.Split(src, "/")
	for _, seg := range segs {
		// Checks read files, so a manifest must not reach outside its root.
		if seg == ".." {
			return nil, fmt.Errorf("pattern %q must stay inside the project (no \"..\")", src)
		}
	}
	for i, seg := range segs {
		last := i == len(segs)-1
		if seg == "**" {
			if last {
				if i == 0 {
					b.WriteString(".*")
				} else {
					// "a/**" matches "a" itself and everything below it; the
					// separator written for the previous segment is optional.
					s := b.String()
					b.Reset()
					b.WriteString(strings.TrimSuffix(s, "/"))
					b.WriteString("(?:/.*)?")
				}
			} else {
				b.WriteString("(?:[^/]+/)*")
			}
			continue
		}
		if strings.Contains(seg, "**") {
			return nil, fmt.Errorf("pattern %q: ** must be a whole path segment", src)
		}
		if err := p.writeSegment(&b, seg); err != nil {
			return nil, err
		}
		if !last {
			b.WriteString("/")
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", src, err)
	}
	p.re = re
	return p, nil
}

func (p *Pattern) writeSegment(b *strings.Builder, seg string) error {
	for len(seg) > 0 {
		switch {
		case seg[0] == '*':
			b.WriteString("[^/]*")
			seg = seg[1:]
		case seg[0] == '{':
			loc := varRef.FindStringSubmatchIndex(seg)
			if loc == nil || loc[0] != 0 {
				return fmt.Errorf("pattern %q: malformed placeholder in %q", p.src, seg)
			}
			p.vars = append(p.vars, seg[loc[2]:loc[3]])
			b.WriteString("([^/]+)")
			seg = seg[loc[1]:]
		default:
			n := strings.IndexAny(seg, "*{")
			if n < 0 {
				n = len(seg)
			}
			b.WriteString(regexp.QuoteMeta(seg[:n]))
			seg = seg[n:]
		}
	}
	return nil
}

// Vars returns the distinct capture names in the pattern.
func (p *Pattern) Vars() []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range p.vars {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Match reports whether rel matches, with the captured vars.
func (p *Pattern) Match(rel string) (map[string]string, bool) {
	m := p.re.FindStringSubmatch(rel)
	if m == nil {
		return nil, false
	}
	vars := make(map[string]string, len(p.vars))
	for i, name := range p.vars {
		val := m[i+1]
		if prev, ok := vars[name]; ok && prev != val {
			return nil, false
		}
		vars[name] = val
	}
	return vars, true
}

func (p *Pattern) String() string { return p.src }

// render substitutes {var} placeholders from vars. It returns the names of
// placeholders that had no value, leaving those placeholders in place.
func render(tmpl string, vars map[string]string) (string, []string) {
	var missing []string
	out := varRef.ReplaceAllStringFunc(tmpl, func(ref string) string {
		name := ref[1 : len(ref)-1]
		if v, ok := vars[name]; ok {
			return v
		}
		missing = append(missing, name)
		return ref
	})
	return out, missing
}

// templateVars returns the placeholder names used in tmpl.
func templateVars(tmpl string) []string {
	var out []string
	for _, m := range varRef.FindAllStringSubmatch(tmpl, -1) {
		out = append(out, m[1])
	}
	return out
}

// builtinVars returns the vars every file-scoped rule gets from its subject:
// {path} (relative to the rule's root), {dir}, and {stem}.
func builtinVars(rel string) map[string]string {
	base := path.Base(rel)
	stem := base
	if i := strings.Index(base, "."); i > 0 {
		stem = base[:i]
	}
	return map[string]string{
		"path": rel,
		"dir":  path.Dir(rel),
		"stem": stem,
	}
}

// hasGlob reports whether pattern needs matching rather than a direct stat.
func hasGlob(pattern string) bool {
	return strings.Contains(pattern, "*")
}

// skippedDirs are never descended into while globbing the filesystem, unless
// the pattern names them explicitly; they are large and never hold artifacts
// a rule cares about.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, ".b1codes": true}

// readInRoot reads rel ("/"-separated) from inside root. Paths rendered from
// templates can pick up ".." from stack vars, and a cloned repo can hold
// symlinks to anywhere, so reads go through os.Root, which refuses to leave
// root either way.
func readInRoot(root, rel string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.ReadFile(filepath.FromSlash(rel))
}

// statInRoot is os.Stat confined to root, like readInRoot.
func statInRoot(root, rel string) (fs.FileInfo, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Stat(filepath.FromSlash(rel))
}

// globFS returns the paths under root (relative, "/"-separated, sorted) that
// match pattern, including directories. Walking starts at the pattern's
// literal prefix, so "src/features/*/pages/*.tsx" only reads src/features.
// Like readInRoot, it never looks outside root.
func globFS(root, pattern string) ([]string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil
	}
	defer r.Close()
	if !hasGlob(pattern) {
		if _, err := r.Stat(filepath.FromSlash(pattern)); err != nil {
			return nil, nil
		}
		return []string{pattern}, nil
	}
	p, err := CompilePattern(pattern)
	if err != nil {
		return nil, err
	}
	segs := strings.Split(pattern, "/")
	var prefix []string
	for _, s := range segs {
		if hasGlob(s) {
			break
		}
		prefix = append(prefix, s)
	}
	start := "."
	if len(prefix) > 0 {
		start = strings.Join(prefix, "/")
	}
	explicit := map[string]bool{}
	for _, s := range segs {
		explicit[s] = true
	}
	var out []string
	err = fs.WalkDir(r.FS(), start, func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			if rel == start {
				return fs.SkipDir
			}
			return nil
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() && skippedDirs[d.Name()] && !explicit[d.Name()] {
			return fs.SkipDir
		}
		if _, ok := p.Match(rel); ok {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
