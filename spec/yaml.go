// Package spec renders a carapace command.Command as carapace-spec YAML
// (or JSON), mirroring the output of the official `carapace spec` command.
package spec

import (
	"bytes"
	"encoding/json"

	cmd "github.com/carapace-sh/carapace/pkg/command"
	"go.yaml.in/yaml/v3"
)

// SchemaHint is the IDE hint the official spec command prefixes.
const SchemaHint = "# yaml-language-server: $schema=https://carapace.sh/schemas/command.json"

// ToYAML renders the command as spec YAML with a schema hint.
func ToYAML(c cmd.Command) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return SchemaHint + "\n" + buf.String(), nil
}

// ClearCompletion removes the carapace2spec-specific completion: section from every
// command in the tree, matching the official `carapace spec` output exactly.
func ClearCompletion(c *cmd.Command) {
	if c == nil {
		return
	}
	var zero cmd.Command
	c.Completion = zero.Completion
	for i := range c.Commands {
		ClearCompletion(&c.Commands[i])
	}
}

// ToJSON renders the command as spec JSON.
func ToJSON(c cmd.Command) (string, error) {
	out, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
