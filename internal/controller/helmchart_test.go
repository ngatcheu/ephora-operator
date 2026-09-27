package controller

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/runtime"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
)

func TestNamespaceName(t *testing.T) {
	pe := &ephoraiov1alpha1.PreviewEnvironment{
		Spec: ephoraiov1alpha1.PreviewEnvironmentSpec{PRNumber: 1234, AppName: "checkout-api"},
	}
	if got, want := namespaceName(pe), "preview-pr-1234-checkout-api"; got != want {
		t.Errorf("namespaceName() = %q, want %q", got, want)
	}
}

func TestHelmValues(t *testing.T) {
	tests := []struct {
		name    string
		raw     *runtime.RawExtension
		want    map[string]interface{}
		wantErr bool
	}{
		{name: "nil override", raw: nil, want: map[string]interface{}{}},
		{name: "empty override", raw: &runtime.RawExtension{}, want: map[string]interface{}{}},
		{
			name: "nested values",
			raw:  &runtime.RawExtension{Raw: []byte(`{"replicaCount":2,"image":{"tag":"pr-1"}}`)},
			want: map[string]interface{}{
				"replicaCount": float64(2),
				"image":        map[string]interface{}{"tag": "pr-1"},
			},
		},
		{name: "invalid JSON", raw: &runtime.RawExtension{Raw: []byte(`{not json`)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := helmValues(&ephoraiov1alpha1.PreviewEnvironmentSpec{ValuesOverride: tt.raw})
			if (err != nil) != tt.wantErr {
				t.Fatalf("helmValues() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("helmValues() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// newChartRepo creates a local git repository holding a minimal chart at
// charts/app, so resolveChart can be exercised without any network access.
// Optional extra funcs add files to the chart directory before the commit.
func newChartRepo(t *testing.T, extra ...func(chartDir string)) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	chartDir := filepath.Join(dir, "charts", "app")
	if err := os.MkdirAll(chartDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chartYAML := "apiVersion: v2\nname: app\nversion: 0.1.0\n"
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range extra {
		f(chartDir)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"add", "."},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false",
			"commit", "--quiet", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestResolveChart(t *testing.T) {
	repo := newChartRepo(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		source    ephoraiov1alpha1.ChartSource
		wantErr   string // substring; empty means success
		wantChart string
	}{
		{
			name:      "loads chart at revision",
			source:    ephoraiov1alpha1.ChartSource{Repo: repo, ChartPath: "charts/app", Revision: "HEAD"},
			wantChart: "app",
		},
		{
			name:    "rejects revision parsed as git option",
			source:  ephoraiov1alpha1.ChartSource{Repo: repo, ChartPath: "charts/app", Revision: "--upload-pack=touch /tmp/pwned"},
			wantErr: "must not start with '-'",
		},
		{
			name:    "rejects chartPath escaping the repository",
			source:  ephoraiov1alpha1.ChartSource{Repo: repo, ChartPath: "../../etc", Revision: "HEAD"},
			wantErr: "must stay within the repository",
		},
		{
			name:    "rejects https repo (tests are offline)",
			source:  ephoraiov1alpha1.ChartSource{Repo: "https://github.internal/team/app", ChartPath: "charts/app", Revision: "HEAD"},
			wantErr: "cloning",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			r := &PreviewEnvironmentReconciler{WorkDir: workDir}

			ch, cleanup, err := r.resolveChart(ctx, tt.source)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveChart() error = %v, want error containing %q", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("resolveChart() unexpected error: %v", err)
				}
				if ch.Name() != tt.wantChart {
					t.Errorf("chart name = %q, want %q", ch.Name(), tt.wantChart)
				}
				cleanup()
			}

			// Success or failure, no checkout may be left behind.
			entries, err := os.ReadDir(workDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("scratch dir not cleaned up: %d entries left in %s", len(entries), workDir)
			}
		})
	}
}

// A chart comes from the PR author: a symlink to a file on the operator's
// disk (e.g. its ServiceAccount token) must not expose that file's content.
func TestResolveChartDoesNotFollowSymlinks(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "token")
	const secret = "super-secret-token"
	if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := newChartRepo(t, func(chartDir string) {
		if err := os.Symlink(secretFile, filepath.Join(chartDir, "leak")); err != nil {
			t.Fatal(err)
		}
	})

	r := &PreviewEnvironmentReconciler{WorkDir: t.TempDir()}
	ch, cleanup, err := r.resolveChart(context.Background(),
		ephoraiov1alpha1.ChartSource{Repo: repo, ChartPath: "charts/app", Revision: "HEAD"})
	if err != nil {
		t.Fatalf("resolveChart() unexpected error: %v", err)
	}
	defer cleanup()

	found := false
	for _, f := range ch.Files {
		if f.Name != "leak" {
			continue
		}
		found = true
		if bytes.Contains(f.Data, []byte(secret)) {
			t.Fatal("symlink was followed: target file content leaked into the chart")
		}
	}
	if !found {
		t.Error("expected the symlink to be checked out as a plain file named 'leak'")
	}
}

func testChart() *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: chart.APIVersionV2, Name: "app", Version: "0.1.0"},
		Templates: []*chart.File{{
			Name: "templates/configmap.yaml",
			Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n"),
		}},
	}
}

// newTestActionConfig returns a Helm configuration backed by in-memory
// release storage and a no-op Kubernetes client.
func newTestActionConfig() *action.Configuration {
	return &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...interface{}) {},
	}
}

func TestInstallOrUpgrade(t *testing.T) {
	const name, ns = "app", "preview-pr-1-app"

	tests := []struct {
		name        string
		seed        *release.Status // existing revision 1, if any
		wantVersion int
	}{
		{name: "no release: install", seed: nil, wantVersion: 1},
		{name: "deployed release: upgrade", seed: ptr(release.StatusDeployed), wantVersion: 2},
		{name: "first install failed: reinstall", seed: ptr(release.StatusFailed), wantVersion: 1},
		{name: "interrupted install: reinstall", seed: ptr(release.StatusPendingInstall), wantVersion: 1},
		{name: "interrupted upgrade: reinstall", seed: ptr(release.StatusPendingUpgrade), wantVersion: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newTestActionConfig()
			if tt.seed != nil {
				if err := cfg.Releases.Create(&release.Release{
					Name: name, Namespace: ns, Version: 1, Chart: testChart(),
					Info: &release.Info{Status: *tt.seed},
				}); err != nil {
					t.Fatal(err)
				}
			}

			rel, err := installOrUpgrade(cfg, name, ns, testChart(), map[string]interface{}{})
			if err != nil {
				t.Fatalf("installOrUpgrade() error: %v", err)
			}
			if rel.Info.Status != release.StatusDeployed {
				t.Errorf("status = %s, want %s", rel.Info.Status, release.StatusDeployed)
			}
			if rel.Version != tt.wantVersion {
				t.Errorf("version = %d, want %d", rel.Version, tt.wantVersion)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestNextRequeue(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		expiresAt time.Time
		want      time.Duration
	}{
		{"far expiry capped at resync period", now.Add(48 * time.Hour), resyncPeriod},
		{"expiry sooner than resync", now.Add(2 * time.Minute), 2 * time.Minute},
		{"already expired: never zero", now.Add(-time.Minute), time.Second},
		{"expiring now: never zero", now, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextRequeue(now, tt.expiresAt); got != tt.want {
				t.Errorf("nextRequeue() = %v, want %v", got, tt.want)
			}
		})
	}
}
