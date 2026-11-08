package convert

import (
	"go/ast"
	"go/constant"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/carapace-sh/carapace/pkg/command"
)

// pflagBase describes the pflag type family of a flag method.
type pflagBase struct {
	zero   string // DefValue equivalent at zero state
	value  bool   // the flag takes a value
	repeat bool   // the flag is repeatable (slice/array/count)
}

var pflagBases = map[string]pflagBase{
	"String":       {zero: "", value: true},
	"Int":          {zero: "0", value: true},
	"Int8":         {zero: "0", value: true},
	"Int16":        {zero: "0", value: true},
	"Int32":        {zero: "0", value: true},
	"Int64":        {zero: "0", value: true},
	"Uint":         {zero: "0", value: true},
	"Uint8":        {zero: "0", value: true},
	"Uint16":       {zero: "0", value: true},
	"Uint32":       {zero: "0", value: true},
	"Uint64":       {zero: "0", value: true},
	"Float32":      {zero: "0", value: true},
	"Float64":      {zero: "0", value: true},
	"Duration":     {zero: "0s", value: true},
	"IP":           {zero: "<nil>", value: true},
	"IPMask":       {zero: "<nil>", value: true},
	"StringSlice":  {zero: "[]", value: true, repeat: true},
	"IntSlice":     {zero: "[]", value: true, repeat: true},
	"Int32Slice":   {zero: "[]", value: true, repeat: true},
	"Int64Slice":   {zero: "[]", value: true, repeat: true},
	"UintSlice":    {zero: "[]", value: true, repeat: true},
	"Float32Slice": {zero: "[]", value: true, repeat: true},
	"BoolSlice":    {zero: "[]", value: true, repeat: true},
	"StringArray":  {zero: "[]", value: true, repeat: true},
	"Bool":         {zero: "false"},
	"Count":        {zero: "0", repeat: true},
}

const (
	modePlain           = "plain"
	modePosix           = "posix"
	modeShorthandOnly   = "shorthand_only"
	modeNameAsShorthand = "name_as_shorthand"
)

// hasShorthand reports whether the mode carries a shorthand letter argument
// at offset+1 (P/S/N). modePlain (Bool, String, ...) does not.
func hasShorthand(mode string) bool {
	return mode == modePosix || mode == modeShorthandOnly || mode == modeNameAsShorthand
}

