package convert

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/carapace-sh/carapace/pkg/command"
	"golang.org/x/tools/go/packages"
)

// warnAt records a conversion warning anchored to a source position.
func (c *conv) warnAt(node ast.Node, message string) {
	w := &Warning{Message: message}
	if node != nil {
		pos := c.fset.Position(node.Pos())
		if pos.IsValid() {
			w.Position = pos.Filename + ":" + strconv.Itoa(pos.Line) + ":" + strconv.Itoa(pos.Column)
			if line := c.sourceLine(pos.Filename, pos.Line); line != "" {
				w.Snippet = strings.TrimSpace(line)
			}
		}
	}
	c.result.Warnings = append(c.result.Warnings, w)
}

var lineCache = map[string][]string{}

// sourceLine returns line n (1-based) of a source file, best effort.
func (c *conv) sourceLine(filename string, line int) string {
	if lines, ok := lineCache[filename]; ok {
		if line-1 < len(lines) {
			return lines[line-1]
		}
		return ""
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	lineCache[filename] = lines
	if line-1 < len(lines) {
		return lines[line-1]
	}
	return ""
}

// selectRoot picks the completer root command.
func (c *conv) selectRoot() (*instance, error) {
	children := map[string]bool{}
	for _, set := range c.children {
		for _, id := range set {
			children[id] = true
		}
	}
	var roots []*instance
	add := func(id string) {
		if i, ok := c.insts[id]; ok && !children[id] {
			roots = append(roots, i)
		}
	}
	for id := range c.genRoots {
		add(id)
	}
	if len(roots) == 0 {
		for id := range c.insts {
			add(id)
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("no cobra command found")
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].pkg.PkgPath != roots[j].pkg.PkgPath {
			return roots[i].pkg.PkgPath < roots[j].pkg.PkgPath
		}
		return roots[i].varname < roots[j].varname
	})
	if len(roots) > 1 {
		c.warnAt(roots[1].node, fmt.Sprintf("%d completer roots found; converting %q", len(roots), roots[0].varname))
		if os.Getenv("CRA_DEBUG_ROOTS") != "" {
			for _, i := range roots {
				fmt.Fprintf(os.Stderr, "root candidate: %s (%s)\n", i.varname, i.pkg.PkgPath)
			}
			for p, ch := range c.children {
				if len(ch) > 0 {
					fmt.Fprintf(os.Stderr, "edges: parent=%s children=%v\n", p, ch)
				}
			}
			for id := range c.genRoots {
				fmt.Fprintf(os.Stderr, "genroot: %s inchildren=%v\n", id, children[id])
			}
		}
	}
	return roots[0], nil
}

// resolve returns the composite literal backing an instance variable,
// following builder aliases.
func (c *conv) resolve(i *instance) *ast.CompositeLit {
	seen := map[*instance]bool{}
	cur, depth := i, 0
	for depth < 5 {
		seen[cur] = true
		if cur.lit != nil {
			return cur.lit
		}
		var next *instance
		if cur.borrow != nil {
			if bi := c.byFunc[cur.borrow]; bi != nil {
				next = bi
			}
		}
		if next == nil && cur.expr != nil {
			e := *cur.expr
			if cl, ok := unwrapComposite(e); ok && isCommandExpr(cl, cur.pkg.TypesInfo) {
				return cl
			}
			if ce, ok := e.(*ast.CallExpr); ok {
				if f := funcOf(ce, cur.pkg.TypesInfo); f != nil {
					if bd := c.builderFor(cur.pkg, f.Name()); bd != nil {
						if bi := c.byFunc[bd]; bi != nil {
							next = bi
						}
					}
				}
			}
		}
		if next == nil || seen[next] {
			break
		}
		cur, depth = next, depth+1
	}
	c.warnAt(i.node, "could not resolve static command fields for "+orName(i.varname))
	return nil
}

func orName(s string) string {
	if s == "" {
		return "this command"
	}
	return s
}

// unwrapComposite returns the *cobra.Command composite literal behind expr,
// unwrapping an address-of (&cobra.Command{...}).
func unwrapComposite(e ast.Expr) (*ast.CompositeLit, bool) {
	if ue, ok := e.(*ast.UnaryExpr); ok && ue.Op == token.AND {
		if cl, ok2 := ue.X.(*ast.CompositeLit); ok2 {
			return cl, true
		}
	}
	if cl, ok := e.(*ast.CompositeLit); ok {
		return cl, true
	}
	return nil, false
}

// isDeprecated reports whether the command literal carries a Deprecated value.
func (c *conv) isDeprecated(i *instance) bool {
	lit := c.resolve(i)
	if lit == nil {
		return false
	}
	for _, kv := range lit.Elts {
		kv2, ok := kv.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		id, ok := kv2.Key.(*ast.Ident)
		if !ok || id.Name != "Deprecated" {
			continue
		}
		if s, ok := constString(kv2.Value, i.pkg.TypesInfo); ok && s != "" {
			return true
		}
	}
	return false
}

