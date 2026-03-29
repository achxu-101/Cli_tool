package scanner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"upgrador/internal/config"
	"upgrador/internal/lookup"
)

// Component represents a discovered, potentially upgradeable component.
type Component struct {
	Name             string
	Group            string // "OS", "Binaries", "Services", "Helm Charts"
	Current          string // detected version string
	IsInstalled      bool
	BinaryPath       string   // full path if binary
	Method           string   // upgrade method from lookup or config
	GithubRepo       string   // if method is github_*; for Helm Charts: namespace
	AptPackage       string   // for Helm Charts: chart name (e.g. "argo-cd")
	ScriptURL        string
	IsKnown          bool     // true if in lookup table or user config
	IsUnknown        bool     // true if not in lookup or config — needs user input
	SelectedPackages []string // for apt: specific packages to upgrade (nil = all)
	Namespace        string   // for Helm: the release namespace
	KubeconfigPath   string   // for Helm: kubeconfig path (set by TUI before upgrade)
}

// AptPackage represents a single upgradable apt package with version and size info.
type AptPackage struct {
	Name        string
	CurrentVer  string
	NewVer      string
	InstalledKB int64 // installed size in KB from apt-cache show
}

// ScanAptPackages returns the full list of individually upgradable apt packages
// with their current/new versions and installed sizes.
func ScanAptPackages() ([]AptPackage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "apt", "list", "--upgradable").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("apt list: %w", err)
	}

	var names []string
	pkgMap := make(map[string]*AptPackage)

	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Listing") || line == "" {
			continue
		}
		// Format: "name/suite newver arch [upgradable from: oldver]"
		// Lines without "/" in the first field are warnings or headers — skip them.
		parts := strings.Fields(line)
		if len(parts) < 2 || !strings.Contains(parts[0], "/") {
			continue
		}
		name := strings.SplitN(parts[0], "/", 2)[0]
		newVer := parts[1]
		oldVer := ""
		for i, p := range parts {
			if p == "from:" && i+1 < len(parts) {
				oldVer = strings.TrimSuffix(parts[i+1], "]")
				break
			}
		}
		names = append(names, name)
		pkgMap[name] = &AptPackage{Name: name, CurrentVer: oldVer, NewVer: newVer}
	}

	// Fetch installed sizes for all packages in one apt-cache show call.
	if len(names) > 0 {
		args := append([]string{"show", "--no-all-versions"}, names...)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		sizeOut, err := exec.CommandContext(ctx2, "apt-cache", args...).Output()
		cancel2()
		if err == nil {
			currentPkg := ""
			sc2 := bufio.NewScanner(bytes.NewReader(sizeOut))
			for sc2.Scan() {
				l := sc2.Text()
				if strings.HasPrefix(l, "Package: ") {
					currentPkg = strings.TrimPrefix(l, "Package: ")
				} else if strings.HasPrefix(l, "Installed-Size: ") && currentPkg != "" {
					var kb int64
					fmt.Sscanf(strings.TrimPrefix(l, "Installed-Size: "), "%d", &kb)
					if p, ok := pkgMap[currentPkg]; ok {
						p.InstalledKB = kb
					}
				}
			}
		}
	}

	result := make([]AptPackage, 0, len(names))
	for _, name := range names {
		result = append(result, *pkgMap[name])
	}
	return result, nil
}

var versionRe = regexp.MustCompile(`v?\d+\.\d+[\.\d]*`)

var scanDirs = []string{
	"/usr/local/bin",
	"/usr/bin",
	"/usr/sbin",
	"/usr/local/sbin",
}

// versionProbes are tried in order until one returns output with a version.
var versionProbes = [][]string{
	{"--version"},
	{"version"},
	{"-v"},
	{"-V"},
	{"version", "--client-only"},
}

// probeVersion runs each version probe in order and returns the first version
// string found, or "" if none succeed.
func probeVersion(binPath string) string {
	for _, args := range versionProbes {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := exec.CommandContext(ctx, binPath, args...).CombinedOutput()
		cancel()
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			if m := versionRe.FindString(sc.Text()); m != "" {
				return m
			}
		}
	}
	return ""
}