// parseFlagMethod maps a pflag method name to its base type family and
// shorthand mode. idpkg is the defining package of the resolved method.
func parseFlagMethod(method string, nargs int, idpkg string) (pflagBase, string, string, int, string) {
	// via `replace spf13/pflag => carapace-pflag` the import path stays
	// spf13; both spellings denote the fork.
	isFork := idpkg == forkPath || idpkg == spf13Path
	names := make([]string, 0, len(pflagBases))
	for n := range pflagBases {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	var base, suffix string
	for _, n := range names {
		switch {
		case method == n:
			base, suffix = n, ""
		case method == n+"P" || method == n+"S" || method == n+"N":
			base, suffix = n, method[len(n):]
		}
		if base != "" {
			break
		}
	}
	if base == "" {
		if strings.Contains(method, "Var") || strings.Contains(method, "Func") {
			return pflagBase{}, "", "", offsetOf(method),
				method + " binds the value to a pointer/function and is skipped"
		}
		return pflagBase{}, "", "", 0, "unknown pflag method " + method
	}

	b := pflagBases[base]
	offset := offsetOf(method)
	boolish := base == "Bool" || base == "Count"

	switch suffix {
	case "N":
		if !isFork {
			return pflagBase{}, "", "", offset, "NameAsShorthand mode requires carapace-pflag"
		}
		if boolish && nargs != 3+offset || !boolish && nargs != 4+offset {
			return pflagBase{}, "", "", offset, method + ": unexpected arity " + strconv.Itoa(nargs)
		}
		return b, base, modeNameAsShorthand, offset, ""
	case "S":
		if !isFork {
			return pflagBase{}, "", "", offset, "ShorthandOnly mode requires carapace-pflag"
		}
		if boolish && nargs != 4+offset && nargs != 3+offset {
			return pflagBase{}, "", "", offset, method + ": unexpected arity " + strconv.Itoa(nargs)
		}
		if !boolish && nargs != 4+offset {
			return pflagBase{}, "", "", offset, method + ": unexpected arity " + strconv.Itoa(nargs)
		}
		return b, base, modeShorthandOnly, offset, ""
	case "P":
		want := 4 + offset
		if base == "Count" {
			want = 3 + offset
		}
		if nargs != want {
			return pflagBase{}, "", "", offset, method + ":[P] unexpected arity " + strconv.Itoa(nargs) + " (want " + strconv.Itoa(want) + ")"
		}
		return b, base, modePosix, offset, ""
	default:
		if !boolish && nargs != 3+offset || (boolish && nargs != 3+offset) {
			if base == "Count" {
				if nargs != 2+offset {
					return pflagBase{}, "", "", offset, method + ": unexpected arity " + strconv.Itoa(nargs)
				}
			} else {
				return pflagBase{}, "", "", offset, method + ": unexpected arity " + strconv.Itoa(nargs)
			}
		}
		return b, base, modePlain, offset, ""
	}
}

// offsetOf detects a leading pointer/function argument (XVar, XFunc flags).
func offsetOf(method string) int {
	if strings.Contains(method, "Var") || strings.Contains(method, "Func") {
		return 1
	}
	return 0
}

const (
	forkPath  = "github.com/carapace-sh/carapace-pflag"
	spf13Path = "github.com/spf13/pflag"
)

// buildFlag converts a pflag registration into a command.Flag.
func (c *conv) buildFlag(reg flagReg, info *types.Info) (command.Flag, bool) {
	base, pname, mode, offset, warn := parseFlagMethod(reg.method, len(reg.args), reg.idpkg)
	if warn != "" {
		c.warnAt(reg.node, warn)
		return command.Flag{}, false
	}
	if len(reg.args) <= offset {
		c.warnAt(reg.node, reg.method+": missing arguments")
		return command.Flag{}, false
	}

	name, ok := constString(argAt(reg.args, offset), info)
	if !ok {
		c.warnAt(reg.node, reg.method+": non-static flag name")
		return command.Flag{}, false
	}
	usage := ""
	if l := len(reg.args); l > 0 {
		if s, ok := constString(argAt(reg.args, l-1), info); ok {
			usage = s
		}
	}

	f := command.Flag{
		Description: usage,
		Value:       base.value,
		Repeatable:  base.repeat,
		Persistent:  reg.persistent,
	}
	sh := ""
	if hasShorthand(mode) {
		if s, ok := constString(argAt(reg.args, offset+1), info); ok {
			sh = s
		}
	}
	switch mode {
	case modeShorthandOnly:
		f.Shorthand = sh
	case modeNameAsShorthand:
		f.Longhand = name
		f.Shorthand = sh
		f.NameAsShorthand = true
	default:
		f.Longhand = name
		if sh != "" {
			f.Shorthand = sh
		}
	}

	// default value for value flags and bools
	last := len(reg.args)
	switch {
	case base.value && last >= 2:
		arg := argAt(reg.args, last-2)
		switch pname {
		case "Duration":
			if d, ok := constDuration(arg, info); ok && d != 0 {
				f.Default = d.String()
			}
		case "String", "IP", "IPMask",
			"StringSlice", "StringArray":
			if v, ok := constString(arg, info); ok && v != base.zero && v != "" {
				f.Default = v
			}
		default:
			// numeric families (Int*, Uint*, Float*, Time, Count)
			if v, ok := constInt(arg, info); ok && v != 0 {
				f.Default = strconv.Itoa(v)
			}
		}
	case base.zero == "false" && last >= 2:
		if v, ok := constBool(argAt(reg.args, last-2), info); ok && v {
			f.Default = "true"
		}
	case base.zero == "0" && last >= 2:
		if v, ok := constInt(argAt(reg.args, last-2), info); ok && v != 0 {
			f.Default = strconv.Itoa(v)
		}
	}
	return f, true
}

func isShorthandMode(mode string) bool {
	return mode == modeShorthandOnly || mode == modeNameAsShorthand
}

func argAt(args []ast.Expr, i int) ast.Expr {
	if i < 0 || i >= len(args) {
		return nil
	}
	return args[i]
}

// constBool returns the boolean value of a constant expression.
func constBool(expr ast.Expr, info *types.Info) (bool, bool) {
	if expr == nil {
		return false, false
	}
	switch e := expr.(type) {
	case *ast.Ident:
		if e.Name == "true" {
			return true, true
		}
		if e.Name == "false" {
			return false, true
		}
		if c, ok := constOf(expr, info).(*types.Const); ok {
			if b, ok2 := c.Type().Underlying().(*types.Basic); ok2 && b.Kind() == types.Bool {
				return constant.BoolVal(c.Val()), true
			}
		}
	case *ast.UnaryExpr:
		if e.Op.String() == "!" {
			if b, ok := constBool(e.X, info); ok {
				return !b, true
			}
		}
	}
	return false, false
}

// flagKey formats a flag the same way command.Flag.format does.
func flagKey(f command.Flag) string {
	var s string
	if f.Shorthand != "" {
		s = "-" + f.Shorthand
		if f.Longhand != "" {
			s += ", "
		}
	}
	if f.Longhand != "" {
		switch {
		case f.NameAsShorthand:
			s += "-" + f.Shorthand
		default:
			s += "--" + f.Longhand
		}
	}
	switch {
	case f.Optarg && f.Value:
		s += "?"
	case f.Value:
		s += "="
	}
	if f.Repeatable {
		s += "*"
	}
	if f.Required {
		s += "!"
	}
	if f.Hidden {
		s += "&"
	}
	return s
}
