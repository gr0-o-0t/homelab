package cmd

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var tsCmd = &cobra.Command{
	Use:     "tailscale",
	Aliases: []string{"ts"},
	Short:   "Tailscale node information",
}

var tsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print Tailscale node info and IP",
	Long:  `Print Tailscale status and the Caddy node's IP address. Use the IP as the A record value in Cloudflare DNS.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		r := run.Default()

		fmt.Printf("\n%s\n", styles.Primary.Render("── Tailscale status ─────────────────────────────────"))
		if err := r.DockerExec("tailscale", "tailscale", "status"); err != nil {
			return err
		}

		fmt.Printf("\n%s\n", styles.Primary.Render("── Tailscale IP (use as A record value in Cloudflare) "))
		if err := r.DockerExec("tailscale", "tailscale", "ip", "-4"); err != nil {
			return err
		}

		fmt.Printf("\n%s\n", styles.Primary.Render("── FQDN ─────────────────────────────────────────────"))
		out, err := exec.Command(
			"docker", "exec", "tailscale",
			"tailscale", "status", "--self", "--json",
		).Output()
		if err == nil {
			var self struct{ DNSName string }
			if json.Unmarshal(out, &self) == nil && self.DNSName != "" {
				fmt.Println(strings.TrimSuffix(self.DNSName, "."))
			}
		}
		fmt.Println()
		return nil
	},
}

func init() {
	tsCmd.AddCommand(tsStatusCmd)
}