// enrichBinary populates Method/GithubRepo/etc from the user config (priority)
// or the built-in lookup table. Sets IsUnknown if neither has an entry.
func enrichBinary(c *Component, cfg *config.Config) {
	if ub, ok := cfg.GetBinary(c.Name); ok {
		c.Method = ub.Method
		c.GithubRepo = ub.GithubRepo
		c.AptPackage = ub.AptPackage
		c.ScriptURL = ub.ScriptURL
		c.IsKnown = true
		return
	}
	if kb, ok := lookup.LookupBinary(c.Name); ok {
		c.Method = string(kb.Method)
		c.GithubRepo = kb.GithubRepo
		c.AptPackage = kb.AptPackage
		c.ScriptURL = kb.ScriptURL
		c.IsKnown = true
		return
	}
	c.IsUnknown = true
}

// ScanApt returns a single OS component summarising upgradable apt packages.
func ScanApt() []Component {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "apt", "list", "--upgradable").CombinedOutput()
	if err != nil {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// Subtract 1 for the "Listing..." header line.
	n := len(lines) - 1
	if n < 0 {
		n = 0
	}

	return []Component{{
		Name:        "apt packages",
		Group:       "OS",
		Current:     fmt.Sprintf("%d packages upgradable", n),
		IsInstalled: true,
		IsKnown:     true,
		Method:      "apt",
	}}
}

// ScanBinaries discovers executables in standard bin directories, probes their
// versions, and enriches each with its upgrade method.
// Binaries that cannot be version-detected are still included if they are in
// the lookup table or user config, marked as "version unknown".
func ScanBinaries(cfg *config.Config) []Component {
	seen := make(map[string]struct{}) // keyed by resolved real path
	var results []Component

	for _, dir := range scanDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			name := entry.Name()

			// Check lookup table and user config before probing — skips
			// hundreds of unrelated binaries on systems like Kali.
			_, inConfig := cfg.GetBinary(name)
			_, inLookup := lookup.LookupBinary(name)
			if !inConfig && !inLookup {
				continue
			}

			fullPath := filepath.Join(dir, name)

			// Resolve symlinks so we don't probe the same binary twice.
			realPath, err := filepath.EvalSymlinks(fullPath)
			if err != nil {
				realPath = fullPath
			}
			if _, already := seen[realPath]; already {
				continue
			}

			// Skip non-executables.
			info, err := os.Stat(fullPath)
			if err != nil || info.Mode()&0o111 == 0 {
				continue
			}

			version := probeVersion(fullPath)
			if version == "" {
				version = "version unknown"
			}

			seen[realPath] = struct{}{}

			c := Component{
				Name:        name,
				Group:       "Binaries",
				Current:     version,
				IsInstalled: true,
				BinaryPath:  fullPath,
			}
			enrichBinary(&c, cfg)
			results = append(results, c)
		}
	}
	return results
}

// serviceVersion tries `{name} --version` first, then falls back to reading
// the systemd Description property.
func serviceVersion(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	out, err := exec.CommandContext(ctx, name, "--version").CombinedOutput()
	cancel()
	if err == nil {
		if m := versionRe.FindString(string(out)); m != "" {
			return m
		}
	}

	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	out, err = exec.CommandContext(ctx, "systemctl", "show", name, "-p", "Description").Output()
	cancel()
	if err == nil {
		line := strings.TrimPrefix(strings.TrimSpace(string(out)), "Description=")
		if m := versionRe.FindString(line); m != "" {
			return m
		}
	}
	return ""
}

// ScanServices discovers active systemd services that have a known upgrade method.
func ScanServices(cfg *config.Config) []Component {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx,
		"systemctl", "list-units",
		"--type=service", "--state=active",
		"--no-legend", "--plain",
	).Output()
	if err != nil {
		return nil
	}

	var results []Component
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ".service")

		var c Component
		if ub, ok := cfg.GetBinary(name); ok {
			c = Component{
				Name:       name,
				Group:      "Services",
				Method:     ub.Method,
				GithubRepo: ub.GithubRepo,
				AptPackage: ub.AptPackage,
				ScriptURL:  ub.ScriptURL,
				IsKnown:    true,
			}
		} else if ks, ok := lookup.LookupService(name); ok {
			c = Component{
				Name:       name,
				Group:      "Services",
				Method:     string(ks.Method),
				GithubRepo: ks.GithubRepo,
				AptPackage: ks.AptPackage,
				ScriptURL:  ks.ScriptURL,
				IsKnown:    true,
			}
		} else {
			continue // no known upgrade path — skip
		}

		c.Current = serviceVersion(name)
		c.IsInstalled = true
		results = append(results, c)
	}
	return results
}

