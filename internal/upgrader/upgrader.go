package upgrader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"upgrador/internal/config"
	"upgrador/internal/scanner"
)

// DetectKubeconfig finds a usable kubeconfig path, trying common locations.
// Returns "" if none is found.
func DetectKubeconfig() string {
	// 1. Explicit KUBECONFIG env var.
	if k := os.Getenv("KUBECONFIG"); k != "" {
		if _, err := os.Stat(k); err == nil {
			return k
		}
	}
	// 2. The invoking user's kubeconfig (upgrador runs as root via sudo).
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		p := fmt.Sprintf("/home/%s/.kube/config", sudoUser)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 3. Root's kubeconfig.
	if _, err := os.Stat("/root/.kube/config"); err == nil {
		return "/root/.kube/config"
	}
	return ""
}

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
		packages := c.SelectedPackages
		// For a specific service managed via apt (e.g. containerd), upgrade only
		// that package — not a full dist-upgrade of everything on the system.
		if len(packages) == 0 && c.AptPackage != "" {
			packages = []string{c.AptPackage}
		}
		return upgradeApt(packages, w, dryRun)
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
	cfg, _ := config.Load()
	if cfg != nil && cfg.GetDockerMethod() == config.DockerMethodOfficial {
		return upgradeDockerOfficial(w, dryRun)
	}
	return upgradeDockerRancher(version, w, dryRun)
}

func upgradeDockerRancher(version string, w io.Writer, dryRun bool) error {
	// Strip any display-only suffix (e.g. " (Rancher pending)") from the version.
	if idx := strings.Index(version, " "); idx >= 0 {
		version = version[:idx]
	}
	step(w, fmt.Sprintf("Installing Docker %s via Rancher script...", version))
	script := fmt.Sprintf("/tmp/install-docker-%s.sh", version)
	url := fmt.Sprintf("https://releases.rancher.com/install-docker/docker-v%s.sh", version)
	if err := streamShell(fmt.Sprintf(
		`curl -sfL '%s' -o '%s' && sh '%s'; r=$?; rm -f '%s'; exit $r`,
		url, script, script, script,
	), w, dryRun); err != nil {
		return err
	}
	step(w, "Restarting Docker...")
	return streamShell("systemctl restart docker", w, dryRun)
}

func upgradeDockerOfficial(w io.Writer, dryRun bool) error {
	step(w, "Installing latest Docker via get.docker.com...")
	script := "/tmp/install-docker-official.sh"
	if err := streamShell(fmt.Sprintf(
		`curl -sfL https://get.docker.com -o '%s' && sh '%s'; r=$?; rm -f '%s'; exit $r`,
		script, script, script,
	), w, dryRun); err != nil {
		return err
	}
	step(w, "Restarting Docker...")
	return streamShell("systemctl restart docker", w, dryRun)
}

