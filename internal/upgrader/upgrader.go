package upgrader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"upgrador/internal/scanner"
)

const (
	userAgent  = "upgrador/1.0"
	apiTimeout = 10 * time.Second
)

var httpClient = &http.Client{Timeout: apiTimeout}

// step writes a visible section header to w.
func step(w io.Writer, msg string) {
	fmt.Fprintf(w, "→ %s\n", msg)
}

// streamShell runs cmd via bash and streams stdout+stderr live to w.
// In dry-run mode it prints what it would do instead.
func streamShell(cmd string, w io.Writer, dryRun bool) error {
	if dryRun {
		fmt.Fprintf(w, "[DRY RUN] would run: %s\n", cmd)
		return nil
	}
	c := exec.Command("bash", "-c", cmd)
	c.Stdout = w
	c.Stderr = w
	return c.Run()
}

// releaseAsset is a single GitHub release asset.
type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// findGithubAsset fetches the GitHub releases API and returns the download URL
// for the linux/amd64 tarball (wantTarball=true) or single binary (false).
func findGithubAsset(repo, tag string, wantTarball bool) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var release struct {
		Assets []releaseAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	for _, asset := range release.Assets {
		n := strings.ToLower(asset.Name)
		if !strings.Contains(n, "linux") || !strings.Contains(n, "amd64") {
			continue
		}
		// Skip checksums, signatures, SBOMs.
		for _, skip := range []string{".sha256", ".sha512", ".sig", ".pem", ".sbom", ".txt"} {
			if strings.HasSuffix(n, skip) {
				goto nextAsset
			}
		}
		{
			isTar := strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tgz")
			if isTar == wantTarball {
				return asset.BrowserDownloadURL, nil
			}
		}
	nextAsset:
	}
	return "", fmt.Errorf("no linux/amd64 asset found for %s@%s", repo, tag)
}

// RunUpgrade executes the upgrade for c, streaming all output to w.
// If dryRun is true, commands are printed but not executed.
func RunUpgrade(c scanner.Component, version string, w io.Writer, dryRun bool) error {
	if c.Group == "Helm Charts" {
		return upgradeHelmChart(c, version, w, dryRun)
	}
	switch c.Method {
	case "github_tarball":
		return upgradeGithubTarball(c, version, w, dryRun)
	case "github_binary":
		return upgradeGithubBinary(c, version, w, dryRun)
	case "rancher_script":
		return upgradeDocker(c, version, w, dryRun)
	case "k3s_script":
		return upgradeK3s(c, version, w, dryRun)
	case "helm_script":
		return upgradeHelm(w, dryRun)
	case "apt":
		return upgradeApt(c.SelectedPackages, w, dryRun)
	case "custom_script":
		return upgradeCustomScript(c, w, dryRun)
	default:
		return fmt.Errorf("unsupported upgrade method: %s", c.Method)
	}
}

func upgradeGithubTarball(c scanner.Component, version string, w io.Writer, dryRun bool) error {
	step(w, fmt.Sprintf("Downloading %s %s...", c.Name, version))

	if dryRun {
		fmt.Fprintf(w, "[DRY RUN] would fetch GitHub asset for %s/%s (%s)\n", c.GithubRepo, version, "tarball")
		fmt.Fprintf(w, "[DRY RUN] would install to /usr/local/bin/%s\n", c.Name)
		return nil
	}

	assetURL, err := findGithubAsset(c.GithubRepo, version, true)
	if err != nil {
		return err
	}

	tmpTar := fmt.Sprintf("/tmp/%s.tar.gz", c.Name)
	tmpDir := fmt.Sprintf("/tmp/%s-extracted", c.Name)

	defer func() {
		exec.Command("bash", "-c", fmt.Sprintf("rm -rf %s %s", tmpTar, tmpDir)).Run() //nolint:errcheck
	}()

	if err := streamShell(fmt.Sprintf("curl -sfL %q -o %s", assetURL, tmpTar), w, false); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := streamShell(fmt.Sprintf("mkdir -p %s && tar -xzf %s -C %s", tmpDir, tmpTar, tmpDir), w, false); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	step(w, fmt.Sprintf("Installing %s...", c.Name))
	return streamShell(fmt.Sprintf(
		`find %s -name %s -type f | head -1 | xargs -I{} install -m 755 {} /usr/local/bin/%s`,
		tmpDir, c.Name, c.Name,
	), w, false)
}

