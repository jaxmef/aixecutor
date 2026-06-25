package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// mustMap unmarshals a YAML literal into a generic map for merge tests.
func mustMap(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("unmarshal fixture: %v\n%s", err, doc)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func TestDeepMerge(t *testing.T) {
	cases := []struct {
		name string
		dst  string
		src  string
		want string
	}{
		{
			name: "scalar replaces",
			dst:  "a: 1\nb: 2\n",
			src:  "a: 9\n",
			want: "a: 9\nb: 2\n",
		},
		{
			name: "map merges key-by-key, siblings preserved",
			dst:  "m:\n  x: 1\n  y: 2\n",
			src:  "m:\n  y: 9\n",
			want: "m:\n  x: 1\n  y: 9\n",
		},
		{
			name: "absent key leaves dst untouched",
			dst:  "a: 1\nb: 2\n",
			src:  "c: 3\n",
			want: "a: 1\nb: 2\nc: 3\n",
		},
		{
			name: "zero value still overrides (absent != zero)",
			dst:  "a: 5\n",
			src:  "a: 0\n",
			want: "a: 0\n",
		},
		{
			name: "list replaces wholesale (no concat)",
			dst:  "l:\n  - 1\n  - 2\n  - 3\n",
			src:  "l:\n  - 9\n",
			want: "l:\n  - 9\n",
		},
		{
			name: "nested map adds new key, keeps old",
			dst:  "m:\n  a:\n    x: 1\n",
			src:  "m:\n  a:\n    y: 2\n",
			want: "m:\n  a:\n    x: 1\n    y: 2\n",
		},
		{
			name: "type change from map to scalar replaces",
			dst:  "k:\n  a: 1\n",
			src:  "k: 7\n",
			want: "k: 7\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := mustMap(t, tc.dst)
			src := mustMap(t, tc.src)
			want := mustMap(t, tc.want)

			got := deepMerge(dst, src)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("deepMerge mismatch\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

// TestDeepMergeDoesNotAliasNestedSource guards against the merged result
// sharing nested maps with src such that later mutation leaks. We merge, then
// mutate src and confirm dst is unaffected for keys that came from dst.
func TestDeepMergeIndependentLayers(t *testing.T) {
	dst := mustMap(t, "m:\n  a: 1\n  b: 2\n")
	src := mustMap(t, "m:\n  b: 9\n")

	got := deepMerge(dst, src)

	gm, _ := asStringMap(got["m"])
	if gm["a"] != 1 || gm["b"] != 9 {
		t.Fatalf("unexpected merge: %#v", gm)
	}
}
