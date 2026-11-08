package convert

import (
	"go/ast"
	"go/types"
	"sort"
	"strings"

	"github.com/carapace-sh/carapace/pkg/command"
)

// addFlags collects local or persistent pflag registrations into out.
func (c *conv) addFlags(out *command.Command, i *instance, persistent bool, info *types.Info) {
	list := c.flags[i.id]
	if persistent {
		list = c.pflags[i.id]
	}
	if len(list) == 0 {
		return
	}

	type slot struct {
		flag command.Flag
		key  string
		node ast.Node
	}
	slots := make([]slot, 0, len(list))
	byName := map[string]int{} // flag name (longhand or shorthand) -> slot index

	for _, reg := range list {
		f, ok := c.buildFlag(reg, info)
		if !ok {
			continue
		}
		if c.optargs[i.id] != nil {
			if c.optargs[i.id][f.Longhand] || (f.Longhand == "" && c.optargs[i.id][f.Shorthand]) {
				f.Optarg = true
			}
		}
		key := flagKey(f)
		if _, dup := byName[f.Name()]; dup {
			c.warnAt(reg.node, "flag "+f.Name()+" is registered twice")
			continue
		}
		idx := len(slots)
		slots = append(slots, slot{flag: f, key: key, node: reg.node})
		if f.Longhand != "" {
			byName[f.Longhand] = idx
		}
		if f.Shorthand != "" {
			byName[f.Shorthand] = idx
		}
	}

	// cobra marks
	for _, m := range c.marks[i.id] {
		switch m.kind {
		case "required", "hidden":
			var names []string
			if m.name != "" {
				names = []string{m.name}
			} else {
				var ok bool
				names, ok = constNames(m.args, info)
				if !ok {
					c.warnAt(m.node, m.kind+": non-static flag names")
					continue
				}
			}
			for _, n := range names {
				idx, found := byName[n]
				if !found {
					c.warnAt(m.node, m.kind+": flag "+n+" is not registered")
					continue
				}
				f := slots[idx].flag
				if m.kind == "required" {
					f.Required = true
				} else {
					f.Hidden = true
				}
				slots[idx] = slot{flag: f, key: flagKey(f), node: slots[idx].node}
			}
		case "filename", "directory":
			if len(m.args) > 0 {
				if s, ok := constString(m.args[0], info); ok {
					c.warnAt(m.node, "MarkFlag"+strings.Title(m.kind)+"("+s+") is runtime behaviour; "+
						"model it via flag completion instead")
					continue
				}
			}
			c.warnAt(m.node, "MarkFlag"+strings.Title(m.kind)+" is runtime behaviour; "+
				"model it via flag completion instead")
		case "mutually_exclusive":
			names, ok := constNames(m.args, info)
			if !ok {
				c.warnAt(m.node, "MarkFlagsMutuallyExclusive: non-static names")
				continue
			}
			if len(names) < 2 {
				c.warnAt(m.node, "MarkFlagsMutuallyExclusive needs at least two flags")
				continue
			}
			resolved := make([]string, 0, len(names))
			miss := false
			for _, n := range names {
				if _, found := byName[n]; !found {
					miss = true
					break
				}
				resolved = append(resolved, n)
			}
			if miss {
				c.warnAt(m.node, "MarkFlagsMutuallyExclusive: unregistered flag in group")
				continue
			}
			sort.Strings(resolved)
			out.ExclusiveFlags = append(out.ExclusiveFlags, resolved)
		}
	}

	fs := make(command.FlagSet)
	for _, s := range slots {
		fs[s.key] = s.flag
	}
	if persistent {
		out.PersistentFlags = fs
	} else {
		out.Flags = fs
	}
}

// addCompletion converts the carapace completion registrations of an instance.
func (c *conv) addCompletion(out *command.Command, i *instance, info *types.Info) {
	if len(c.completions[i.id]) == 0 && c.standalone[i.id] {
		c.warnAt(nil, "Standalone() is runtime behaviour and has no spec equivalent")
	}
	if len(c.completions[i.id]) == 0 {
		return
	}
	ac := &actionConverter{pkg: i.pkg, info: info, c: c}

	for _, comp := range c.completions[i.id] {
		switch comp.kind {
		case "flag":
			if len(comp.args) == 0 {
				continue
			}
			al, ok := comp.args[0].(*ast.CompositeLit)
			if !ok {
				c.warnAt(comp.args[0], "FlagCompletion: static carapace.ActionMap literal expected")
				continue
			}
			for _, el := range al.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					c.warnAt(el, "FlagCompletion: map entry without key")
					continue
				}
				key, ok := constString(kv.Key, info)
				if !ok || key == "" {
					c.warnAt(kv.Key, "FlagCompletion: non-static flag key")
					continue
				}
				entries := ac.convertAction(kv.Value)
				if out.Completion.Flag == nil {
					out.Completion.Flag = map[string][]string{}
				}
				out.Completion.Flag[key] = entries
			}

		case "position":
			pos := make([][]string, 0, len(comp.args))
			for _, a := range comp.args {
				pos = append(pos, ac.convertAction(a))
			}
			out.Completion.Positional = pos

		case "posany":
			if len(comp.args) > 0 {
				out.Completion.PositionalAny = ac.convertAction(comp.args[0])
			}

		case "dash":
			for _, a := range comp.args {
				out.Completion.Dash = append(out.Completion.Dash, ac.convertAction(a))
			}

		case "dashany":
			if len(comp.args) > 0 {
				out.Completion.DashAny = ac.convertAction(comp.args[0])
			}
		}
	}
}
