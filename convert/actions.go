package convert

import (
	"go/ast"
	"go/types"
	"strconv"

	"golang.org/x/tools/go/packages"
)

const stylePath = "github.com/carapace-sh/carapace/pkg/style"

type actionConverter struct {
	pkg  *packages.Package
	info *types.Info
	c    *conv
}

// convertAction translates a static carapace Action expression into the list
// of spec entries for one action (generic macro list form).
// Returns an empty, non-nil slice when the action was skipped (warning emitted).
func (a *actionConverter) convertAction(expr ast.Expr) []string {
	out := []string{}
	base, mods := a.splitChain(expr)
	if base == nil {
		a.c.warnAt(expr, "dynamic action expression cannot be converted statically")
		return out
	}

	// special case: ActionExecCommand(name, args...)(f)
	if th, ok := base.Fun.(*ast.CallExpr); ok {
		if f := funcOf(th, a.info); f != nil && pkgPathOf(f) == carapacePath && f.Name() == "ActionExecCommand" {
			return a.execCommand(base, th)
		}
		a.c.warnAt(th, "nested action constructor cannot be converted statically")
		return out
	}

	f := funcOf(base, a.info)
	if f == nil || f.Pkg() == nil || pkgPathOf(f) != carapacePath {
		// try one level of local helper indirection
		if hf := namedFunc(base.Fun, a.info); hf != nil {
			if pkg := hf.Pkg(); pkg != nil && pkg.Path() == a.pkg.PkgPath {
				body := a.helperReturn(hf)
				if body != nil {
					return a.convertAction(body)
				}
			}
		}
		a.c.warnAt(base, "action constructor "+funcName(base.Fun)+" cannot be converted statically")
		return out
	}

	entries, ok := a.baseEntry(f.Name(), base.Args)
	if !ok {
		return out
	}
	out = entries

	for _, m := range mods {
		if ms, okc := a.modifier(m.method, m.args); okc {
			out = append(out, ms)
		}
	}
	return out
}

// helperReturn follows a local function whose body returns a single value.
func (a *actionConverter) helperReturn(f *types.Func) ast.Expr {
	for _, pkg := range a.packages() {
		if pkg.PkgPath != pkgPathOf(f) {
			continue
		}
		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Name.Name != f.Name() || fd.Body == nil {
					continue
				}
				var out ast.Expr
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if rs, ok := n.(*ast.ReturnStmt); ok && len(rs.Results) == 1 {
						out = rs.Results[0]
					}
					return true
				})
				return out
			}
		}
	}
	return nil
}

func (a *actionConverter) packages() []*packages.Package {
	var out []*packages.Package
	_ = a
	return out
}

func funcName(n ast.Node) string {
	switch v := n.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return "unknown"
}

type chainMod struct {
	method string
	args   []ast.Expr
}

// splitChain separates a call chain F(...).M1(...).M2(...) into the base
// call and the ordered modifiers.
func (a *actionConverter) splitChain(expr ast.Expr) (base *ast.CallExpr, mods []chainMod) {
	stack := []chainMod{}
	for {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return nil, nil
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			base = call
			break
		}
		if inner, ok := sel.X.(*ast.CallExpr); ok {
			stack = append([]chainMod{{method: sel.Sel.Name, args: call.Args}}, stack...)
			expr = inner
			continue
		}
		base = call
		break
	}
	// reverse stack into call order (M1 before M2)
	mods = reverse(stack)
	return base, mods
}

