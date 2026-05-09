package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

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
	rootCmd.AddCommand(versionCmd, newServerCmd(), attachCmd, pairCmd, doctorCmd)
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

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Run preflight checks (stub — implemented in s5.5)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("doctor: not yet implemented (s5.5)")
	},
}
