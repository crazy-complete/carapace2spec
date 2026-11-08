package convert

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// discoverFile records command instances, edges and carapace registrations
// found in one source file.
func (c *conv) discoverFile(pkg *packages.Package, file *ast.File) {
	c.discoverVars(pkg, file)
	c.discoverGroupSlices(pkg, file)
	c.discoverBuilders(pkg, file)
	c.discoverUsage(pkg, file)
}

// discoverVars registers file-level and in-function variable assignments of
// cobra commands: var cmd = &cobra.Command{...} / cmd = newCmd() etc.
func (c *conv) discoverVars(pkg *packages.Package, file *ast.File) {
	info := pkg.TypesInfo

	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok || vs.Values == nil {
				continue
			}
			for k, name := range vs.Names {
				if k >= len(vs.Values) {
					continue
				}
				v := vs.Values[k]
				if isCommandExpr(v, info) || returnsCommandExpr(v, info) {
					c.instanceFor(pkg, name.Name, name, &v)
				}
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if !isCommandIdent(id, info) {
				continue
			}
			r := as.Rhs[0]
			if isCommandExpr(r, info) || returnsCommandExpr(r, info) {
				c.instanceFor(pkg, id.Name, n, &r)
			}
		}
		return true
	})
}

// pkgPathOf returns the import path of the package defining an object, or
// "" when the object has no package (nil/universe).
func pkgPathOf(o types.Object) string {
	if o == nil || o.Pkg() == nil {
		return ""
	}
	return o.Pkg().Path()
}

func isCommandIdent(id *ast.Ident, info *types.Info) bool {
	for _, m := range []map[*ast.Ident]types.Object{info.Uses, info.Defs} {
		if o := m[id]; o != nil {
			if t := o.Type(); t != nil {
				if isCommandType(t) {
					return true
				}
			}
		}
	}
	return false
}

// returnsCommandExpr reports whether expr is a call returning a command.
func returnsCommandExpr(expr ast.Expr, info *types.Info) bool {
	sig, ok := info.TypeOf(expr).(*types.Signature)
	if !ok {
		return false
	}
	for i := 0; i < sig.Results().Len(); i++ {
		if isCommandType(sig.Results().At(i).Type()) {
			return true
		}
	}
	return false
}

// discoverBuilders registers functions that build a command via a returned
// literal: func newSub() *cobra.Command { ... return &cobra.Command{...} }.
// isCobraGroupSliceType reports whether a type expression denotes []*cobra.Group.
func isCobraGroupSliceType(t ast.Expr) bool {
	at, ok := t.(*ast.ArrayType)
	if !ok {
		return false
	}
	star, ok := at.Elt.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Group" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "cobra" {
		return false
	}
	return true
}

// discoverGroupSlices records package-level `[]*cobra.Group` slices so that
// `GroupID: groups[i].ID` can be resolved statically.
func (c *conv) discoverGroupSlices(pkg *packages.Package, file *ast.File) {
	info := pkg.TypesInfo
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR || gd.Specs == nil {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok || vs.Values == nil || len(vs.Names) == 0 {
				continue
			}
			for k, name := range vs.Names {
				if k >= len(vs.Values) {
					continue
				}
				// The type is either written on the spec (var x []*cobra.Group = ...)
				// or on the composite literal (var x = []*cobra.Group{...}).
				var cl *ast.CompositeLit
				if vs.Type != nil {
					if isCobraGroupSliceType(vs.Type) {
						cl, _ = vs.Values[k].(*ast.CompositeLit)
					}
				} else if c, ok := vs.Values[k].(*ast.CompositeLit); ok {
					if isCobraGroupSliceType(c.Type) {
						cl = c
					}
				}
				if cl == nil {
					continue
				}
				var ids []string
				for _, el := range cl.Elts {
					ce, ok := el.(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, e2 := range ce.Elts {
						kv, ok := e2.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if kd, ok := kv.Key.(*ast.Ident); ok && kd.Name == "ID" {
							if s, ok := constString(kv.Value, info); ok {
								ids = append(ids, s)
							}
							break
						}
					}
				}
				if len(ids) > 0 {
					c.groupSlices[pkg.PkgPath+"."+name.Name] = ids
				}
			}
		}
	}
}

func (c *conv) discoverBuilders(pkg *packages.Package, file *ast.File) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if !returnsCommand(fd, pkg.TypesInfo) {
			continue
		}
		if lit := commandLiteral(fd, pkg.TypesInfo); lit != nil {
			i := c.instanceFor(pkg, fd.Name.Name, fd, nil)
			i.lit = lit
			c.byFunc[fd] = i
		} else if ret := commandReturnCall(fd, pkg.TypesInfo); ret != nil {
			if f := funcOf(ret, pkg.TypesInfo); f != nil {
				i := c.instanceFor(pkg, fd.Name.Name, fd, nil)
				i.borrow = c.builderFor(pkg, f.Name())
				c.byFunc[fd] = i
			}
		}
	}
}

