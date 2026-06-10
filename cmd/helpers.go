package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// detectServicePort reads the service's caddy.conf and extracts the
// reverse_proxy target port.
func detectServicePort(root, name string) (string, error) {
	caddyConf := filepath.Join(root, "services", name, "caddy.conf")
	data, err := os.ReadFile(caddyConf) // nosec G304 -- path is programmatically constructed
	if err != nil {
		return "", err
	}
	line := string(data)
	prefix := "reverse_proxy " + name + ":"
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return "", fmt.Errorf("could not find 'reverse_proxy %s:<port>' in caddy.conf", name)
	}
	rest := line[idx+len(prefix):]
	end := strings.IndexAny(rest, " \t\n\r")
	if end < 0 {
		return "", fmt.Errorf("malformed reverse_proxy line")
	}
	return rest[:end], nil
}
