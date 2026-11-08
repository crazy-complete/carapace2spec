package convert

import (
	"go/ast"
	"go/constant"
	"go/types"
)

// evalCtx is the context of one helper-body evaluation frame.
type evalCtx struct {
	bodyInfo *types.Info           // type info of the package holding the frame body
	cmdParam string                // parameter name of the *cobra.Command
	file     *ast.File             // source file of the frame body
	env      map[string]bindingEnv // parameter -> caller expression
}

// evalFrame evaluates the body of a registered flag helper from one of its
// call sites and returns the flag registrations and attributes performed.
func (c *conv) evalFrame(d *helperDecl, idx int, env map[string]bindingEnv) ([]flagReg, []attrRef) {
	if d.decl.Body == nil || d.pkg.TypesInfo == nil {
		return nil, nil
	}
	ctx := &evalCtx{
		bodyInfo: d.pkg.TypesInfo,
		cmdParam: helperCmdName(d.decl, idx),
		file:     d.file,
		env:      env,
	}
	return c.evalBlock(d.decl.Body, ctx)
}

// evalBlock evaluates the statements of a block, collecting flag
// registrations and attribute assignments. Calls to nested registered
// helpers are expanded.
func (c *conv) evalBlock(block *ast.BlockStmt, ctx *evalCtx) ([]flagReg, []attrRef) {
	var regs []flagReg
	var attrs []attrRef
	for _, stmt := range block.List {
		switch s := stmt.(type) {
		case *ast.ExprStmt:
			call, ok := s.X.(*ast.CallExpr)
			if !ok || callInFuncLit(ctx.file, call) {
				continue
			}
			f := funcOf(call, ctx.bodyInfo)
			if f == nil {
				continue
			}
			if isPflagReg(call, ctx) {
				// Only known pflag registration methods count; other
				// pflag calls (Lookup, Set, ...) are bookkeeping.
				if _, _, _, _, warn := parseFlagMethod(f.Name(), len(call.Args), pkgPathOf(f)); warn == "" {
					regs = append(regs, flagReg{
						method:     f.Name(),
						args:       call.Args,
						persistent: isPflagPersistent(call),
						node:       call,
						idpkg:      pkgPathOf(f),
					})
				}
				continue
			}
			qn := pkgPathOf(f) + "." + f.Name()
			if sub := c.helperDecls[qn]; sub != nil && helperCmdIdent(call, c.helperIdx[qn], ctx.cmdParam) {
				sreg, sattrs := c.evalBlock(sub.decl.Body, c.frameCtx(sub, c.helperIdx[qn], call, ctx))
				regs = append(regs, sreg...)
				attrs = append(attrs, sattrs...)
			}
		case *ast.IfStmt:
			cv, known := c.constBool(ctx, s.Cond)
			var br []*ast.BlockStmt
			if known {
				if cv {
					br = append(br, s.Body)
				} else if e, ok := s.Else.(*ast.BlockStmt); ok {
					br = append(br, e)
				}
			} else {
				// Non-constant condition: conservatively take both branches
				// (registrations inside are attributed to all call sites).
				br = append(br, s.Body)
				if e, ok := s.Else.(*ast.BlockStmt); ok {
					br = append(br, e)
				}
			}
			for _, bl := range br {
				breg, battrs := c.evalBlock(bl, ctx)
				regs = append(regs, breg...)
				attrs = append(attrs, battrs...)
			}
		case *ast.AssignStmt:
			if a, ok := attrInAssign(s, ctx); ok {
				attrs = append(attrs, a)
			}
		}
	}
	return regs, attrs
}

// frameCtx builds the evaluation context for a nested helper call: the
// command parameter is identified by its position, and the remaining
// arguments are bound either to a value already in the parent frame's
// environment or to the expression as written in the parent body.
func (c *conv) frameCtx(sub *helperDecl, idx int, call *ast.CallExpr, ctx *evalCtx) *evalCtx {
	info := sub.pkg.TypesInfo
	names := paramNames(sub.decl)
	cmd := ""
	if idx >= 0 && idx < len(names) {
		cmd = names[idx]
	}
	env := map[string]bindingEnv{}
	for i := idx + 1; i < len(names) && i < len(call.Args); i++ {
		arg := call.Args[i]
		if id, ok := arg.(*ast.Ident); ok {
			if en, has := ctx.env[id.Name]; has {
				env[names[i]] = en
				continue
			}
		}
		env[names[i]] = bindingEnv{expr: arg, info: ctx.bodyInfo}
	}
	return &evalCtx{bodyInfo: info, cmdParam: cmd, file: sub.file, env: env}
}