// builderFor finds the builder instance of the given top-level function name.
func (c *conv) builderFor(pkg *packages.Package, name string) *ast.FuncDecl {
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == name {
				return fd
			}
		}
	}
	return nil
}

// commandReturnCall returns the returned call of a command builder.
func commandReturnCall(fd *ast.FuncDecl, info *types.Info) *ast.CallExpr {
	var out *ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		rs, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, r := range rs.Results {
			if ce, ok := r.(*ast.CallExpr); ok {
				out = ce
			}
		}
		return true
	})
	return out
}

// instanceFor creates or updates the instance bound to a variable.
func (c *conv) instanceFor(pkg *packages.Package, varname string, node ast.Node, expr *ast.Expr) *instance {
	key := pkg.PkgPath + "\t" + varname
	if i := c.byVar[key]; i != nil {
		if expr != nil && i.expr == nil {
			i.expr = expr
		}
		return i
	}
	c.counter++
	id := fmt.Sprintf("%s#%d", varname, c.counter)
	i := &instance{id: id, pkg: pkg, varname: varname, node: node, expr: expr}
	c.insts[id] = i
	c.byVar[key] = i
	return i
}

// discoverUsage records carapace Gen targets, AddCommand edges, flag
// registrations, cobra marks and completion registrations.
func (c *conv) discoverUsage(pkg *packages.Package, file *ast.File) {
	info := pkg.TypesInfo

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Registrations inside function literals (Run closures,
		// ActionCallback bodies) execute at runtime, not at spec
		// construction time: they have no static representation.
		if inFuncLit(file, call) {
			return true
		}
		if f := funcOf(call, info); f != nil {
			c.onCall(pkg, file, call, f, info)
			return true
		}
		// Flag attribute assignments: cmd.Flag("name").NoOptDefVal = " ",
		// cmd.Flag("name").Hidden = true
		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if inFuncLit(file, as) {
			return true
		}
		sel, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ce, ok := sel.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel2, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok || sel2.Sel.Name != "Flag" || len(ce.Args) == 0 {
			return true
		}
		name, ok := constString(ce.Args[0], info)
		if !ok {
			return true
		}
		var kind string
		switch sel.Sel.Name {
		case "NoOptDefVal":
			kind = "optarg"
		case "Hidden":
			if lit, ok := as.Rhs[0].(*ast.Ident); !ok || lit.Name != "true" {
				return true
			}
			kind = "hidden"
		default:
			return true
		}

		if i2 := c.instanceOf(pkg, sel2.X, info); i2 != nil {
			switch kind {
			case "optarg":
				if c.optargs[i2.id] == nil {
					c.optargs[i2.id] = map[string]bool{}
				}
				c.optargs[i2.id][name] = true
			case "hidden":
				c.marks[i2.id] = append(c.marks[i2.id], mark{kind: "hidden", name: name, node: as})
			}
			return true
		}

		// Receiver is a helper-function parameter: the helper evaluator
		// (evalFrame) attributes it from the helper's call sites.
		return true
	})
}

