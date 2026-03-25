package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"upgrador/internal/config"
	"upgrador/internal/tui"
)

const (
	version      = "1.0.0"
	selfUpdateRepo = "yourorg/upgrador"
)

func main() {
	// 1. Root check.
	if os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "\033[33m⚠ upgrador must be run as root (sudo upgrador)\033[0m\n")
		os.Exit(1)
	}

	// 2. Flags.
	versionFlag := flag.Bool("version", false, "print version and exit")
	dryRunFlag  := flag.Bool("dry-run", false, "show what would be upgraded without running upgrades")
	offlineFlag := flag.Bool("offline", false, "skip resolver; manually enter target versions")
	updateFlag  := flag.Bool("update", false, "download and install the latest upgrador release")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("upgrador v%s\n", version)
		os.Exit(0)
	}

	if *updateFlag {
		if err := selfUpdate(); err != nil {
			fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("upgrador updated successfully.")
		os.Exit(0)
	}

	// 3. Config.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	// 4. Launch TUI.
	m := tui.New(cfg, *dryRunFlag, *offlineFlag)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// selfUpdate downloads the latest release binary from GitHub and replaces
// /usr/local/bin/upgrador.
func selfUpdate() error {
	fmt.Println("Checking for latest release...")

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", selfUpdateRepo)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "upgrador/"+version)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return err
	}

	// Find the linux/amd64 binary asset.
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	var downloadURL string
	for _, a := range release.Assets {
		if strings.Contains(a.Name, goos) && strings.Contains(a.Name, goarch) {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no asset found for %s/%s in release %s", goos, goarch, release.TagName)
	}

	fmt.Printf("Downloading upgrador %s...\n", release.TagName)

	// Download to a temp file then atomically replace the binary.
	tmp, err := os.CreateTemp("", "upgrador-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	dlReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	dlResp, err := client.Do(dlReq)
	if err != nil {
		return err
	}
	defer dlResp.Body.Close()

	if _, err := io.Copy(tmp, dlResp.Body); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}

	dest := "/usr/local/bin/upgrador"
	return exec.Command("install", "-m", "0755", tmp.Name(), dest).Run()
}

