package completeness

import (
	"slices"
	"testing"
)

// An absent: pattern names a file that must go, so Expected must not list it
// as a bare path: every view shows Expected as "expected: <path>", which
// would steer the agent to create the file it should delete.
func TestExpected_AbsentIsNotAnExpectedPath(t *testing.T) {
	const manifest = `
version: 1
rules:
  - id: screen-deleted
    describe: "Deleted screen"
    when: { deleted: "src/screens/{name}.tsx" }
    require:
      - id: stories-removed
        describe: "Remove the stories for {name}"
        verify: { absent: "src/screens/{name}.stories.tsx" }
      - id: moved
        describe: "Replace {name} with its successor"
        verify:
          all_of:
            - { exists: "src/screens/{name}V2.tsx" }
            - { absent: "src/screens/{name}.test.tsx" }
`
	files := map[string]string{
		"src/screens/Foo.stories.tsx": "export default {}",
		"src/screens/Foo.test.tsx":    "",
	}
	res := evalFixture(t, manifest, files, []Change{{Path: "src/screens/Foo.tsx", Type: Deleted}})
	got := map[string]Finding{}
	for _, f := range res.Findings {
		got[f.RequirementID] = f
	}

	stories, ok := got["stories-removed"]
	if !ok || stories.Satisfied {
		t.Fatalf("stories-removed = %+v (findings %+v)", stories, res.Findings)
	}
	if want := []string{"must not exist: src/screens/Foo.stories.tsx"}; !slices.Equal(stories.Expected, want) {
		t.Errorf("stories-removed expected = %q, want %q", stories.Expected, want)
	}

	moved, ok := got["moved"]
	if !ok || moved.Satisfied {
		t.Fatalf("moved = %+v (findings %+v)", moved, res.Findings)
	}
	if want := []string{"src/screens/FooV2.tsx", "must not exist: src/screens/Foo.test.tsx"}; !slices.Equal(moved.Expected, want) {
		t.Errorf("moved expected = %q, want %q", moved.Expected, want)
	}

	for _, f := range res.Findings {
		for _, e := range f.Expected {
			if e == "src/screens/Foo.stories.tsx" || e == "src/screens/Foo.test.tsx" {
				t.Errorf("%s lists absent path %q as an expected path", f.RequirementID, e)
			}
		}
	}
}
