package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
func newChartRepo(t *testing.T) string {
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
			source:  ephoraiov1alpha1.ChartSource{Repo: "https://gitlab.internal/team/app", ChartPath: "charts/app", Revision: "HEAD"},
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