// onCall dispatches a resolved call to the matching model hook.
func (c *conv) onCall(pkg *packages.Package, file *ast.File, call *ast.CallExpr, f *types.Func, info *types.Info) {
	path := pkgPathOf(f)
	switch {
	case path == carapacePath && f.Name() == "Gen":
		if len(call.Args) == 0 {
			return
		}
		if t := c.instanceOf(pkg, call.Args[0], info); t != nil {
			c.genRoots[t.id] = true
		}

	case path == carapacePath:
		// method on Gen(cmd) or Carapace{cmd}
		t, ok := c.carapaceReceiver(pkg, call, info)
		if !ok {
			return
		}
		switch f.Name() {
		case "Standalone":
			c.standalone[t.id] = true
		case "FlagCompletion", "PositionalCompletion", "PositionalAnyCompletion",
			"DashCompletion", "DashAnyCompletion":
			kind := map[string]string{
				"FlagCompletion":          "flag",
				"PositionalCompletion":    "position",
				"PositionalAnyCompletion": "posany",
				"DashCompletion":          "dash",
				"DashAnyCompletion":       "dashany",
			}[f.Name()]
			c.completions[t.id] = append(c.completions[t.id], completion{kind: kind, args: call.Args, node: call})

		case "Args":
			// carapace Gen ... Args() — auto args handling; nothing to emit
			return

		default:
			// PreRun, PostRun etc. are runtime only
			return
		}

	case path == cobraPath:
		receiver := call.Fun
		if sel, ok := receiver.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "AddCommand":
				for _, a := range call.Args {
					if parent := c.instanceOf(pkg, sel.X, info); parent != nil {
						if ch := c.instanceOf(pkg, a, info); ch != nil {
							c.children[parent.id] = append(c.children[parent.id], ch.id)
							continue
						}
					}
					// Receiver or child var was declared later in file order;
					// re-resolve once all vars are discovered.
					c.pendingEdges = append(c.pendingEdges, pendingEdge{pkg: pkg, info: info, receiver: sel.X, child: a})
				}
				return
			case "MarkFlagRequired", "MarkFlagHidden":
				kind := "required"
				if sel.Sel.Name == "MarkFlagHidden" {
					kind = "hidden"
				}
				if t := c.instanceOf(pkg, receiverBase(sel, call), info); t != nil {
					c.marks[t.id] = append(c.marks[t.id], mark{kind: kind, args: call.Args, node: call})
				}
				return
			case "MarkFlagsMutuallyExclusive":
				if t := c.cobraReceiver(pkg, call, info); t != nil {
					c.marks[t.id] = append(c.marks[t.id], mark{kind: "mutually_exclusive", args: call.Args, node: call})
				}
				return
			case "MarkFlagFilename", "MarkFlagDirname":
				if t := c.cobraReceiver(pkg, call, info); t != nil {
					kind := "filename"
					if sel.Sel.Name == "MarkFlagDirname" {
						kind = "directory"
					}
					c.marks[t.id] = append(c.marks[t.id], mark{kind: kind, args: call.Args, node: call})
				}
				return
			}
		}

	case path == pflagPath:
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			// receiver of the Flags() / PersistentFlags() call
			inner, ok := sel.X.(*ast.CallExpr)
			if !ok {
				return
			}
			isel, ok := inner.Fun.(*ast.SelectorExpr)
			if !ok {
				return
			}
			persistent := isel.Sel.Name == "PersistentFlags"
			if isel.Sel.Name != "Flags" && !persistent {
				return
			}
			if t := c.instanceOf(pkg, isel.X, info); t != nil {
				reg := flagReg{method: f.Name(), args: call.Args, persistent: persistent, node: call, idpkg: pkgPathOf(f)}
				if persistent {
					c.pflags[t.id] = append(c.pflags[t.id], reg)
				} else {
					c.flags[t.id] = append(c.flags[t.id], reg)
				}
				return
			}
			// Receiver is not a resolved command (e.g. a helper-function
			// parameter): the helper evaluator (evalFrame) attributes it.
			return
		}
	}
}

// receiverBase extracts the receiver expression of a cobra method whose
// receiver is a chained call, e.g. cmd.AddCommand's cmd.
func receiverBase(sel *ast.SelectorExpr, call *ast.CallExpr) ast.Expr {
	return sel.X
}

// carapaceReceiver resolves the command carried by a carapace method call of
// the form carapace.Gen(cmd).M(...) or carapace.Carapace{cmd}.M(...).
func (c *conv) carapaceReceiver(pkg *packages.Package, call *ast.CallExpr, info *types.Info) (*instance, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	switch x := sel.X.(type) {
	case *ast.CallExpr:
		if f := funcOf(x, info); f != nil && f.Name() == "Gen" && pkgPathOf(f) == carapacePath && len(x.Args) == 1 {
			if t := c.instanceOf(pkg, x.Args[0], info); t != nil {
				return t, true
			}
		}
	case *ast.CompositeLit:
		if len(x.Elts) == 1 && isCommandType(info.TypeOf(x)) == false &&
			carapaceStruct(info.TypeOf(x)) {
			if t := c.instanceOf(pkg, x.Elts[0], info); t != nil {
				return t, true
			}
		}
	}
	return nil, false
}

func carapaceStruct(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == carapacePath && obj.Name() == "Carapace"
}

// cobraReceiver resolves the command receiver of a cobra method.
func (c *conv) cobraReceiver(pkg *packages.Package, call *ast.CallExpr, info *types.Info) *instance {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	return c.instanceOf(pkg, sel.X, info)
}

// pendingEdge is an AddCommand edge whose receiver or child could not be
// resolved at call time (var declared in a later-processed file).
type pendingEdge struct {
	pkg      *packages.Package
	info     *types.Info
	receiver ast.Expr
	child    ast.Expr
}