func upgradeK3s(_ scanner.Component, version string, w io.Writer, dryRun bool) error {
	const k3sKube = "KUBECONFIG=/etc/rancher/k3s/k3s.yaml"

	// Discover nodes so we can cordon/drain/uncordon each one safely.
	step(w, "Discovering cluster nodes...")
	var nodes []string
	if !dryRun {
		out, err := exec.Command("kubectl",
			"--kubeconfig=/etc/rancher/k3s/k3s.yaml",
			"get", "nodes", "-o", "jsonpath={.items[*].metadata.name}",
		).Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			nodes = strings.Fields(string(out))
		}
	} else {
		nodes = []string{"<node>"}
	}
	fmt.Fprintf(w, "nodes: %s\n", strings.Join(nodes, ", "))

	// Cordon and drain every node before touching the control plane.
	for _, node := range nodes {
		step(w, fmt.Sprintf("Cordoning %s...", node))
		if err := streamShell(fmt.Sprintf("%s kubectl cordon %s", k3sKube, node), w, dryRun); err != nil {
			fmt.Fprintf(w, "warning: cordon %s failed: %v\n", node, err)
		}

		step(w, fmt.Sprintf("Draining %s...", node))
		drain := fmt.Sprintf(
			`%s kubectl drain %s --ignore-daemonsets --delete-emptydir-data --timeout=300s --force`,
			k3sKube, node,
		)
		if err := streamShell(drain, w, dryRun); err != nil {
			// On single-node clusters some pods can't be evicted — log and continue.
			fmt.Fprintf(w, "warning: drain %s incomplete (continuing): %v\n", node, err)
		}
	}

	// Upgrade k3s.
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

	// Wait for Ready then uncordon each node.
	for _, node := range nodes {
		step(w, fmt.Sprintf("Waiting for %s to be Ready...", node))
		if err := streamShell(fmt.Sprintf(
			"%s kubectl wait --for=condition=Ready node/%s --timeout=300s",
			k3sKube, node,
		), w, dryRun); err != nil {
			fmt.Fprintf(w, "warning: %s not ready within timeout\n", node)
		}

		step(w, fmt.Sprintf("Uncordoning %s...", node))
		if err := streamShell(fmt.Sprintf("%s kubectl uncordon %s", k3sKube, node), w, dryRun); err != nil {
			fmt.Fprintf(w, "warning: uncordon %s failed: %v\n", node, err)
		}
	}

	return nil
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
	// Prefer kubeconfig set by TUI prompt; fall back to auto-detection.
	kubeconfigPath := c.KubeconfigPath
	if kubeconfigPath == "" {
		kubeconfigPath = DetectKubeconfig()
	}
	if kubeconfigPath == "" {
		return fmt.Errorf("kubeconfig not found — set KUBECONFIG or re-run and provide the path when prompted")
	}
	// Build env prefix for all helm and kubectl commands.
	envParts := []string{"KUBECONFIG=" + kubeconfigPath}
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		home := fmt.Sprintf("/home/%s", sudoUser)
		envParts = append(envParts,
			"HELM_CONFIG_HOME="+home+"/.config/helm",
			"HELM_CACHE_HOME="+home+"/.cache/helm",
			"HELM_DATA_HOME="+home+"/.local/share/helm",
		)
	}
	env := strings.Join(envParts, " ")

	namespace := c.Namespace
	if namespace == "" {
		namespace = "default"
	}

	chart := c.AptPackage
	repoName := c.GithubRepo

	step(w, "Updating Helm repos...")
	if err := streamShell(env+" helm repo update", w, dryRun); err != nil {
		return err
	}

	// Apply CRDs before upgrading — Helm does not update CRDs automatically.
	step(w, fmt.Sprintf("Checking CRD updates for %s/%s %s...", repoName, chart, version))
	crdCmd := fmt.Sprintf(
		`crds=$(%s helm show crds %s/%s --version %s 2>/dev/null); `+
			`if [ -n "$crds" ]; then `+
			`echo "$crds" | KUBECONFIG=%s kubectl apply --server-side --force-conflicts -f -; `+
			`else echo "  no CRDs to update"; fi`,
		env, repoName, chart, version, kubeconfigPath,
	)
	if err := streamShell(crdCmd, w, dryRun); err != nil {
		fmt.Fprintf(w, "warning: CRD update failed (proceeding): %v\n", err)
	}

	step(w, fmt.Sprintf("Upgrading %s to %s in namespace %s...", c.Name, version, namespace))
	return streamShell(fmt.Sprintf(
		`%s helm upgrade %s %s/%s --namespace %s --version %s --reset-then-reuse-values --wait --timeout 10m`,
		env, c.Name, repoName, chart, namespace, version,
	), w, dryRun)
}

func upgradeCustomScript(c scanner.Component, w io.Writer, dryRun bool) error {
	step(w, "Running custom script...")
	if strings.HasPrefix(c.ScriptURL, "http://") || strings.HasPrefix(c.ScriptURL, "https://") {
		return streamShell(fmt.Sprintf("curl -sfL %q | bash", c.ScriptURL), w, dryRun)
	}
	return streamShell(c.ScriptURL, w, dryRun)
}
