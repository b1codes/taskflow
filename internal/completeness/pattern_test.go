package completeness

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPattern_Match(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		match   bool
		caps    map[string]string
	}{
		{"src/features/{feature}/pages/{name}.tsx", "src/features/payments/pages/CheckoutPage.tsx", true,
			map[string]string{"feature": "payments", "name": "CheckoutPage"}},
		{"src/features/{feature}/pages/{name}.tsx", "src/features/payments/pages/sub/X.tsx", false, nil},
		{"src/features/{feature}/pages/{name}.tsx", "src/features/payments/pages/X.ts", false, nil},
		{"src/core/components/{name}/{name}.tsx", "src/core/components/Button/Button.tsx", true,
			map[string]string{"name": "Button"}},
		{"src/core/components/{name}/{name}.tsx", "src/core/components/Button/Card.tsx", false, nil},
		{"**/*.stories.tsx", "a/b/C.stories.tsx", true, map[string]string{}},
		{"**/*.stories.tsx", "C.stories.tsx", true, map[string]string{}},
		{"**/*.stories.tsx", "C.tsx", false, nil},
		{"e2e/**/*.spec.ts", "e2e/checkout.spec.ts", true, map[string]string{}},
		{"e2e/**/*.spec.ts", "e2e/flows/pay/checkout.spec.ts", true, map[string]string{}},
		{"e2e/**/*.spec.ts", "e2ex/checkout.spec.ts", false, nil},
		{"tests/contract/**", "tests/contract/payments/api.test.ts", true, map[string]string{}},
		{"tests/contract/**", "tests/contract", true, map[string]string{}},
		{"tests/contract/**", "tests/contractx/a.ts", false, nil},
		{"**", "any/path/at/all.go", true, map[string]string{}},
		{"src/*.ts", "src/a.ts", true, map[string]string{}},
		{"src/*.ts", "src/a/b.ts", false, nil},
		{"src/use*.ts", "src/useCart.ts", true, map[string]string{}},
		{"lib/{name}.dart", "lib/a.b.dart", true, map[string]string{"name": "a.b"}},
		{"a+b/{x}.(1).txt", "a+b/y.(1).txt", true, map[string]string{"x": "y"}},
	}
	for _, tt := range tests {
		p, err := CompilePattern(tt.pattern)
		if err != nil {
			t.Fatalf("CompilePattern(%q): %v", tt.pattern, err)
		}
		caps, ok := p.Match(tt.path)
		if ok != tt.match {
			t.Errorf("%q vs %q: match = %v, want %v", tt.pattern, tt.path, ok, tt.match)
			continue
		}
		if ok && !maps.Equal(caps, tt.caps) {
			t.Errorf("%q vs %q: captures = %v, want %v", tt.pattern, tt.path, caps, tt.caps)
		}
	}
}

func TestCompilePattern_Errors(t *testing.T) {
	for _, src := range []string{"", "/abs/path", "src/a**b", "src/{unclosed", "../outside.txt", "src/../../etc/passwd"} {
		if _, err := CompilePattern(src); err == nil {
			t.Errorf("CompilePattern(%q): expected an error", src)
		}
	}
}

func TestRender(t *testing.T) {
	out, missing := render("{feature}/{name}.stories.tsx in {nope}", map[string]string{"feature": "pay", "name": "Checkout"})
	if out != "pay/Checkout.stories.tsx in {nope}" {
		t.Errorf("render = %q", out)
	}
	if !slices.Equal(missing, []string{"nope"}) {
		t.Errorf("missing = %v", missing)
	}
}

func TestBuiltinVars(t *testing.T) {
	got := builtinVars("src/pages/CheckoutPage.tsx")
	want := map[string]string{"path": "src/pages/CheckoutPage.tsx", "dir": "src/pages", "stem": "CheckoutPage"}
	if !maps.Equal(got, want) {
		t.Errorf("builtinVars = %v, want %v", got, want)
	}
}

func TestGlobFS(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"src/a.ts":                 "",
		"src/deep/b.ts":            "",
		"node_modules/pkg/c.ts":    "",
		"App.xcodeproj/project.pb": "",
	})

	got, err := globFS(root, "**/*.ts")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/a.ts", "src/deep/b.ts"}; !slices.Equal(got, want) {
		t.Errorf("**/*.ts = %v, want %v (node_modules skipped)", got, want)
	}
	got, _ = globFS(root, "node_modules/**/*.ts")
	if want := []string{"node_modules/pkg/c.ts"}; !slices.Equal(got, want) {
		t.Errorf("explicit node_modules = %v, want %v", got, want)
	}
	got, _ = globFS(root, "*.xcodeproj")
	if want := []string{"App.xcodeproj"}; !slices.Equal(got, want) {
		t.Errorf("*.xcodeproj = %v, want %v (directories match)", got, want)
	}
	got, _ = globFS(root, "src/a.ts")
	if want := []string{"src/a.ts"}; !slices.Equal(got, want) {
		t.Errorf("literal = %v, want %v", got, want)
	}
	got, _ = globFS(root, "missing/*.ts")
	if len(got) != 0 {
		t.Errorf("missing dir = %v, want none", got)
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGlobFS_StaysInRoot(t *testing.T) {
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"secret.txt": "s"})
	root := filepath.Join(t.TempDir(), "proj")
	writeFiles(t, root, map[string]string{"src/a.ts": ""})
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "secret.txt")); err != nil {
		t.Fatal(err)
	}

	for _, pattern := range []string{"link/secret.txt", "link/*.txt", "secret.txt", "../" + filepath.Base(outside) + "/secret.txt"} {
		if got, _ := globFS(root, pattern); len(got) != 0 {
			t.Errorf("globFS(%q) = %v, want nothing outside root", pattern, got)
		}
	}
	for _, rel := range []string{"link/secret.txt", "secret.txt", "../proj/../../" + filepath.Base(outside) + "/secret.txt"} {
		if _, err := readInRoot(root, rel); err == nil {
			t.Errorf("readInRoot(%q) read a file outside root", rel)
		}
	}
}
