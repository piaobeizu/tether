package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/piaobeizu/tether/internal/doctor"
	"github.com/piaobeizu/tether/internal/server"
)

const version = "v0.1.0-dev"

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "tether",
	Short: "tether — AI workspace daemon + browser UI",
	CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
}

func init() {
	rootCmd.AddCommand(versionCmd, newServerCmd(), attachCmd, pairCmd, newDoctorCmd())
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Println(version)
	},
}

func newServerCmd() *cobra.Command {
	cfg := &server.Config{}
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start the tether HTTP/3 + WebTransport server",
		RunE: func(_ *cobra.Command, _ []string) error {
			return server.Run(cfg)
		},
	}
	cmd.Flags().IntVarP(&cfg.Port, "port", "p", 8898, "listen port (TCP + UDP)")
	cmd.Flags().StringVar(&cfg.CertFile, "cert-file", "", "TLS cert PEM (bypasses auto-rotation)")
	cmd.Flags().StringVar(&cfg.KeyFile, "key-file", "", "TLS key PEM (bypasses auto-rotation)")
	cmd.Flags().BoolVar(&cfg.DevMode, "dev", false, "proxy SPA to Vite dev server")
	cmd.Flags().StringVar(&cfg.DevFrontendURL, "dev-url", "", "Vite dev server URL (default http://localhost:5173)")
	cmd.Flags().StringVar(&cfg.Token, "token", "", "static access token (runtime only, not persisted)")
	return cmd
}

var attachCmd = &cobra.Command{
	Use:   "attach",
	Short: "Attach to a running session (stub — implemented in s6)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("attach: not yet implemented (s6)")
	},
}

var pairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair with a remote tether instance (stub — implemented in s6)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("pair: not yet implemented (s6)")
	},
}

func newDoctorCmd() *cobra.Command {
	var port int
	var verbose bool
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks",
		RunE: func(_ *cobra.Command, _ []string) error {
			report := doctor.Run(port, verbose)
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			for _, c := range report.Checks {
				mark := "✓"
				if !c.OK {
					mark = "✗"
				}
				fmt.Printf("  %s  %-22s  %s\n", mark, c.Name, c.Message)
				if verbose && c.Detail != "" {
					fmt.Printf("       %s\n", c.Detail)
				}
			}
			if !report.OK {
				return fmt.Errorf("one or more preflight checks failed")
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&port, "port", "p", 8898, "port to check bindability for")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show extra detail")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}