// resolvePendingEdges re-resolves deferred AddCommand edges now that all
// package vars are known.
func (c *conv) resolvePendingEdges() {
	for _, e := range c.pendingEdges {
		if parent := c.instanceOf(e.pkg, e.receiver, e.info); parent != nil {
			if ch := c.instanceOf(e.pkg, e.child, e.info); ch != nil {
				c.children[parent.id] = append(c.children[parent.id], ch.id)
			}
		}
	}
	c.pendingEdges = nil
}

// helperBindRef is a helper call site whose target command was not
// resolvable at discovery time.
type helperBindRef struct {
	qn   string
	pkg  *packages.Package
	info *types.Info
	arg  ast.Expr
	idx  int
	bi   int // index of the (targetless) binding in helperBindings[qn]
}

// helperDecl is a registered flag-helper declaration.
type helperDecl struct {
	decl *ast.FuncDecl
	pkg  *packages.Package
	file *ast.File
}

// bindingEnv binds a helper parameter to the caller expression passed for
// it, recorded with the type info of the package where it was written.
type bindingEnv struct {
	expr ast.Expr
	info *types.Info
}

// helperBinding is a helper call site together with the command it was
// called for and the argument bindings of the remaining parameters.
type helperBinding struct {
	targets []*instance
	env     map[string]bindingEnv
}

// attrRef is a flag attribute (optarg/hidden) set on a flag of a command.
type attrRef struct {
	name string
	kind string
}

// scanHelpers records functions that take a *cobra.Command parameter —
// flag-registration helpers.
func (c *conv) scanHelpers(pkg *packages.Package) {
	info := pkg.TypesInfo
	if info == nil {
		return
	}
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Type == nil || fd.Name == nil {
				continue
			}
			idx := commandParamIndex(info, fd.Type.Params)
			if idx < 0 {
				continue
			}
			if fn, ok := info.Defs[fd.Name].(*types.Func); ok {
				if path := pkgPathOf(fn); path != "" {
					qn := path + "." + fn.Name()
					c.helperIdx[qn] = idx
					c.helperDecls[qn] = &helperDecl{decl: fd, pkg: pkg, file: file}
				}
			}
		}
	}
}

// commandParamIndex finds the index of the first *cobra.Command parameter.
func commandParamIndex(info *types.Info, fields *ast.FieldList) int {
	if fields == nil {
		return -1
	}
	idx := 0
	for _, field := range fields.List {
		for _, name := range field.Names {
			if obj := info.Defs[name]; obj != nil && isCommandPointer(obj.Type()) {
				return idx
			}
			idx++
		}
	}
	return -1
}

func isCommandPointer(t types.Type) bool {
	p, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	n, ok := p.Elem().(*types.Named)
	if !ok {
		return false
	}
	obj := n.Obj()
	return pkgPathOf(obj) == cobraPath && obj.Name() == "Command"
}

// paramNames returns the ordered parameter names of a function declaration.
func paramNames(fn *ast.FuncDecl) []string {
	var out []string
	if fn.Type == nil || fn.Type.Params == nil {
		return out
	}
	for _, field := range fn.Type.Params.List {
		for _, name := range field.Names {
			out = append(out, name.Name)
		}
	}
	return out
}

// helperCmdName returns the name of the *cobra.Command parameter at idx.
func helperCmdName(fn *ast.FuncDecl, idx int) string {
	names := paramNames(fn)
	if idx < 0 || idx >= len(names) {
		return ""
	}
	return names[idx]
}

// collectHelperBinds records helper call sites and the target command passed
// to them, together with bindings for the remaining parameters.
func (c *conv) collectHelperBinds(pkg *packages.Package) {
	info := pkg.TypesInfo
	if info == nil {
		return
	}
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			f := funcOf(call, info)
			if f == nil {
				return true
			}
			path := pkgPathOf(f)
			if path == "" {
				return true
			}
			qn := path + "." + f.Name()
			idx, ok := c.helperIdx[qn]
			if !ok || len(call.Args) <= idx || inFuncLit(file, call) {
				return true
			}
			args := call.Args[idx:]
			b := &helperBinding{env: map[string]bindingEnv{}}
			if names := c.helperParamNames(qn); len(names) > idx {
				for i := idx + 1; i < len(names) && i < len(args); i++ {
					b.env[names[i]] = bindingEnv{expr: args[i], info: info}
				}
			}
			if t := c.instanceOf(pkg, args[0], info); t != nil {
				b.targets = append(b.targets, t)
			} else {
				c.pendingBinds = append(c.pendingBinds, helperBindRef{
					qn:   qn,
					pkg:  pkg,
					info: info,
					arg:  args[0],
					idx:  idx,
					bi:   len(c.helperBindings[qn]),
				})
			}
			c.helperBindings[qn] = append(c.helperBindings[qn], b)
			return true
		})
	}
}

