package convert

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/carapace-sh/carapace/pkg/command"
	"golang.org/x/tools/go/packages"
)

const (
	carapacePath = "github.com/carapace-sh/carapace"
	cobraPath    = "github.com/spf13/cobra"
	pflagPath    = "github.com/spf13/pflag"
)

// Result holds the converted spec and everything that could not be converted.
type Result struct {
	// Spec is the carapace spec in the official data model (command.Command),
	// ready for spec.ToYAML / spec.ToJSON.
	Spec command.Command
	// Warnings lists the constructs that were omitted from the spec.
	Warnings []*Warning
}

// Convert converts the carapace completer rooted at dir (a directory with a
// main package and/or cmd package in any module) into carapace spec format.
func Convert(dir string) (*Result, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  dir,
		Fset: fset,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", dir, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go packages found in %s", dir)
	}

	r := &Result{}
	for _, p := range pkgs {
		for _, e := range p.Errors {
			// version-mismatch load noise is harmless for static analysis
			if strings.Contains(e.Msg, "requires newer Go version") {
				continue
			}
			r.Warnings = append(r.Warnings, &Warning{Message: e.Msg})
		}
	}

	c := &conv{
		fset:           fset,
		result:         r,
		insts:          map[string]*instance{},
		byVar:          map[string]*instance{},
		byFunc:         map[*ast.FuncDecl]*instance{},
		children:       map[string][]string{},
		genRoots:       map[string]bool{},
		flags:          map[string][]flagReg{},
		pflags:         map[string][]flagReg{},
		marks:          map[string][]mark{},
		optargs:        map[string]map[string]bool{},
		completions:    map[string][]completion{},
		standalone:     map[string]bool{},
		usedNames:      map[string]string{},
		helperIdx:      map[string]int{},
		helperDecls:    map[string]*helperDecl{},
		helperBindings: map[string][]*helperBinding{},
		groupSlices:    map[string][]string{},
		parsing:        map[string]bool{},
	}
	for _, p := range pkgs {
		for _, file := range p.Syntax {
			c.discoverFile(p, file)
		}
	}
	for _, p := range pkgs {
		c.scanHelpers(p)
	}
	if d := os.Getenv("CRA_DEBUG_GROUPS"); d != "" {
		for k, v := range c.groupSlices {
			fmt.Fprintf(os.Stderr, "GROUP %s = %v\n", k, v)
		}
	}
	for _, p := range pkgs {
		c.collectHelperBinds(p)
	}
	c.resolvePendingEdges()
	c.resolvePendingBinds()
	c.resolveHelperFlags()

	root, err := c.selectRoot()
	if err != nil {
		return nil, err
	}
	r.Spec = c.build(root, 0, map[string]bool{})
	return r, nil
}

// conv carries the intermediate program model while scanning.
type conv struct {
	fset   *token.FileSet
	result *Result

	insts   map[string]*instance
	byVar   map[string]*instance // "pkgpath\tvarname" -> instance
	byFunc  map[*ast.FuncDecl]*instance
	counter int

	children map[string][]string // parent -> ordered children
	genRoots map[string]bool

	pendingEdges []pendingEdge // AddCommand edges needing later re-resolution

	// Inter-procedural flag helpers: functions like common.AddDiffFlags(cmd
	// *cobra.Command) that register flags on a passed-in command.
	helperIdx      map[string]int              // qualified "pkgpath.Func" -> command param index
	helperDecls    map[string]*helperDecl      // qualified name -> helper declaration
	helperBindings map[string][]*helperBinding // qualified name -> call-site bindings
	pendingBinds   []helperBindRef             // binds whose target was not yet resolvable
	groupSlices    map[string][]string         // pkgPath + "." + sliceVar -> ordered group IDs

	flags       map[string][]flagReg
	pflags      map[string][]flagReg
	marks       map[string][]mark
	optargs     map[string]map[string]bool
	completions map[string][]completion
	standalone  map[string]bool
	usedNames   map[string]string // instance id -> first word of its Use (precomputed)
	parsing     map[string]bool   // instance id -> interspersed flag parsing
}

