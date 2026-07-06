/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
)

var metadataCmd = &cobra.Command{
	Use:   "metadata",
	Short: "Metadata server diagnostics",
	Long:  `Commands for inspecting the GCE metadata server emulator.`,
}

var metadataStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check metadata server health and configuration",
	Long: `Runs a series of diagnostic checks on the in-process GCE metadata server:
environment variables, port connectivity, health endpoint, token availability,
and iptables interception rules.

Exit code 0 means all critical checks passed; non-zero means at least one failed.`,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(runMetadataStatus())
	},
}

func init() {
	rootCmd.AddCommand(metadataCmd)
	metadataCmd.AddCommand(metadataStatusCmd)
}

func runMetadataStatus() int {
	failed := false

	// Environment variables
	envVars := []struct {
		name     string
		required bool
	}{
		{"SCION_METADATA_MODE", true},
		{"GCE_METADATA_HOST", true},
		{"GCE_METADATA_ROOT", true},
		{"SCION_METADATA_PORT", false},
		{"SCION_METADATA_SA_EMAIL", false},
		{"SCION_METADATA_PROJECT_ID", false},
	}

	fmt.Println("=== Environment ===")
	for _, ev := range envVars {
		val := os.Getenv(ev.name)
		if val == "" {
			if ev.required {
				fmt.Printf("[FAIL] %s is not set\n", ev.name)
				failed = true
			} else {
				fmt.Printf("[INFO] %s is not set\n", ev.name)
			}
		} else {
			fmt.Printf("[ OK ] %s = %s\n", ev.name, val)
		}
	}

	// Configuration
	fmt.Println("\n=== Configuration ===")
	cfg := metadata.ConfigFromEnv()
	if cfg == nil {
		fmt.Println("[FAIL] SCION_METADATA_MODE not set — metadata server is not configured")
		return 1
	}
	fmt.Printf("[ OK ] Mode: %s\n", cfg.Mode)
	fmt.Printf("[INFO] Port: %d\n", cfg.Port)
	if cfg.SAEmail != "" {
		fmt.Printf("[INFO] Service Account: %s\n", cfg.SAEmail)
	}
	if cfg.ProjectID != "" {
		fmt.Printf("[INFO] Project ID: %s\n", cfg.ProjectID)
	}

	// Port connectivity
	fmt.Println("\n=== Connectivity ===")
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fmt.Printf("[FAIL] Cannot connect to %s: %v\n", addr, err)
		failed = true
	} else {
		_ = conn.Close()
		fmt.Printf("[ OK ] Port %d is open\n", cfg.Port)
	}

	// Health endpoint
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		fmt.Printf("[FAIL] Health endpoint unreachable: %v\n", err)
		failed = true
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Println("[ OK ] Health endpoint returned 200")
		} else {
			fmt.Printf("[FAIL] Health endpoint returned %d\n", resp.StatusCode)
			failed = true
		}
	}

	// Token endpoint (assign mode only)
	if cfg.Mode == "assign" {
		fmt.Println("\n=== Token ===")
		tokenFile := hub.ReadTokenFile()
		if tokenFile != "" {
			fmt.Println("[ OK ] Hub token file is present")
		} else {
			fmt.Println("[WARN] Hub token file is empty or missing")
		}

		req, _ := http.NewRequest("GET",
			fmt.Sprintf("http://%s/computeMetadata/v1/instance/service-accounts/default/token", addr),
			nil)
		req.Header.Set("Metadata-Flavor", "Google")
		tokenResp, err := client.Do(req)
		if err != nil {
			fmt.Printf("[FAIL] Token endpoint unreachable: %v\n", err)
			failed = true
		} else {
			_ = tokenResp.Body.Close()
			if tokenResp.StatusCode == http.StatusOK {
				fmt.Println("[ OK ] Token endpoint returned 200")
			} else {
				fmt.Printf("[FAIL] Token endpoint returned %d\n", tokenResp.StatusCode)
				failed = true
			}
		}
	}

	// iptables check (best-effort, requires root)
	fmt.Println("\n=== iptables ===")
	if os.Getuid() != 0 {
		fmt.Println("[INFO] Not running as root — skipping iptables check")
	} else {
		portStr := strconv.Itoa(cfg.Port)
		cmd := exec.Command("iptables", "-t", "nat", "-C", "OUTPUT",
			"-d", "169.254.169.254", "-p", "tcp", "--dport", "80",
			"-j", "REDIRECT", "--to-port", portStr)
		if err := cmd.Run(); err != nil {
			fmt.Printf("[WARN] iptables REDIRECT rule not found (port %s)\n", portStr)
		} else {
			fmt.Printf("[ OK ] iptables REDIRECT rule present (169.254.169.254:80 -> localhost:%s)\n", portStr)
		}
	}

	if failed {
		fmt.Println("\n[RESULT] One or more checks FAILED")
		return 1
	}
	fmt.Println("\n[RESULT] All checks passed")
	return 0
}