// findKubeconfig returns a kubeconfig path for helm commands, checking the
// same locations as the upgrader: KUBECONFIG env, SUDO_USER's home, then /root.
func findKubeconfig() string {
	if k := os.Getenv("KUBECONFIG"); k != "" {
		if _, err := os.Stat(k); err == nil {
			return k
		}
	}
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		p := fmt.Sprintf("/home/%s/.kube/config", sudoUser)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat("/root/.kube/config"); err == nil {
		return "/root/.kube/config"
	}
	return ""
}

// findSudoUserHome returns the home directory of the user who invoked sudo,
// or "" if not running under sudo.
func findSudoUserHome() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		p := fmt.Sprintf("/home/%s", sudoUser)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// helmCmdWithKube creates a helm exec.Cmd with an explicit kubeconfig path
// and the invoking user's helm XDG dirs injected.
func helmCmdWithKube(ctx context.Context, kubeconfig string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "helm", args...)
	env := os.Environ()
	if kubeconfig != "" {
		env = append(env, "KUBECONFIG="+kubeconfig)
	}
	if home := findSudoUserHome(); home != "" {
		env = append(env, "HELM_CONFIG_HOME="+home+"/.config/helm")
		env = append(env, "HELM_CACHE_HOME="+home+"/.cache/helm")
		env = append(env, "HELM_DATA_HOME="+home+"/.local/share/helm")
	}
	cmd.Env = env
	return cmd
}

// helmCmd creates a helm exec.Cmd using auto-detected kubeconfig.
func helmCmd(ctx context.Context, args ...string) *exec.Cmd {
	return helmCmdWithKube(ctx, findKubeconfig(), args...)
}

// helmRepoMap builds a map from chart-name prefix → repo name by running
// `helm repo list` and `helm search repo` cross-referencing. Returns an empty
// map if helm is unavailable.
func helmRepoMap(kubeconfig string) map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := helmCmdWithKube(ctx, kubeconfig, "repo", "list", "-o", "json").Output()
	if err != nil {
		return nil
	}

	var repos []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(out, &repos); err != nil || len(repos) == 0 {
		return nil
	}

	// For each repo, run `helm search repo <repo>/` and collect chart names.
	m := make(map[string]string)
	for _, repo := range repos {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		searchOut, err := helmCmdWithKube(ctx2, kubeconfig, "search", "repo", repo.Name+"/", "-o", "json").Output()
		cancel2()
		if err != nil {
			continue
		}
		var hits []struct {
			Name string `json:"name"` // "repo/chart-name"
		}
		if err := json.Unmarshal(searchOut, &hits); err != nil {
			continue
		}
		for _, h := range hits {
			// Strip "repo/" prefix to get bare chart name.
			bare := strings.TrimPrefix(h.Name, repo.Name+"/")
			m[bare] = repo.Name
		}
	}
	return m
}

// helmKubeconfigCandidates returns the ordered list of kubeconfig paths to try,
// with configOverride (from saved config or user input) first.
func helmKubeconfigCandidates(configOverride string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if configOverride != "" {
		add(configOverride)
	}
	if k := os.Getenv("KUBECONFIG"); k != "" {
		add(k)
	}
	if home := findSudoUserHome(); home != "" {
		add(home + "/.kube/config")
	}
	add("/root/.kube/config")
	add("/etc/rancher/k3s/k3s.yaml")
	add("") // bare helm — uses whatever helm finds on its own
	return out
}

// scanHelmChartsWithKubeconfig runs helm list with a specific kubeconfig.
// Returns nil if helm returns an error or no releases.
func scanHelmChartsWithKubeconfig(kubeconfig string) []Component {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := helmCmdWithKube(ctx, kubeconfig, "list", "--all-namespaces", "-o", "json").Output()
	if err != nil {
		return nil
	}

	var releases []struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		Chart      string `json:"chart"`
		AppVersion string `json:"app_version"`
	}
	if err := json.Unmarshal(out, &releases); err != nil || len(releases) == 0 {
		return nil
	}

	repoMap := helmRepoMap(kubeconfig) // chart-name → repo-name

	results := make([]Component, 0, len(releases))
	for _, r := range releases {
		chartName, chartVer := splitChartVersion(r.Chart)
		version := chartVer
		if version == "" {
			version = r.AppVersion
		}

		repoName := repoMap[chartName]
		isUnknown := repoName == ""
		if isUnknown {
			repoName = r.Namespace
		}

		method := "helm_upgrade"
		if kr, ok := lookup.LookupHelmRelease(r.Name); ok {
			method = string(kr.Method)
			isUnknown = false
		}

		results = append(results, Component{
			Name:        r.Name,
			Group:       "Helm Charts",
			Current:     version,
			IsInstalled: true,
			IsKnown:     !isUnknown,
			IsUnknown:   isUnknown,
			Method:      method,
			GithubRepo:  repoName,
			AptPackage:  chartName,
			Namespace:   r.Namespace,
		})
	}
	return results
}