func upgradeGithubBinary(c scanner.Component, version string, w io.Writer, dryRun bool) error {
	step(w, fmt.Sprintf("Downloading %s %s...", c.Name, version))

	if dryRun {
		fmt.Fprintf(w, "[DRY RUN] would fetch GitHub asset for %s/%s (binary)\n", c.GithubRepo, version)
		fmt.Fprintf(w, "[DRY RUN] would install to /usr/local/bin/%s\n", c.Name)
		return nil
	}

	assetURL, err := findGithubAsset(c.GithubRepo, version, false)
	if err != nil {
		return err
	}

	dest := fmt.Sprintf("/usr/local/bin/%s", c.Name)
	if err := streamShell(fmt.Sprintf("curl -sfL %q -o %s", assetURL, dest), w, false); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	return streamShell(fmt.Sprintf("chmod +x %s", dest), w, false)
}

func upgradeDocker(_ scanner.Component, version string, w io.Writer, dryRun bool) error {
	step(w, fmt.Sprintf("Installing Docker %s via Rancher script...", version))
	if err := streamShell(fmt.Sprintf(
		"curl -sfL https://releases.rancher.com/install-docker/%s.sh | sh", version,
	), w, dryRun); err != nil {
		return err
	}
	step(w, "Restarting Docker...")
	return streamShell("systemctl restart docker", w, dryRun)
}

func upgradeK3s(_ scanner.Component, version string, w io.Writer, dryRun bool) error {
	step(w, fmt.Sprintf("Installing K3s %s...", version))
	if err := streamShell(fmt.Sprintf(
		`curl -sfL https://get.k3s.io | `+
			`INSTALL_K3S_VERSION=%q `+
			`INSTALL_K3S_EXEC="--disable traefik --docker --data-dir /data/k3s --kubelet-arg root-dir=/data/kubelet" `+
			`sh -s -`,
		version,
	), w, dryRun); err != nil {
		return err
	}
	step(w, "Waiting for nodes to be Ready...")
	return streamShell(
		"KUBECONFIG=/etc/rancher/k3s/k3s.yaml kubectl wait --for=condition=Ready node --all --timeout=300s",
		w, dryRun,
	)
}

func upgradeHelm(w io.Writer, dryRun bool) error {
	step(w, "Installing latest Helm 3...")
	return streamShell(
		"curl -sfL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash",
		w, dryRun,
	)
}

func upgradeApt(packages []string, w io.Writer, dryRun bool) error {
	step(w, "Updating apt cache...")
	if err := streamShell("apt-get update -q", w, dryRun); err != nil {
		return err
	}
	if len(packages) > 0 {
		step(w, fmt.Sprintf("Upgrading %d selected packages...", len(packages)))
		return streamShell(fmt.Sprintf(
			`DEBIAN_FRONTEND=noninteractive apt-get install --only-upgrade -y %s`,
			strings.Join(packages, " "),
		), w, dryRun)
	}
	step(w, "Running dist-upgrade...")
	return streamShell(
		`DEBIAN_FRONTEND=noninteractive apt-get dist-upgrade -y `+
			`-o Dpkg::Options::="--force-confold" `+
			`-o Dpkg::Options::="--force-confdef"`,
		w, dryRun,
	)
}

func upgradeHelmChart(c scanner.Component, version string, w io.Writer, dryRun bool) error {
	const kubeconfig = "KUBECONFIG=/home/ubuntu/.kube/config"

	step(w, "Updating Helm repos...")
	if err := streamShell(kubeconfig+" helm repo update", w, dryRun); err != nil {
		return err
	}

	// AptPackage = chart name, GithubRepo = repo name (see scanner).
	chart := c.AptPackage
	repoName := c.GithubRepo

	step(w, fmt.Sprintf("Upgrading %s to %s...", c.Name, version))
	return streamShell(fmt.Sprintf(
		`%s helm upgrade %s %s/%s `+
			`--namespace %s --version %s --reuse-values --wait --timeout 10m`,
		kubeconfig, c.Name, repoName, chart, c.GithubRepo, version,
	), w, dryRun)
}

func upgradeCustomScript(c scanner.Component, w io.Writer, dryRun bool) error {
	step(w, "Running custom script...")
	if strings.HasPrefix(c.ScriptURL, "http://") || strings.HasPrefix(c.ScriptURL, "https://") {
		return streamShell(fmt.Sprintf("curl -sfL %q | bash", c.ScriptURL), w, dryRun)
	}
	return streamShell(c.ScriptURL, w, dryRun)
}
