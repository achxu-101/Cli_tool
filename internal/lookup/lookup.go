package lookup

// Method describes how a binary or service is upgraded.
type Method string

const (
	MethodGithubTarball Method = "github_tarball"
	MethodGithubBinary  Method = "github_binary"
	MethodRancherScript Method = "rancher_script"
	MethodK3sScript     Method = "k3s_script"
	MethodHelmScript    Method = "helm_script"
	MethodApt           Method = "apt"
	MethodCustomScript  Method = "custom_script"
	MethodSkip          Method = "skip"
)

// KnownBinary describes how to upgrade a well-known binary.
type KnownBinary struct {
	Name        string
	Method      Method
	GithubRepo  string   // e.g. "vmware-tanzu/velero"
	AptPackage  string   // e.g. "keepalived"
	ScriptURL   string   // for custom scripts
	VersionArgs []string // e.g. ["version", "--client-only"]
	VersionFlag string   // e.g. "--version"
}

// KnownService describes how to upgrade a well-known system service.
type KnownService struct {
	Name       string
	Method     Method
	GithubRepo string
	AptPackage string
	ScriptURL  string
}

var knownBinaries = []KnownBinary{
	{
		Name:        "velero",
		Method:      MethodGithubTarball,
		GithubRepo:  "vmware-tanzu/velero",
		VersionArgs: []string{"version", "--client-only"},
	},
	{
		Name:       "keepalived-exporter",
		Method:     MethodGithubTarball,
		GithubRepo: "mehdy/keepalived-exporter",
	},
	{
		Name:       "helmfile",
		Method:     MethodGithubTarball,
		GithubRepo: "helmfile/helmfile",
	},
	{
		Name:   "helm",
		Method: MethodHelmScript,
	},
	{
		Name:       "kubectl",
		Method:     MethodGithubBinary,
		GithubRepo: "kubernetes/kubernetes",
	},
	{
		Name:   "k3s",
		Method: MethodK3sScript,
	},
	{
		Name:   "docker",
		Method: MethodRancherScript,
	},
	{
		Name:       "argocd",
		Method:     MethodGithubBinary,
		GithubRepo: "argoproj/argo-cd",
	},
	{
		Name:       "flux",
		Method:     MethodGithubTarball,
		GithubRepo: "fluxcd/flux2",
	},
}

// knownHelmReleases maps Helm release names to their upgrade method.
// Entries here take priority over the scanner's repo-detection for known releases.
var knownHelmReleases = []KnownBinary{
	{
		// Managed by k3s — upgraded automatically with k3s itself.
		Name:   "traefik",
		Method: MethodSkip,
	},
	{
		Name:   "traefik-crd",
		Method: MethodSkip,
	},
}

var knownServices = []KnownService{
	{
		Name:       "keepalived",
		Method:     MethodApt,
		AptPackage: "keepalived",
	},
	{
		Name:   "docker",
		Method: MethodRancherScript,
	},
	{
		Name:   "k3s",
		Method: MethodK3sScript,
	},
	{
		Name:       "containerd",
		Method:     MethodApt,
		AptPackage: "containerd",
	},
}

// Build lookup maps once at init time.
var (
	binaryMap      map[string]*KnownBinary
	serviceMap     map[string]*KnownService
	helmReleaseMap map[string]*KnownBinary
)

func init() {
	binaryMap = make(map[string]*KnownBinary, len(knownBinaries))
	for i := range knownBinaries {
		binaryMap[knownBinaries[i].Name] = &knownBinaries[i]
	}

	serviceMap = make(map[string]*KnownService, len(knownServices))
	for i := range knownServices {
		serviceMap[knownServices[i].Name] = &knownServices[i]
	}

	helmReleaseMap = make(map[string]*KnownBinary, len(knownHelmReleases))
	for i := range knownHelmReleases {
		helmReleaseMap[knownHelmReleases[i].Name] = &knownHelmReleases[i]
	}
}

// LookupBinary returns the upgrade definition for a well-known binary.
func LookupBinary(name string) (*KnownBinary, bool) {
	b, ok := binaryMap[name]
	return b, ok
}

// LookupService returns the upgrade definition for a well-known service.
func LookupService(name string) (*KnownService, bool) {
	s, ok := serviceMap[name]
	return s, ok
}

// LookupHelmRelease returns the upgrade definition for a well-known Helm release.
func LookupHelmRelease(name string) (*KnownBinary, bool) {
	r, ok := helmReleaseMap[name]
	return r, ok
}

// AllKnownBinaryNames returns the names of all binaries in the registry.
func AllKnownBinaryNames() []string {
	names := make([]string, len(knownBinaries))
	for i, b := range knownBinaries {
		names[i] = b.Name
	}
	return names
}
