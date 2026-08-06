package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate shell completion scripts",
	Long: `Generate autocompletion scripts for your shell.

Bash:
  homelab completion bash > /etc/bash_completion.d/homelab
  # or for the current user:
  homelab completion bash > ~/.bash_completion

Zsh:
  homelab completion zsh > "${fpath[1]}/_homelab"

Fish:
  homelab completion fish > ~/.config/fish/completions/homelab.fish

PowerShell:
  homelab completion powershell | Out-String | Invoke-Expression`,
	ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	Args:      cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletion(os.Stdout)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		case "powershell":
			return rootCmd.GenPowerShellCompletion(os.Stdout)
		default:
			return fmt.Errorf("unsupported shell %q\n\n  Valid options: bash, zsh, fish, powershell", args[0])
		}
	},
}

func init() {
	rootCmd.AddCommand(completionCmd)
}
