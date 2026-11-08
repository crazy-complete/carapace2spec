// Package convert translates a carapace completer (a Go program wiring
// cobra commands to carapace completion actions) into a carapace spec in
// the declarative user-spec format described at carapace.sh.
//
// The conversion is static: the source is type-checked (golang.org/x/tools/
// go/packages) and every carapace API call is resolved by *types.Func
// identity, so aliased imports and renamed packages are handled. No part of
// the completer code is executed.
//
// Parts that cannot be expressed in spec format (runtime callbacks,
// non-static arguments, unknown actions) are reported as Warnings and
// omitted from the spec rather than guessed.
package convert

import (
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
)

// Warning reports a construct that could not be translated into spec format.
type Warning struct {
	Position string // "file:line"
	Message  string
	Snippet  string // source line at Position, best effort
}

func (w *Warning) String() string {
	if w.Snippet != "" {
		return fmt.Sprintf("%s: %s\n    %s", w.Position, w.Message, w.Snippet)
	}
	return fmt.Sprintf("%s: %s", w.Position, w.Message)
}

// position formats a file:line pair.
func position(fset *token.FileSet, node ast.Node) string {
	pos := fset.Position(node.Pos())
	return pos.Filename + ":" + strconv.Itoa(pos.Line)
}