// makeRegFromCall builds a flagReg from a pflag registration call.
func (c *conv) makeRegFromCall(call *ast.CallExpr, method *types.Func, persistent bool) flagReg {
	return flagReg{method: method.Name(), args: call.Args, persistent: persistent, node: call, idpkg: pkgPathOf(method)}
}

// helperParamNames returns the ordered parameter names of a registered helper.
func (c *conv) helperParamNames(qn string) []string {
	if d := c.helperDecls[qn]; d != nil {
		return paramNames(d.decl)
	}
	return nil
}

func (c *conv) resolvePendingBinds() {
	for _, b := range c.pendingBinds {
		if t := c.instanceOf(b.pkg, b.arg, b.info); t != nil {
			if bs := c.helperBindings[b.qn]; len(bs) > b.bi {
				bs[b.bi].targets = append(bs[b.bi].targets, t)
			}
		}
	}
	c.pendingBinds = nil
}

// resolveHelperFlags evaluates the registered flag helpers from their call
// sites and attributes the collected registrations and attributes to the
// bound commands.
func (c *conv) resolveHelperFlags() {
	for qn, bindings := range c.helperBindings {
		d := c.helperDecls[qn]
		if d == nil {
			continue
		}
		idx := c.helperIdx[qn]
		for _, b := range bindings {
			if len(b.targets) == 0 {
				continue
			}
			reg, attrs := c.evalFrame(d, idx, b.env)
			for _, t := range b.targets {
				for _, r := range reg {
					if r.persistent {
						c.pflags[t.id] = append(c.pflags[t.id], r)
					} else {
						c.flags[t.id] = append(c.flags[t.id], r)
					}
				}
				for _, a := range attrs {
					switch a.kind {
					case "optarg":
						if c.optargs[t.id] == nil {
							c.optargs[t.id] = map[string]bool{}
						}
						c.optargs[t.id][a.name] = true
					case "hidden":
						c.marks[t.id] = append(c.marks[t.id], mark{kind: "hidden", name: a.name})
					}
				}
			}
		}
	}
	c.helperBindings = nil
}

// instanceOf maps an expression to the command instance it refers to.
func (c *conv) instanceOf(pkg *packages.Package, expr ast.Expr, info *types.Info) *instance {
	e := unwrapPtr(expr)
	switch v := e.(type) {
	case *ast.Ident:
		if i := c.byVar[pkg.PkgPath+"	"+v.Name]; i != nil {
			return i
		}
		// function parameter of a builder: the command being built
		if fn := nearestFunc(pkg, expr); fn != nil {
			if i := c.byFunc[fn]; i != nil {
				return i
			}
		}
		return nil
	case *ast.CallExpr:
		if f := funcOf(v, info); f != nil {
			// direct builder call used as command value
			bf := c.builderFor(pkg, f.Name())
			if bf != nil {
				if i := c.byFunc[bf]; i != nil {
					return i
				}
			}
		}
		if isCommandExpr(v, info) {
			var ex ast.Expr = v
			return c.instanceFor(pkg, "", v, &ex)
		}
		return nil
	case *ast.CompositeLit:
		if isCommandType(info.TypeOf(v)) {
			var ex ast.Expr = v
			return c.instanceFor(pkg, "", v, &ex)
		}
		return nil
	default:
		return nil
	}
}

func nearestFunc(pkg *packages.Package, n ast.Node) *ast.FuncDecl {
	var out *ast.FuncDecl
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(x ast.Node) bool {
			if fd, ok := x.(*ast.FuncDecl); ok && (n.Pos() >= fd.Pos() && n.End() <= fd.End()) {
				if out == nil || fd.Pos() > out.Pos() {
					out = fd
				}
			}
			return true
		})
	}
	return out
}

// containsSubtree reports whether n occurs in the subtree rooted at root.
func containsSubtree(root, n ast.Node) bool {
	done := false
	ast.Inspect(root, func(x ast.Node) bool {
		if done {
			return false
		}
		if x == n {
			done = true
			return false
		}
		return true
	})
	return done
}

// inFuncLit reports whether n is nested inside a function literal body —
// i.e. it executes at runtime (Run closures, ActionCallback bodies) rather
// than during command construction.
func inFuncLit(file *ast.File, n ast.Node) bool {
	found := false
	ast.Inspect(file, func(x ast.Node) bool {
		if found {
			return false
		}
		if lit, ok := x.(*ast.FuncLit); ok && lit.Body != nil {
			if containsSubtree(lit.Body, n) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
