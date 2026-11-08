package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/crazy-complete/carapace2spec/convert"
	spec "github.com/crazy-complete/carapace2spec/spec"
	"go.yaml.in/yaml/v3"
)

type specCmd struct {
	Name            string         `yaml:"name"`
	Description     string         `yaml:"description"`
	Aliases         []string       `yaml:"aliases"`
	Hidden          bool           `yaml:"hidden"`
	Group           string         `yaml:"group"`
	Standalone      bool           `yaml:"standalone"`
	Flags           map[string]any `yaml:"flags"`
	PersistentFlags map[string]any `yaml:"persistentflags"`
	Commands        []*specCmd     `yaml:"commands"`
}

var cases = []struct{ name, dir string }{
	{"grep", "completers/common/grep_completer"},
	{"kubectl", "completers/common/kubectl_completer"},
	{"uv", "completers/common/uv_completer"},
	{"fd", "completers/common/fd_completer"},
	{"git", "completers/common/git_completer"},
	{"age", "completers/common/age_completer"},
	{"bat", "completers/common/bat_completer"},
	{"bc", "completers/common/bc_completer"},
	{"docker", "completers/common/docker_completer"},
	{"gh", "completers/common/gh_completer"},
	{"go", "completers/common/go_completer"},
	{"helm", "completers/common/helm_completer"},
}

func carapaceBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("CARA_BIN")
	if bin == "" {
		bin = filepath.Clean(filepath.Join("..", "carapace-bin"))
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("carapace-bin checkout not found (set CARA_BIN)")
	}
	return bin
}

func flat(root *specCmd) map[string]*specCmd {
	d := map[string]*specCmd{root.Name: root}
	var walk func(cmds []*specCmd, parent string)
	walk = func(cmds []*specCmd, parent string) {
		for _, c := range cmds {
			key := parent + "/" + c.Name
			d[key] = c
			walk(c.Commands, key)
		}
	}
	walk(root.Commands, root.Name)
	return d
}

func allFlags(c *specCmd) map[string]any {
	m := map[string]any{}
	for k, v := range c.Flags {
		m[k] = v
	}
	for k, v := range c.PersistentFlags {
		m[k] = v
	}
	return m
}

func normFlag(v any) map[string]any {
	if s, ok := v.(string); ok {
		return map[string]any{"description": s}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func sortedAliases(a []string) string {
	s := make([]string, len(a))
	copy(s, a)
	sort.Strings(s)
	return strings.Join(s, "\x00")
}

func specParity(t *testing.T, g, o *specCmd) {
	t.Helper()
	tg, to := flat(g), flat(o)
	for k := range tg {
		if _, ok := to[k]; !ok {
			t.Errorf("missing command: %s", k)
		}
	}
	for k := range to {
		if _, ok := tg[k]; !ok {
			t.Errorf("extra command: %s", k)
		}
	}
	for k, gc := range tg {
		oc, ok := to[k]
		if !ok {
			continue
		}
		var diffs []string
		if gc.Description != oc.Description {
			diffs = append(diffs, fmt.Sprintf("description: %q vs %q", gc.Description, oc.Description))
		}
		if sortedAliases(gc.Aliases) != sortedAliases(oc.Aliases) {
			diffs = append(diffs, fmt.Sprintf("aliases: %v vs %v", gc.Aliases, oc.Aliases))
		}
		if gc.Hidden != oc.Hidden {
			diffs = append(diffs, fmt.Sprintf("hidden: %v vs %v", gc.Hidden, oc.Hidden))
		}
		if gc.Group != oc.Group {
			diffs = append(diffs, fmt.Sprintf("group: %q vs %q", gc.Group, oc.Group))
		}
		if gc.Standalone != oc.Standalone {
			diffs = append(diffs, fmt.Sprintf("standalone: %v vs %v", gc.Standalone, oc.Standalone))
		}
		fg, fo := allFlags(gc), allFlags(oc)
		keys := map[string]bool{}
		for k := range fg {
			keys[k] = true
		}
		for k := range fo {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, fk := range ks {
			switch {
			case !inMap(fg, fk):
				diffs = append(diffs, "flag missing: "+fk)
			case !inMap(fo, fk):
				diffs = append(diffs, "flag extra: "+fk)
			default:
				a, b := normFlag(fg[fk]), normFlag(fo[fk])
				fk2 := map[string]bool{}
				for k := range a {
					fk2[k] = true
				}
				for k := range b {
					fk2[k] = true
				}
				for k := range fk2 {
					if !reflect.DeepEqual(a[k], b[k]) {
						diffs = append(diffs, fmt.Sprintf("flag %s field %s: %v vs %v", fk, k, a[k], b[k]))
					}
				}
			}
		}
		if len(diffs) > 0 {
			t.Errorf("command %s:\n  %s", k, strings.Join(diffs, "\n  "))
		}
	}
}

func inMap(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

func TestSpecParity(t *testing.T) {
	bin := carapaceBin(t)
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			gold, err := os.ReadFile(filepath.Join("testdata", c.name, "expected.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var g specCmd
			if err := yaml.Unmarshal(gold, &g); err != nil {
				t.Fatal(err)
			}

			res, err := convert.Convert(filepath.Join(bin, c.dir))
			if err != nil {
				t.Fatal(err)
			}
			doc, err := spec.ToYAML(res.Spec)
			if err != nil {
				t.Fatal(err)
			}
			var o specCmd
			if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
				t.Fatal(err)
			}

			if g.Name != o.Name {
				t.Fatalf("root name: %q vs %q", g.Name, o.Name)
			}
			specParity(t, &g, &o)
		})
	}
}
