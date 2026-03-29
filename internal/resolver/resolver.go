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
			// Strip "v", keep only major.minor (e.g. "27.3").
			ver := strings.TrimPrefix(tag, "v")
			if parts := strings.SplitN(ver, ".", 3); len(parts) >= 2 {
				ver = parts[0] + "." + parts[1]
			}
			tag = ver
		}
		r.Latest = tag
		r.IsOutdated = outdated(c.Current, tag)

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
		r.IsOutdated = outdated(c.Current, tag)

	case "apt":
		r.Latest = "run dist-upgrade"
		var n int
		fmt.Sscanf(c.Current, "%d packages upgradable", &n)
		r.IsOutdated = n > 0

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
