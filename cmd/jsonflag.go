package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// jsonAnnotation marks a command that reads the root --json flag. Every
// other command refuses --json (checkJSON) instead of printing human text.
const jsonAnnotation = "homelab.json"

// jsonSupported is the Annotations value of a command that honours --json.
var jsonSupported = map[string]string{jsonAnnotation: "true"}

// supportsJSON reports whether c, or a command between it and the root,
// honours --json. The root counts only when c is the root itself.
func supportsJSON(c *cobra.Command) bool {
	for ; c != nil; c = c.Parent() {
		if c.Annotations[jsonAnnotation] == "true" {
			return true
		}
		if !c.HasParent() || c.Parent() == rootCmd {
			return false
		}
	}
	return false
}

// checkJSON fails --json on a command that would ignore it. It runs before
// the command is forwarded, so nothing reaches the leader.
func checkJSON(c *cobra.Command) error {
	if !rootFlags.json || supportsJSON(c) {
		return nil
	}
	if name := c.Name(); name == "help" || strings.HasPrefix(name, "__") {
		return nil
	}
	return fmt.Errorf("--json is not supported by %q", c.CommandPath())
}
