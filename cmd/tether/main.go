package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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
	rootCmd.AddCommand(versionCmd, serverCmd, attachCmd, pairCmd, doctorCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Println(version)
	},
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the tether server (stub — implemented in s2)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("server: not yet implemented (s2)")
	},
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
