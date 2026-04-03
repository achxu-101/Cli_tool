package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"upgrador/internal/config"
	"upgrador/internal/scanner"
)

// helmEnv returns the environment for helm commands, injecting the invoking
// user's helm dirs when upgrador is running as root via sudo.
func helmEnv() []string {
	env := os.Environ()
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		home := fmt.Sprintf("/home/%s", sudoUser)
		env = append(env,
			"HELM_CONFIG_HOME="+home+"/.config/helm",
			"HELM_CACHE_HOME="+home+"/.cache/helm",
			"HELM_DATA_HOME="+home+"/.local/share/helm",
		)
	}
	return env
}

const (
	userAgent  = "upgrador/1.0"
	apiTimeout = 10 * time.Second
	maxConc    = 10
)

// Result pairs a component with its resolved latest version.
type Result struct {
	Component  scanner.Component
	Latest     string
	IsOutdated bool
	Error      string // non-empty if resolution failed
}

var httpClient = &http.Client{Timeout: apiTimeout}

// githubLatest fetches the latest release tag from the GitHub API.
// Returns "rate limited" (with nil error) on 403/429.
func githubLatest(repo string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
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

	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		return "rate limited", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %d for %s", resp.StatusCode, repo)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.TagName, nil
}

// helmRepoUpdate runs `helm repo update` once per process to ensure the local
// cache is fresh before searching. Errors are silently ignored.
var helmRepoUpdateOnce sync.Once

func ensureHelmRepoUpdated() {
	helmRepoUpdateOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "helm", "repo", "update")
		cmd.Env = helmEnv()
		cmd.Run() //nolint:errcheck
	})
}

// helmChartLatest queries the helm repo cache for the latest chart version.
// Tries "{repoName}/{chart}" first, then bare "{chart}" as a fallback.
func helmChartLatest(chart, repoName string) string {
	ensureHelmRepoUpdated()

	candidates := []string{chart}
	if repoName != "" {
		candidates = append([]string{repoName + "/" + chart}, candidates...)
	}
	for _, query := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		cmd := exec.CommandContext(ctx, "helm", "search", "repo", query, "--output", "json")
		cmd.Env = helmEnv()
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		var results []struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(out, &results); err != nil || len(results) == 0 {
			continue
		}
		if results[0].Version != "" {
			return results[0].Version
		}
	}
	return ""
}

// normalizedVersion strips the leading "v" and surrounding whitespace for comparison.
func normalizedVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// outdated returns true when current and latest differ after normalization.
func outdated(current, latest string) bool {
	if current == "not installed" || latest == "" || latest == "rate limited" {
		return false
	}
	return normalizedVersion(current) != normalizedVersion(latest)
}

// resolve fetches the latest version for a single component.
func resolve(c scanner.Component) Result {
	r := Result{Component: c}

	switch c.Method {
	case "github_tarball", "github_binary":
		tag, err := githubLatest(c.GithubRepo)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.Latest = tag
		r.IsOutdated = outdated(c.Current, tag)

	case "rancher_script":
		tag, err := githubLatest("moby/moby")
		if err != nil {
			r.Error = err.Error()
			return r
		}
		if tag != "rate limited" {
			// Strip "docker-v" or plain "v" prefix, keep only major.minor (e.g. "29.3").
			ver := tag
			for _, pfx := range []string{"docker-v", "v"} {
				if strings.HasPrefix(ver, pfx) {
					ver = strings.TrimPrefix(ver, pfx)
					break
				}
			}
			if parts := strings.SplitN(ver, ".", 3); len(parts) >= 2 {
				ver = parts[0] + "." + parts[1]
			}
			tag = ver
		}
		r.Latest = tag
		// Compare only major.minor of the installed version so "29.3.1" == "29.3".
		curVer := strings.TrimPrefix(c.Current, "v")
		if parts := strings.SplitN(curVer, ".", 3); len(parts) >= 2 {
			curVer = parts[0] + "." + parts[1]
		}
		if !outdated(curVer, tag) {
			break
		}
		// When using the Rancher installer (default), verify the script actually
		// exists for this version before marking Docker as outdatable. Rancher lags
		// behind moby/moby releases, so the latest GitHub tag may not have a
		// corresponding script yet.
		cfg, _ := config.Load()
		if cfg == nil || cfg.GetDockerMethod() == config.DockerMethodRancher {
			rancherURL := fmt.Sprintf("https://releases.rancher.com/install-docker/docker-v%s.sh", tag)
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodHead, rancherURL, nil)
			if req != nil {
				resp, err := httpClient.Do(req)
				if err != nil || resp.StatusCode != http.StatusOK {
					// Rancher hasn't published the script for this version yet.
					r.Latest = tag + " (Rancher pending)"
					break
				}
			}
		}
		r.IsOutdated = true

	case "k3s_script":
		tag, err := githubLatest("k3s-io/k3s")
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.Latest = tag
		r.IsOutdated = outdated(c.Current, tag)

	case "helm_script":
		tag, err := githubLatest("helm/helm")
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.Latest = tag
		// The helm install script only manages Helm 3; don't flag a newer major version as outdated.
		curMajor := strings.SplitN(strings.TrimPrefix(c.Current, "v"), ".", 2)[0]
		latMajor := strings.SplitN(strings.TrimPrefix(tag, "v"), ".", 2)[0]
		if curMajor == latMajor {
			r.IsOutdated = outdated(c.Current, tag)
		}

	case "apt":
		r.Latest = "run dist-upgrade"
		var n int
		if parsed, _ := fmt.Sscanf(c.Current, "%d packages upgradable", &n); parsed == 1 {
			// General apt-packages component — use the pre-counted number.
			r.IsOutdated = n > 0
		} else if c.AptPackage != "" {
			// Specific service managed via apt — compare installed vs candidate.
			// apt-cache policy is more reliable than apt list --upgradable because
			// it doesn't require a fresh apt-get update to be accurate.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			out, _ := exec.CommandContext(ctx, "apt-cache", "policy", c.AptPackage).Output()
			cancel()
			var installed, candidate string
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if rest, ok := strings.CutPrefix(line, "Installed:"); ok {
					installed = strings.TrimSpace(rest)
				} else if rest, ok := strings.CutPrefix(line, "Candidate:"); ok {
					candidate = strings.TrimSpace(rest)
				}
			}
			if installed != "" && installed != "(none)" && candidate != "" && candidate != "(none)" {
				r.IsOutdated = installed != candidate
			}
		}

	case "custom_script":
		r.Latest = "custom"
		r.IsOutdated = false

	case "skip":
		r.Latest = "skipped"
		r.IsOutdated = false

	default:
		// Helm Charts have group "Helm Charts" and method "helm_upgrade".
		if c.Group == "Helm Charts" {
			// AptPackage = chart name, GithubRepo = namespace (see scanner).
			ver := helmChartLatest(c.AptPackage, c.GithubRepo)
			r.Latest = ver
			r.IsOutdated = outdated(c.Current, ver)
		} else {
			r.Latest = "unknown"
		}
	}

	return r
}

// ResolveAll concurrently resolves the latest version for every component.
// At most maxConc resolutions run simultaneously.
func ResolveAll(components []scanner.Component) []Result {
	results := make([]Result, len(components))
	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup

	for i, c := range components {
		wg.Add(1)
		go func(idx int, comp scanner.Component) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = resolve(comp)
		}(i, c)
	}

	wg.Wait()
	return results
}