// build assembles the command tree.
func (c *conv) build(i *instance, depth int, visited map[string]bool) command.Command {
	var out command.Command
	info := i.pkg.TypesInfo

	if lit := c.resolve(i); lit != nil {
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			id, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch id.Name {
			case "Use":
				if s, ok := constString(kv.Value, info); ok && s != "" {
					out.Name = s
					c.usedNames[i.id] = strings.Fields(s)[0]
				}
			case "Short":
				if s, ok := constString(kv.Value, info); ok {
					out.Description = s
				}
			case "Long":
				if s, ok := constString(kv.Value, info); ok {
					out.Documentation.Command = s
				}
			case "GroupID":
				if s, ok := constString(kv.Value, info); ok && s != "" {
					out.Group = s
				} else if g := c.groupIndex(kv.Value, i.pkg, info); g != "" {
					out.Group = g
				}
			case "Hidden":
				if b, ok := constBool(kv.Value, info); ok && b {
					out.Hidden = true
				} else {
					c.warnAt(kv.Value, "Hidden: non-static value")
				}
			case "Aliases":
				if vals, ok := constStrings(kv.Value, info); ok {
					out.Aliases = vals
				} else {
					c.warnAt(kv.Value, "Aliases: non-static list")
				}
			case "Deprecated":
				if s, ok := constString(kv.Value, info); ok && s != "" {
					out.Hidden = true
				}
			}
		}
	}
	if out.Name == "" {
		out.Name = strings.TrimSuffix(strings.ToLower(i.varname), "cmd")
	}

	// persistent flags first, then local flags
	c.addFlags(&out, i, true, info)
	c.addFlags(&out, i, false, info)

	// parsing mode
	if b, ok := c.parsing[i.id]; ok {
		if b {
			out.Parsing = "interspersed"
		} else {
			out.Parsing = "non-interspersed"
		}
	}

	// completion
	c.addCompletion(&out, i, info)

	// subcommands
	if depth < 10 && !visited[i.id] {
		visited[i.id] = true
		for _, ch := range c.children[i.id] {
			if child, ok := c.insts[ch]; ok {
				if c.isDeprecated(child) {
					continue
				}
				if _, okUse := c.staticUse(child); !okUse {
					c.warnAt(child.node, "dynamic command (non-static Use): omitted from spec")
					continue
				}
				name := c.rootName(child)
				if name == "_carapace" || name == "carapace" {
					continue
				}
				out.Commands = append(out.Commands, c.build(child, depth+1, visited))
			}
		}
		sortCommands(&out)
	}
	return out
}

// rootName returns the command name of an instance without full resolution.
func (c *conv) rootName(i *instance) string {
	if s, ok := c.usedNames[i.id]; ok && s != "" {
		return strings.Fields(s)[0]
	}
	lit := c.resolve(i)
	if lit != nil {
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			id, ok := kv.Key.(*ast.Ident)
			if !ok || id.Name != "Use" {
				continue
			}
			if s, ok := constString(kv.Value, i.pkg.TypesInfo); ok {
				return strings.Fields(s)[0]
			}
		}
	}
	return ""
}

// staticUse returns the statically known Use field; ok=false means the
// command name is resolved at runtime (dynamic command, e.g. built from
// loop variables).
func (c *conv) staticUse(i *instance) (string, bool) {
	lit := c.resolve(i)
	if lit == nil {
		return "", false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); !ok || id.Name != "Use" {
			continue
		}
		s, ok := constString(kv.Value, i.pkg.TypesInfo)
		return s, ok && s != ""
	}
	return "", false
}

// groupIndex resolves `slice[idx]` / `slice[idx].ID` expressions against the
// known cobra.Group slices, returning the group ID at the constant index.
func (c *conv) groupIndex(e ast.Expr, pkg *packages.Package, info *types.Info) string {
	sel, isSel := e.(*ast.SelectorExpr)
	var idx *ast.IndexExpr
	if isSel {
		if sel.Sel.Name != "ID" {
			return ""
		}
		idx, _ = sel.X.(*ast.IndexExpr)
	} else {
		idx, _ = e.(*ast.IndexExpr)
	}
	if idx == nil {
		return ""
	}
	ident, ok := idx.X.(*ast.Ident)
	if !ok {
		return ""
	}
	ids, ok := c.groupSlices[pkg.PkgPath+"."+ident.Name]
	if !ok {
		return ""
	}
	n, ok := constInt(idx.Index, info)
	if !ok || n < 0 || n >= len(ids) {
		return ""
	}
	return ids[n]
}

func sortCommands(out *command.Command) {
	children := out.Commands
	if len(children) > 0 {
		sort.Slice(children, func(i, j int) bool {
			// _carapace first, then alphabetical
			a, b := children[i].Name, children[j].Name
			if a == "carapace" || b == "carapace" {
				return a == "carapace"
			}
			return a < b
		})
	}
}