func reverse(in []chainMod) []chainMod {
	out := make([]chainMod, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

// execCommand converts ActionExecCommand(name, args...)(f) to a $sh macro.
func (a *actionConverter) execCommand(outer, th *ast.CallExpr) []string {
	out := []string{}
	if len(th.Args) < 1 || len(outer.Args) != 1 {
		a.c.warnAt(outer, "ActionExecCommand: unsupported shape")
		return out
	}
	args := []string{}
	for i, arg := range th.Args {
		s, ok := constString(arg, a.info)
		if !ok {
			a.c.warnAt(outer, "ActionExecCommand: non-static argument")
			return out
		}
		if i == 0 && s == "" {
			a.c.warnAt(outer, "ActionExecCommand: empty command name")
			return out
		}
		args = append(args, s)
	}
	if !a.isActionImport(outer.Args[0]) {
		a.c.warnAt(outer, "ActionExecCommand: completion function other than ActionImport")
		return out
	}
	joined := args[0]
	if len(args) > 1 {
		for _, x := range args[1:] {
			joined += " " + quoteMacro(x)
		}
	}
	out = append(out, "$sh("+joined+")")
	return out
}

// isActionImport reports whether the thunk argument passes the command output
// to carapace.ActionImport.
func (a *actionConverter) isActionImport(n ast.Expr) bool {
	switch v := n.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		if f := namedFunc(n, a.info); f != nil && f.Name() == "ActionImport" && pkgPathOf(f) == carapacePath {
			return true
		}
	case *ast.CallExpr:
		if f := funcOf(v, a.info); f != nil && f.Name() == "ActionImport" && pkgPathOf(f) == carapacePath {
			return true
		}
	case *ast.FuncLit:
		if v.Body != nil {
			var out bool
			ast.Inspect(v.Body, func(x ast.Node) bool {
				rs, ok := x.(*ast.ReturnStmt)
				if !ok || len(rs.Results) != 1 {
					return true
				}
				if ce, ok := rs.Results[0].(*ast.CallExpr); ok {
					if f := funcOf(ce, a.info); f != nil && f.Name() == "ActionImport" && pkgPathOf(f) == carapacePath {
						out = true
					}
				}
				return true
			})
			if out {
				return true
			}
		}
	}
	return false
}

// baseEntry converts the base action constructor to spec entries.
func (a *actionConverter) baseEntry(name string, args []ast.Expr) ([]string, bool) {
	info := a.info
	out := []string{}

	stringArgs := func(n int) ([]string, bool) {
		vals := make([]string, 0, n)
		for i := 0; i < n && i < len(args); i++ {
			s, ok := constString(args[i], info)
			if !ok {
				return nil, false
			}
			vals = append(vals, s)
		}
		return vals, len(args) == n
	}

	switch name {
	case "ActionDirectories":
		if len(args) > 0 {
			a.c.warnAt(args[len(args)-1], "ActionDirectories: depth argument is not representable in spec")
		}
		out = append(out, "$directories")
		return out, true

	case "ActionFiles":
		vals, ok := stringArgs(len(args))
		if !ok && len(args) > 0 {
			a.c.warnAt(args[0], "ActionFiles: non-static pattern")
			return out, false
		}
		if len(vals) == 0 {
			out = append(out, "$files")
		} else if len(vals) == 1 {
			out = append(out, "$files("+quoteMacro(vals[0])+")")
		} else {
			out = append(out, "$files(["+strList(vals)+"])")
		}
		return out, true

	case "ActionExecutables":
		vals, ok := stringArgs(len(args))
		if !ok && len(args) > 0 {
			a.c.warnAt(args[0], "ActionExecutables: non-static directory")
			return out, false
		}
		if len(vals) == 0 {
			out = append(out, "$executables")
		} else if len(vals) == 1 {
			out = append(out, "$executables("+quoteMacro(vals[0])+")")
		} else {
			out = append(out, "$executables(["+strList(vals)+"])")
		}
		return out, true

	case "ActionValues", "ActionValuesDescribed", "ActionStyledValues",
		"ActionStyledValuesDescribed":
		width := 1
		if name == "ActionValuesDescribed" || name == "ActionStyledValuesDescribed" {
			width = 2
		}
		if name == "ActionStyledValues" {
			width = 2
		}
		if len(args)%width != 0 {
			a.c.warnAt(args[len(args)-1], name+": wrong number of arguments")
			return out, false
		}
		for i := 0; i < len(args); i += width {
			words := []string{}
			for j := 0; j < width; j++ {
				s, ok := constString(args[i+j], info)
				if !ok {
					a.c.warnAt(args[i+j], name+": non-static argument")
					return out, false
				}
				words = append(words, s)
			}
			out = append(out, joinWords(words))
		}
		return out, true

	case "ActionMessage":
		if len(args) == 0 {
			return out, false
		}
		s, ok := constString(args[0], info)
		if !ok {
			a.c.warnAt(args[0], "ActionMessage: non-static message")
			return out, false
		}
		if len(args) > 1 {
			a.c.warnAt(args[1], "ActionMessage: format arguments are not representable in spec")
		}
		out = append(out, `$message(`+quoteMacro(s)+`)`)
		return out, true

	case "ActionMultiParts", "ActionMultiPartsN":
		vals := []string{}
		for _, argR := range args {
			if _, ok := argR.(*ast.FuncLit); ok {
				continue // dynamic part callback — no spec representation
			}
			if _, ok := constInt(argR, info); ok {
				continue // part count (ActionMultiPartsN)
			}
			if r, ok := constRune(argR, info); ok {
				vals = append(vals, string(r))
				continue
			}
			if s, ok := constString(argR, info); ok {
				vals = append(vals, s)
				continue
			}
			a.c.warnAt(argR, name+": non-static delimiter")
			return out, false
		}
		out = append(out, "$multiparts(["+strList(vals)+"])")
		return out, true

	case "ActionCallback", "ActionAction", "ActionImport", "ActionCobra":
		a.c.warnAt(args[0], name+" is runtime behaviour and has no spec equivalent")
		return out, false

	case "ActionCommands", "ActionPositional":
		// subcommand completion is provided by the spec structure itself
		return out, true

	case "ActionExecute":
		a.c.warnAt(args[0], name+" cannot be converted; use a spec run section instead")
		return out, false

	case "ActionStyle", "ActionStyleConfig", "ActionStyler", "ActionStyles",
		"ActionEnvVars":
		a.c.warnAt(args[0], name+" cannot be converted statically")
		return out, false

	default:
		a.c.warnAt(args[argMax(args)], "unknown action "+name)
	}
	return out, false
}

func argMax(args []ast.Expr) int {
	if len(args) == 0 {
		return 0
	}
	return len(args) - 1
}

// strList renders a macro list of already-validated words.
func strList(vals []string) string {
	s := ""
	for i, v := range vals {
		if i > 0 {
			s += ", "
		}
		s += quoteMacro(v)
	}
	return s
}

// joinWords renders a tab-separated spec value (value, description, style).
func joinWords(words []string) string {
	s := words[0]
	for _, w := range words[1:] {
		s += "\t" + w
	}
	return s
}

// converterMap maps carapace action method names to their spec macro form.
func (a *actionConverter) modifier(method string, args []ast.Expr) (string, bool) {
	info := a.info
	one := func() (string, bool) {
		if len(args) == 0 {
			return "", false
		}
		s, ok := constString(args[0], info)
		if !ok {
			return "", false
		}
		return s, true
	}
	simple := func(name string) (string, bool) {
		s, ok := one()
		if !ok {
			a.c.warnAt(args[0], method+": non-static argument")
			return "", false
		}
		if len(args) > 1 {
			a.c.warnAt(args[len(args)-1], method+": extra arguments are not representable in spec")
		}
		return "$" + name + "(" + quoteMacro(s) + ")", true
	}
	listy := func(name string, runes bool) (string, bool) {
		vals := []string{}
		for _, arg := range args {
			if runes {
				if r, ok := constRune(arg, info); ok {
					vals = append(vals, string(r))
					continue
				}
			}
			s, ok := constString(arg, info)
			if !ok {
				a.c.warnAt(arg, method+": non-static argument")
				return "", false
			}
			vals = append(vals, s)
		}
		if len(vals) == 1 {
			return "$" + name + "(" + quoteMacro(vals[0]) + ")", true
		}
		return "$" + name + "(" + strList(vals) + ")", true
	}

	switch method {
	case "Chdir":
		return simple("chdir")
	case "ChdirF":
		return a.chdirF(args)
	case "Filter":
		return listy("filter", false)
	case "FilterArgs":
		if len(args) > 0 {
			a.c.warnAt(args[0], "FilterArgs: unexpected arguments")
		}
		return "$filterargs", true
	case "List":
		return simple("list")
	case "MultiParts":
		return listy("multiparts", true)
	case "NoPrefix":
		return simple("noprefix")
	case "NoSpace":
		return simple("nospace")
	case "Prefix":
		return simple("prefix")
	case "Retain":
		return listy("retain", false)
	case "Shift":
		if len(args) == 0 {
			return "", false
		}
		n, ok := constInt(args[0], info)
		if !ok {
			a.c.warnAt(args[0], "Shift: non-static argument")
			return "", false
		}
		return "$shift(" + strconv.Itoa(n) + ")", true
	case "Split":
		if len(args) > 0 {
			a.c.warnAt(args[0], "Split: shlex formats are not representable in spec")
		}
		return "$split", true
	case "SplitP":
		if len(args) > 0 {
			a.c.warnAt(args[0], "SplitP: shlex formats are not representable in spec")
		}
		return "$splitp", true
	case "Style":
		return simple("style")
	case "Suffix":
		return simple("suffix")
	case "Suppress":
		return simple("suppress")
	case "Tag":
		return simple("tag")
	case "Timeout":
		a.c.warnAt(args[argMax(args)], "Timeout is runtime behaviour; omitted from spec")
		return "", true
	case "Cache":
		a.c.warnAt(args[argMax(args)], "Cache is runtime behaviour; omitted from spec")
		return "", true
	case "UniqueList":
		return simple("uniquelist")
	case "Usage":
		if len(args) == 0 {
			return "", false
		}
		s, ok := constString(args[0], info)
		if !ok {
			a.c.warnAt(args[0], "Usage: non-static argument")
			return "", false
		}
		if len(args) > 1 {
			a.c.warnAt(args[1], "Usage: format arguments are not representable in spec")
		}
		return `$usage(` + quoteMacro(s) + `)`, true

	case "StyleF", "StyleR", "TagF", "Unless", "UnlessF", "Uid", "UidF",
		"Query", "QueryF", "UsageF", "SplitF", "UniqueListF", "MultiPartsP",
		"FilterParts", "Unique", "FilterFunc":
		a.c.warnAt(args[argMax(args)], method+" cannot be converted statically (dynamic/conditional behaviour)")
		return "", false
	}

	a.c.warnAt(args[argMax(args)], "modifier "+method+" is unknown")
	return "", false
}

// chdirF maps carapace traverse package arguments to spec macros.
func (a *actionConverter) chdirF(args []ast.Expr) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	arg := args[0]
	switch v := arg.(type) {
	case *ast.SelectorExpr:
		if o := a.info.Uses[v.Sel]; o != nil {
			if pkg := o.Pkg(); pkg != nil && pkg.Path() == carapacePath+"/pkg/traverse" {
				if dir, ok := traverseMap[o.Name()]; ok {
					return `$chdir(` + dir + `)`, true
				}
			}
			a.c.warnAt(arg, "ChdirF: unknown traverse function "+o.Name())
		}
	case *ast.CallExpr:
		if f := funcOf(v, a.info); f != nil && pkgPathOf(f) == carapacePath+"/pkg/traverse" {
			if f.Name() == "Parent" {
				vals := []string{}
				for _, arg2 := range v.Args {
					s, ok := constString(arg2, a.info)
					if !ok {
						a.c.warnAt(arg2, "ChdirF: Parent with non-static arguments")
						return "", false
					}
					vals = append(vals, s)
				}
				return `$chdir($parent([` + strList(vals) + `]))`, true
			}
		}
		a.c.warnAt(arg, "ChdirF: dynamic traverse call cannot be converted")
	}
	a.c.warnAt(arg, "ChdirF cannot be converted statically")
	return "", false
}

var traverseMap = map[string]string{
	"GitDir":        `$gitdir`,
	"GitWorkTree":   `$gitworktree`,
	"UserHomeDir":   `$userhomedir`,
	"UserCacheDir":  `$usercachedir`,
	"UserConfigDir": `$userconfigdir`,
	"XdgConfigHome": `$xdgconfighome`,
	"XdgCacheHome":  `$xdgcachehome`,
	"TempDir":       `$tempdir`,
	"NixProfile":    `$nixprofile`,
}
