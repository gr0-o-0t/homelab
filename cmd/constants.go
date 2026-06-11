// Shared string constants for the CLI commands.
// Extracted to reduce goconst warnings and improve maintainability.
package cmd

// Command subcommand names reused across multiple commands.
const (
	cmdLogs   = "logs"
	cmdStatus = "status"
	cmdList   = "list"
)

// Docker container state strings.
const (
	containerStateRunning = "running"
)

// Environment variable names.
const (
	envDomain     = "DOMAIN"
	envHomeSub    = "HOME_SUBDOMAIN"
	envACMEEMail  = "ACME_EMAIL"
	envTSHostname = "TS_HOSTNAME"
)
