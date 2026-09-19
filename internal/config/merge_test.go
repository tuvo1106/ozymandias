package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// frag is a stand-in config with the shapes conf.d merging must handle.
type frag struct {
	Name   string   `yaml:"name"`
	Tags   []string `yaml:"tags"`
	Nested struct {
		A    string   `yaml:"a"`
		B    string   `yaml:"b"`
		List []string `yaml:"list"`
	} `yaml:"nested"`
}

func loadFrag(t *testing.T, files map[string]string) (frag, error) {
	t.Helper()
	dir := testutil.TempDirWith(t, files)
	var f frag
	opts := Options{FragmentDir: dir + "/conf.d"}
	if _, ok := files["main.yaml"]; ok {
		opts.Path = dir + "/main.yaml"
	}
	_, err := Load(&f, opts)
	return f, err
}

func TestFragments_ListsAppendInFileNameOrder(t *testing.T) {
	f, err := loadFrag(t, map[string]string{
		"main.yaml":        "tags: [env:dev]\n",
		"conf.d/b.yaml":    "tags: [app:b]\n",
		"conf.d/a.yml":     "tags: [app:a]\nnested: {list: [x]}\n",
		"conf.d/c.yaml":    "nested: {list: [y]}\n",
		"conf.d/README.md": "ignored: true\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"env:dev", "app:a", "app:b"}; !reflect.DeepEqual(f.Tags, want) {
		t.Errorf("tags = %v, want %v", f.Tags, want)
	}
	if want := []string{"x", "y"}; !reflect.DeepEqual(f.Nested.List, want) {
		t.Errorf("nested.list = %v, want %v", f.Nested.List, want)
	}
}

func TestFragments_MapsMergeKeyByKey(t *testing.T) {
	f, err := loadFrag(t, map[string]string{
		"main.yaml":     "nested: {a: from-main}\n",
		"conf.d/x.yaml": "nested: {b: from-fragment}\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.Nested.A != "from-main" || f.Nested.B != "from-fragment" {
		t.Fatalf("nested = %+v", f.Nested)
	}
}

// Last-writer-wins would make the result depend on file names; a conflict
// must name both files so the operator can fix it.
func TestFragments_ScalarSetTwiceIsAnErrorNamingBothFiles(t *testing.T) {
	_, err := loadFrag(t, map[string]string{
		"main.yaml":     "nested: {a: one}\n",
		"conf.d/x.yaml": "nested: {a: two}\n",
	})
	if err == nil || !strings.Contains(err.Error(), "main.yaml") || !strings.Contains(err.Error(), "x.yaml") ||
		!strings.Contains(err.Error(), "nested.a") {
		t.Fatalf("err = %v, want conflict on nested.a naming both files", err)
	}
}

func TestFragments_ShapeMismatchesAreErrors(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"list over scalar": {"conf.d/1.yaml": "name: x\n", "conf.d/2.yaml": "name: [y]\n"},
		"map over scalar":  {"conf.d/1.yaml": "name: x\n", "conf.d/2.yaml": "name: {y: 1}\n"},
	} {
		// The strict pass rejects these as type errors before merging even
		// starts; either way the load must fail.
		if _, err := loadFrag(t, files); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// deepMerge is also exercised directly: the strict pass normally catches
// shape mismatches first, but merge must not silently clobber if it doesn't.
func TestDeepMerge_ReportsMismatchedShapes(t *testing.T) {
	origins := map[string]string{"k": "first.yaml"}
	if err := deepMerge(map[string]any{"k": "scalar"}, map[string]any{"k": []any{1}}, "", "second.yaml", origins); err == nil {
		t.Error("list over scalar: want error")
	}
	if err := deepMerge(map[string]any{"k": "scalar"}, map[string]any{"k": map[string]any{}}, "", "second.yaml", origins); err == nil {
		t.Error("map over scalar: want error")
	}
	nested := map[string]any{"m": map[string]any{"a": 1}}
	if err := deepMerge(nested, map[string]any{"m": map[string]any{"a": 2}}, "", "second.yaml", map[string]string{}); err == nil {
		t.Error("nested scalar conflict: want error")
	}
}

func TestFragments_UnknownKeyInFragmentIsAnError(t *testing.T) {
	_, err := loadFrag(t, map[string]string{"conf.d/typo.yaml": "tagz: [a]\n"})
	if err == nil || !strings.Contains(err.Error(), "typo.yaml") {
		t.Fatalf("err = %v, want one naming typo.yaml", err)
	}
}

func TestFragments_MissingDirectoryMeansNone(t *testing.T) {
	var f frag
	if _, err := Load(&f, Options{FragmentDir: t.TempDir() + "/nope"}); err != nil {
		t.Fatal(err)
	}
}

func TestFragments_UnreadableDirectoryIsAnError(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{"file": "x"})
	var f frag
	if _, err := Load(&f, Options{FragmentDir: dir + "/file"}); err == nil {
		t.Fatal("fragment dir that is a file: want error")
	}
}

// Review finding: a file holding only comments (a placeholder fragment, or a
// main file with everything commented out) must load as empty, not fail
// with a bare "EOF".
func TestLoad_CommentOnlyFilesAreEmpty(t *testing.T) {
	f, err := loadFrag(t, map[string]string{
		"main.yaml":               "# nothing set yet\n",
		"conf.d/placeholder.yaml": "# app config goes here\n# tags: [app:x]\n",
		"conf.d/real.yaml":        "tags: [app:real]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Tags) != 1 || f.Tags[0] != "app:real" {
		t.Fatalf("tags = %v", f.Tags)
	}
}

// Review finding: `tags:` with no items (e.g. all commented out) is null in
// YAML; a later fragment's list must still append, and a null in a later
// file must not clobber or conflict.
func TestFragments_NullValuesMergeAsAbsent(t *testing.T) {
	f, err := loadFrag(t, map[string]string{
		"main.yaml":     "tags:\n#  - env:dev\nname:\n",
		"conf.d/a.yaml": "tags: [app:a]\nname: from-a\n",
		"conf.d/b.yaml": "tags:\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Tags) != 1 || f.Tags[0] != "app:a" || f.Name != "from-a" {
		t.Fatalf("got %+v", f)
	}
}