// ScanHelmCharts lists all Helm releases across all namespaces.
// configOverride is the kubeconfig path saved in config (may be empty).
// It tries multiple kubeconfig paths in order and returns the first success.
// Always returns a non-nil slice (may be empty if no charts found).
func ScanHelmCharts(configOverride string) []Component {
	for _, kube := range helmKubeconfigCandidates(configOverride) {
		if comps := scanHelmChartsWithKubeconfig(kube); len(comps) > 0 {
			return comps
		}
	}
	return []Component{}
}

// splitChartVersion splits a Helm chart field like "argo-cd-9.1.0" or
// "cert-manager-v1.11.4" into chart name and version. A version segment
// starts with a digit or with 'v' followed by a digit.
func splitChartVersion(chart string) (name, version string) {
	parts := strings.Split(chart, "-")
	for i := len(parts) - 1; i >= 1; i-- {
		p := parts[i]
		if len(p) == 0 {
			continue
		}
		startsWithDigit := p[0] >= '0' && p[0] <= '9'
		startsWithV := len(p) > 1 && p[0] == 'v' && p[1] >= '0' && p[1] <= '9'
		if startsWithDigit || startsWithV {
			return strings.Join(parts[:i], "-"), strings.Join(parts[i:], "-")
		}
	}
	return chart, ""
}

// safeRun calls fn and recovers from any panic, logging it instead of crashing.
func safeRun(name string, fn func() []Component) (result []Component) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("scanner %s panicked: %v", name, r)
		}
	}()
	return fn()
}

// ScanAllWithProgress is like ScanAll but calls progress before each step
// so callers can display the current scan stage to the user.
func ScanAllWithProgress(cfg *config.Config, progress func(string)) []Component {
	var all []Component
	progress("Scanning apt packages...")
	all = append(all, safeRun("ScanApt", func() []Component { return ScanApt() })...)
	progress("Scanning binaries...")
	all = append(all, safeRun("ScanBinaries", func() []Component { return ScanBinaries(cfg) })...)
	progress("Scanning services...")
	all = append(all, safeRun("ScanServices", func() []Component { return ScanServices(cfg) })...)
	progress("Scanning Helm charts...")
	all = append(all, safeRun("ScanHelmCharts", func() []Component { return ScanHelmCharts(cfg.KubeconfigPath) })...)
	return deduplicate(all)
}

// ScanAll runs all scanners and returns a deduplicated component list.
// Each scanner is protected by a recover so a single failure never crashes the scan.
// When a name appears in both Binaries and Services, the Services entry wins.
func ScanAll(cfg *config.Config) []Component {
	var all []Component
	all = append(all, safeRun("ScanApt", func() []Component { return ScanApt() })...)
	all = append(all, safeRun("ScanBinaries", func() []Component { return ScanBinaries(cfg) })...)
	all = append(all, safeRun("ScanServices", func() []Component { return ScanServices(cfg) })...)
	all = append(all, safeRun("ScanHelmCharts", func() []Component { return ScanHelmCharts(cfg.KubeconfigPath) })...)
	return deduplicate(all)
}

// deduplicate preserves insertion order while preferring the Services entry
// when the same name appears in multiple groups.
func deduplicate(components []Component) []Component {
	byName := make(map[string]Component, len(components))
	order := make([]string, 0, len(components))

	for _, c := range components {
		existing, exists := byName[c.Name]
		if !exists {
			byName[c.Name] = c
			order = append(order, c.Name)
			continue
		}
		// Service entry beats binary entry.
		if existing.Group != "Services" && c.Group == "Services" {
			byName[c.Name] = c
		}
	}

	result := make([]Component, 0, len(order))
	for _, name := range order {
		result = append(result, byName[name])
	}
	return result
}