// constBool evaluates a constant boolean condition, resolving identifiers
// through the frame's parameter environment.
func (c *conv) constBool(ctx *evalCtx, e ast.Expr) (bool, bool) {
	v, ok := constValue(e, ctx.bodyInfo, func(name string) (ast.Expr, *types.Info, bool) {
		en, has := ctx.env[name]
		if !has {
			return nil, nil, false
		}
		return en.expr, en.info, true
	})
	if !ok {
		return false, false
	}
	b := constant.BoolVal(v)
	return b, true
}

// isPflagPersistent reports whether a pflag registration call targets
// PersistentFlags().
func isPflagPersistent(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	isel, ok := inner.Fun.(*ast.SelectorExpr)
	return ok && isel.Sel.Name == "PersistentFlags"
}

// isPflagReg reports whether call is a pflag registration of the shape
// cmdParam.Flags().M(...) / cmdParam.PersistentFlags().M(...).
func isPflagReg(call *ast.CallExpr, ctx *evalCtx) bool {
	if ctx.cmdParam == "" {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	isel, ok := inner.Fun.(*ast.SelectorExpr)
	if !ok || (isel.Sel.Name != "Flags" && isel.Sel.Name != "PersistentFlags") {
		return false
	}
	return isIdentExpr(isel.X, ctx.cmdParam)
}

// helperCmdIdent reports whether the command argument at idx of a helper
// call is the current frame's command parameter (same command object).
func helperCmdIdent(call *ast.CallExpr, idx int, cmdParam string) bool {
	if cmdParam == "" || len(call.Args) <= idx {
		return false
	}
	return isIdentExpr(call.Args[idx], cmdParam)
}

func isIdentExpr(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// frameReceiver reports whether recv designates the frame's command,
// tolerating a trailing .Flags()/.PersistentFlags() accessor.
func frameReceiver(recv ast.Expr, ctx *evalCtx) bool {
	switch x := recv.(type) {
	case *ast.Ident:
		return x.Name == ctx.cmdParam
	case *ast.SelectorExpr:
		if x.Sel.Name == "Flags" || x.Sel.Name == "PersistentFlags" {
			if id, ok := x.X.(*ast.Ident); ok && id.Name == ctx.cmdParam {
				return true
			}
		}
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if ok && (sel.Sel.Name == "Flags" || sel.Sel.Name == "PersistentFlags") {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == ctx.cmdParam {
				return true
			}
		}
	}
	return false
}

// attrInAssign extracts hidden/optarg attribute assignments on the frame's
// command parameter:
//
//	cmd.Flag("name").Hidden = true
//	cmd.Flag("name").NoOptDefVal = " "
//	cmd.Flags().Lookup("name").NoOptDefVal = " "
func attrInAssign(s *ast.AssignStmt, ctx *evalCtx) (attrRef, bool) {
	if ctx.cmdParam == "" || len(s.Lhs) != 1 || len(s.Rhs) != 1 {
		return attrRef{}, false
	}
	sel, ok := s.Lhs[0].(*ast.SelectorExpr)
	if !ok {
		return attrRef{}, false
	}
	var recv, nameArg ast.Expr
	switch x := sel.X.(type) {
	case *ast.CallExpr: // cmd.Flag("name") / cmd.Lookup("name") / cmd.Flags().Lookup("name")
		csel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok || (csel.Sel.Name != "Flag" && csel.Sel.Name != "Lookup") || len(x.Args) == 0 {
			return attrRef{}, false
		}
		if !frameReceiver(csel.X, ctx) {
			return attrRef{}, false
		}
		recv, nameArg = csel.X, x.Args[0]
	case *ast.SelectorExpr: // cmd.Flag("name").Hidden = true is handled above;
		// this form: cmd.Flag(...) with a plain selector tail is not an
		// attribute pattern, reject.
		return attrRef{}, false
	default:
		return attrRef{}, false
	}
	_ = recv
	name, ok := constString(nameArg, ctx.bodyInfo)
	if !ok {
		return attrRef{}, false
	}
	var kind string
	switch sel.Sel.Name {
	case "NoOptDefVal":
		kind = "optarg"
	case "Hidden":
		if lit, ok := s.Rhs[0].(*ast.Ident); !ok || lit.Name != "true" {
			return attrRef{}, false
		}
		kind = "hidden"
	default:
		return attrRef{}, false
	}
	return attrRef{name: name, kind: kind}, true
}

// callInFuncLit reports whether the call is nested inside a function
// literal body of the given file.
func callInFuncLit(file *ast.File, n ast.Node) bool {
	return file != nil && inFuncLit(file, n)
}
