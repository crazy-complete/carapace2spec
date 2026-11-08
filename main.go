// Command carapace2spec converts a carapace-sh completer (cobra + carapace) into a
// carapace spec YAML file without executing the completer.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/crazy-complete/carapace2spec/convert"
	"github.com/crazy-complete/carapace2spec/spec"
)

func main() {
	var (
		out          = flag.String("o", "", "output file (default: stdout)")
		jsonFmt      = flag.Bool("json", false, "emit JSON instead of YAML")
		noCompletion = flag.Bool("no-completion", false, "omit the completion: section (byte-compatible with carapace spec)")
	)
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: carapace2spec [-o file] [-json] [-no-completion] <completer directory>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	dir := flag.Arg(0)

	res, err := convert.Convert(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "carapace2spec:", err)
		os.Exit(1)
	}
	for _, w := range res.Warnings {
		msg := w.Message
		if w.Position != "" {
			msg = w.Position + ": " + msg
		}
		fmt.Fprintln(os.Stderr, "warning: "+msg)
	}

	if *noCompletion {
		spec.ClearCompletion(&res.Spec)
	}

	var doc string
	if *jsonFmt {
		doc, err = spec.ToJSON(res.Spec)
	} else {
		doc, err = spec.ToYAML(res.Spec)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "carapace2spec:", err)
		os.Exit(1)
	}
	if *out == "" {
		fmt.Print(doc)
		return
	}
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "carapace2spec:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "carapace2spec: wrote "+*out)
}