type instance struct {
	id      string
	pkg     *packages.Package
	varname string // "" for anonymous literals
	node    ast.Node
	expr    *ast.Expr
	borrow  *ast.FuncDecl // builder to follow when expr is not a literal
	lit     *ast.CompositeLit
}

type flagReg struct {
	method     string
	args       []ast.Expr
	persistent bool
	node       ast.Node
	idpkg      string
}

type mark struct {
	kind string
	args []ast.Expr
	name string // direct flag name, preferred over constNames(args)
	node ast.Node
}

type completion struct {
	kind string // flag|position|posany|dash|dashany
	args []ast.Expr
	node ast.Node
}

// funcOf resolves the *types.Func of a call, if statically known.
func funcOf(call *ast.CallExpr, info *types.Info) *types.Func {
	if f, ok := constOf(call.Fun, info).(*types.Func); ok {
		return f
	}
	return nil
}

// isCommandExpr reports whether expr denotes a *cobra.Command value or pointer.
func isCommandExpr(expr ast.Expr, info *types.Info) bool {
	t := info.TypeOf(expr)
	return t != nil && isCommandType(t)
}

func isCommandType(t types.Type) bool {
	if t == nil {
		return false
	}
	if pt, ok := t.(*types.Pointer); ok {
		t = pt.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == cobraPath && obj.Name() == "Command"
}

// returnsCommand reports whether fd is a function returning a cobra Command.
func returnsCommand(fd *ast.FuncDecl, info *types.Info) bool {
	if fd.Type.Results == nil {
		return false
	}
	for _, r := range fd.Type.Results.List {
		if isCommandType(info.TypeOf(r.Type)) {
			return true
		}
	}
	return false
}

// commandLiteral returns the returned *cobra.Command literal of fd, if any.
func commandLiteral(fd *ast.FuncDecl, info *types.Info) *ast.CompositeLit {
	if fd.Body == nil {
		return nil
	}
	var out *ast.CompositeLit
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CompositeLit:
			if isCommandExpr(e, info) {
				out = e
			}
		case *ast.ReturnStmt:
			for _, r := range e.Results {
				if cl, ok := unwrapPtr(r).(*ast.CompositeLit); ok && isCommandExpr(cl, info) {
					out = cl
				}
			}
		}
		return true
	})
	return out
}

func unwrapPtr(n ast.Node) ast.Node {
	if u, ok := n.(*ast.UnaryExpr); ok && u.Op == token.AND {
		return u.X
	}
	return n
}

// constString returns the literal string value of a constant expression.
func constString(expr ast.Expr, info *types.Info) (string, bool) {
	if e, ok := expr.(*ast.BasicLit); ok && e.Kind == token.STRING {
		if s, err := strconv.Unquote(e.Value); err == nil {
			return s, true
		}
		return "", false
	}
	if e, ok := expr.(*ast.Ident); ok {
		if c, ok := constOf(e, info).(*types.Const); ok {
			return constant.StringVal(c.Val()), true
		}
	}
	return "", false
}

// constInt returns the literal int value of a constant expression.
func constInt(expr ast.Expr, info *types.Info) (int, bool) {
	if e, ok := expr.(*ast.BasicLit); ok && e.Kind == token.INT {
		if v, err := strconv.ParseInt(e.Value, 10, 64); err == nil {
			return int(v), true
		}
		return 0, false
	}
	if c, ok := constOf(expr, info).(*types.Const); ok {
		if v, ok := constant.Int64Val(c.Val()); ok {
			return int(v), true
		}
	}
	return 0, false
}

