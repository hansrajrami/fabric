/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func pipelineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pipeline",
		Short: "Show this peer's local MST relayer status (outbox counts, relayer account, gas balance)",
		Long: "Reads the embedded relayer's metrics endpoint (mst.metricsAddr in core.yaml). " +
			"Requires the metrics endpoint to be enabled on the peer.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			addr := viper.GetString("mst.metricsAddr")
			if addr == "" {
				return fmt.Errorf("mst.metricsAddr is not set: enable the MST metrics endpoint on this peer to use 'peer mst pipeline'")
			}
			base := addr
			if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
				base = "http://" + base
			}
			client := &http.Client{Timeout: 5 * time.Second}
			w := cmd.OutOrStdout()

			// Liveness.
			body, err := httpGet(client, base+"/healthz")
			if err != nil {
				return fmt.Errorf("relayer metrics endpoint unreachable at %s: %w", addr, err)
			}
			fmt.Fprintf(w, "relayer      : %s\n", strings.TrimSpace(body))

			metrics, err := httpGet(client, base+"/metrics")
			if err != nil {
				return fmt.Errorf("reading /metrics: %w", err)
			}
			renderMetrics(w, metrics)
			return nil
		},
	}
	return cmd
}

func httpGet(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return string(body), nil
}

// renderMetrics prints the interesting MST gauges from the Prometheus text
// exposition: per-channel outbox entry counts and the relayer gas balance.
// Comment lines demarcate channels ("# channel <id>") and the relayer account.
func renderMetrics(w io.Writer, text string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "# channel "):
			fmt.Fprintf(w, "channel %s\n", strings.TrimPrefix(line, "# channel "))
		case strings.HasPrefix(line, "# relayer account"):
			fmt.Fprintln(w, "relayer account")
		case strings.HasPrefix(line, "#"):
			// other comment / HELP / TYPE lines: skip
		case strings.HasPrefix(line, "mst_outbox_entries"):
			fmt.Fprintf(w, "  %s\n", line)
		case strings.HasPrefix(line, "mst_outbox_oldest_active_age_seconds"):
			fmt.Fprintf(w, "  %s\n", line)
		case strings.HasPrefix(line, "mst_relayer_balance_gwei"):
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
}
