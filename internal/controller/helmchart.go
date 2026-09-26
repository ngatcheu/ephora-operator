package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
)

// gitCloneTimeout bounds how long a chart checkout may take before the
// reconcile fails and gets retried with backoff.
const gitCloneTimeout = 2 * time.Minute

// resolveChart fetches spec.source.repo at spec.source.revision into a
// scratch directory and loads spec.source.chartPath from it.
//
// This is the piece of custom Go logic ADR-01 flags as a known risk of the
// Helm-plugin approach ("passage partiel en Go si la complexité augmente"):
// the chart to deploy is dynamic per PreviewEnvironment (the requesting
// team's own repo/revision), so it cannot be the single chart statically
// bundled/watched by the default operator-sdk Helm-plugin reconciler. We
// still only ever drive deployment through the Helm SDK (install/upgrade/
// uninstall) — no bespoke Kubernetes resource management replacing it — so
// this stays within ADR-01's "wrap a Helm chart deploy" decision.
//
// A full git clone (rather than a shallow fetch of the exact revision) is
// used for simplicity and because spec.source.revision may be a branch, tag,
// or arbitrary commit SHA; optimize to a shallow fetch later if clone time
// becomes a bottleneck against the < 3 minute provisioning NFR (DAT §6).
func (r *PreviewEnvironmentReconciler) resolveChart(ctx context.Context, source ephoraiov1alpha1.ChartSource) (*chart.Chart, func(), error) {
	logger := log.FromContext(ctx)

	// Defense in depth on top of the CRD pattern: a revision starting with
	// "-" would be parsed by git as an option (argument injection). Not
	// relying on "--end-of-options", which older git versions don't honor
	// for checkout.
	if strings.HasPrefix(source.Revision, "-") {
		return nil, nil, fmt.Errorf("invalid spec.source.revision %q: must not start with '-'", source.Revision)
	}

	workDir, err := os.MkdirTemp(r.workDir(), "ephora-chart-")
	if err != nil {
		return nil, nil, fmt.Errorf("creating chart scratch dir: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(workDir); err != nil {
			logger.Error(err, "failed to clean up chart scratch dir", "dir", workDir)
		}
	}

	cloneCtx, cancel := context.WithTimeout(ctx, gitCloneTimeout)
	defer cancel()

	if err := runGit(cloneCtx, "", "clone", "--quiet", "--", source.Repo, workDir); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("cloning %s: %w", source.Repo, err)
	}
	// Trailing "--": the argument before it is always a revision, never a path.
	if err := runGit(cloneCtx, workDir, "checkout", "--quiet", source.Revision, "--"); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("checking out revision %s: %w", source.Revision, err)
	}

	// Path traversal guard: spec.source.chartPath (e.g. "../../etc") must not
	// escape the checkout and load arbitrary files from the operator's disk.
	chartDir := filepath.Join(workDir, source.ChartPath)
	if rel, err := filepath.Rel(workDir, chartDir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		cleanup()
		return nil, nil, fmt.Errorf("invalid spec.source.chartPath %q: must stay within the repository", source.ChartPath)
	}
	ch, err := loader.Load(chartDir)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("loading chart from %s: %w", source.ChartPath, err)
	}
	return ch, cleanup, nil
}

func (r *PreviewEnvironmentReconciler) workDir() string {
	if r.WorkDir != "" {
		return r.WorkDir
	}
	return os.TempDir()
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Only the subcommand, not the full args: they include the random
		// scratch dir, which would make every failure message unique and churn
		// the Ready condition on each retry.
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// helmValues decodes spec.valuesOverride (arbitrary JSON) into the
// map[string]interface{} shape the Helm SDK expects. A nil override yields
// an empty map (chart defaults apply).
func helmValues(raw *ephoraiov1alpha1.PreviewEnvironmentSpec) (map[string]interface{}, error) {
	if raw.ValuesOverride == nil || len(raw.ValuesOverride.Raw) == 0 {
		return map[string]interface{}{}, nil
	}
	values := map[string]interface{}{}
	if err := json.Unmarshal(raw.ValuesOverride.Raw, &values); err != nil {
		return nil, fmt.Errorf("decoding spec.valuesOverride: %w", err)
	}
	return values, nil
}

// newActionConfig builds a Helm action.Configuration scoped to namespace,
// backed by the operator's own in-cluster REST config (no separate
// kubeconfig/credentials — same identity as the operator's ServiceAccount,
// per the RBAC model in DAT §5).
func (r *PreviewEnvironmentReconciler) newActionConfig(namespace string, logger func(format string, v ...interface{})) (*action.Configuration, error) {
	getter := &restClientGetter{restConfig: r.RESTConfig, namespace: namespace}
	cfg := new(action.Configuration)
	if err := cfg.Init(getter, namespace, "secrets", logger); err != nil {
		return nil, fmt.Errorf("initializing helm action config: %w", err)
	}
	return cfg, nil
}

// installOrUpgrade deploys ch as releaseName into namespace, installing if no
// release exists yet and upgrading otherwise (DAT §4.1, §4.3).
func installOrUpgrade(cfg *action.Configuration, releaseName, namespace string, ch *chart.Chart, values map[string]interface{}) (*release.Release, error) {
	hist := action.NewHistory(cfg)
	hist.Max = 1
	_, err := hist.Run(releaseName)

	switch {
	case err == driver.ErrReleaseNotFound:
		install := action.NewInstall(cfg)
		install.ReleaseName = releaseName
		install.Namespace = namespace
		install.CreateNamespace = false // the reconciler owns namespace lifecycle
		install.Timeout = 3 * time.Minute
		return install.Run(ch, values)
	case err != nil:
		return nil, fmt.Errorf("checking release history for %s: %w", releaseName, err)
	default:
		upgrade := action.NewUpgrade(cfg)
		upgrade.Namespace = namespace
		upgrade.Timeout = 3 * time.Minute
		return upgrade.Run(releaseName, ch, values)
	}
}

// uninstallRelease removes releaseName, tolerating it already being gone.
func uninstallRelease(cfg *action.Configuration, releaseName string) error {
	uninstall := action.NewUninstall(cfg)
	_, err := uninstall.Run(releaseName)
	if err != nil && err != driver.ErrReleaseNotFound {
		return fmt.Errorf("uninstalling release %s: %w", releaseName, err)
	}
	return nil
}

// restClientGetter is a minimal genericclioptions.RESTClientGetter backed by
// a static rest.Config, so the Helm SDK can be driven with the operator's own
// in-cluster credentials instead of loading a kubeconfig from disk.
type restClientGetter struct {
	restConfig *rest.Config
	namespace  string
}

func (g *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return g.restConfig, nil
}

func (g *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(g.restConfig)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (g *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}

func (g *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	overrides := &clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: g.namespace}}
	return clientcmd.NewDefaultClientConfig(*clientcmdapi.NewConfig(), overrides)
}