// constValue returns the constant value of a (possibly compound) constant
// expression such as 5*time.Minute or a & b. The optional env resolver maps
// an identifier to a bound expression (a helper parameter), which is then
// resolved under the type info of the package where it was written.
func constValue(expr ast.Expr, info *types.Info, env func(string) (ast.Expr, *types.Info, bool)) (constant.Value, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.INT {
			return constant.MakeFromLiteral(e.Value, token.INT, 0), true
		}
		return nil, false
	case *ast.Ident:
		if env != nil {
			if ex, ei, ok := env(e.Name); ok {
				return constValue(ex, ei, nil)
			}
		}
		if c, ok := constOf(e, info).(*types.Const); ok {
			return c.Val(), true
		}
		return nil, false
	case *ast.SelectorExpr:
		if c, ok := constOf(expr, info).(*types.Const); ok {
			return c.Val(), true
		}
		return nil, false
	case *ast.ParenExpr:
		return constValue(e.X, info, env)
	case *ast.UnaryExpr:
		v, ok := constValue(e.X, info, env)
		if !ok {
			return nil, false
		}
		switch e.Op {
		case token.ADD, token.SUB, token.AND, token.XOR:
			return constant.UnaryOp(e.Op, v, 0), true
		}
		return nil, false
	case *ast.BinaryExpr:
		a, ok1 := constValue(e.X, info, env)
		b, ok2 := constValue(e.Y, info, env)
		if !ok1 || !ok2 {
			return nil, false
		}
		switch e.Op {
		case token.LAND, token.LOR,
			token.ADD, token.SUB, token.MUL, token.QUO, token.REM,
			token.AND, token.OR, token.XOR, token.AND_NOT,
			token.SHL, token.SHR:
			return safeBinaryOp(a, e.Op, b)
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
			return constant.MakeBool(constant.Compare(a, e.Op, b)), true
		}
		return nil, false
	}
	return nil, false
}

// safeBinaryOp wraps constant.BinaryOp, which panics on undefined
// operations (division by zero, ...).
func safeBinaryOp(a constant.Value, op token.Token, b constant.Value) (v constant.Value, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			v, ok = nil, false
		}
	}()
	return constant.BinaryOp(a, op, b), true
}

// constDuration returns the evaluated value of a constant time.Duration
// expression such as 5*time.Minute.
func constDuration(expr ast.Expr, info *types.Info) (time.Duration, bool) {
	v, ok := constValue(expr, info, nil)
	if !ok {
		return 0, false
	}
	n, ok := constant.Int64Val(v)
	if !ok {
		return 0, false
	}
	return time.Duration(n), true
}

// constRune returns the rune value of a constant expression.
func constRune(expr ast.Expr, info *types.Info) (rune, bool) {
	if e, ok := expr.(*ast.BasicLit); ok && e.Kind == token.CHAR {
		if s, err := strconv.Unquote(e.Value); err == nil && len([]rune(s)) == 1 {
			return []rune(s)[0], true
		}
		return 0, false
	}
	if c, ok := constOf(expr, info).(*types.Const); ok {
		if v, ok := constant.Int64Val(c.Val()); ok && v >= 0 && v <= unicode.MaxRune {
			return rune(v), true
		}
	}
	return 0, false
}

// constStrings returns the literal string values of a slice composite literal.
func constStrings(expr ast.Expr, info *types.Info) ([]string, bool) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	out := []string{}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			el = kv.Value
		}
		s, ok := constString(el, info)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// constNames returns the string values of a list of constant arguments.
func constNames(args []ast.Expr, info *types.Info) ([]string, bool) {
	out := make([]string, 0, len(args))
	for _, a := range args {
		s, ok := constString(a, info)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// constOf resolves an identifier (or selector chain) to its defining object.
func constOf(expr ast.Expr, info *types.Info) types.Object {
	if e, ok := expr.(*ast.Ident); ok {
		if o := info.Uses[e]; o != nil {
			return o
		}
		if o := info.Defs[e]; o != nil {
			return o
		}
	}
	if e, ok := expr.(*ast.SelectorExpr); ok {
		if o := info.Uses[e.Sel]; o != nil {
			return o
		}
	}
	return nil
}

// namedFunc resolves a (possibly aliased) identifier to its defining function.
func namedFunc(expr ast.Expr, info *types.Info) *types.Func {
	// follow selector chains to the base identifier (pkg.Func)
	for {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok {
			break
		}
		expr = sel.X
	}
	if f, ok := constOf(expr, info).(*types.Func); ok {
		return f
	}
	return nil
}

// safeWord reports whether s can be written unquoted inside a macro argument:
// non-empty, no whitespace, and no characters that need YAML macro quoting.
var safeWordRe = regexp.MustCompile(`^[A-Za-z0-9_./:@=,-]+$`)

// quoteMacro formats a string for use inside a spec macro, keeping the
// output readable where possible.
func quoteMacro(s string) string {
	if s == "" {
		return `""`
	}
	if safeWordRe.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}
