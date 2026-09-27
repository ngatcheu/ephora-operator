package controller

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
)

func writeCredentials(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func basicHeader(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func TestGitEnv(t *testing.T) {
	const repo = "https://github.internal/team/app"

	t.Run("no credentials: anonymous, never prompts", func(t *testing.T) {
		r := &PreviewEnvironmentReconciler{GitCredentialsDir: t.TempDir()}
		env, err := r.gitEnv(repo)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
			t.Error("GIT_TERMINAL_PROMPT=0 not set")
		}
		for _, e := range env {
			if strings.HasPrefix(e, "GIT_CONFIG_KEY_0=") {
				t.Errorf("unexpected git config without credentials: %s", e)
			}
		}
	})

	t.Run("token scoped to the repository host, redirects disabled", func(t *testing.T) {
		// Trailing newline, as often left by `kubectl create secret --from-file`.
		r := &PreviewEnvironmentReconciler{GitCredentialsDir: writeCredentials(t, map[string]string{"password": "tok3n\n"})}
		env, err := r.gitEnv(repo)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"GIT_CONFIG_KEY_0=http.https://github.internal/.extraheader",
			"GIT_CONFIG_VALUE_0=Authorization: " + basicHeader(defaultGitUsername, "tok3n"),
			"GIT_CONFIG_KEY_1=http.followRedirects",
			"GIT_CONFIG_VALUE_1=false",
		} {
			if !slices.Contains(env, want) {
				t.Errorf("env missing %q", want)
			}
		}
	})

	t.Run("custom username", func(t *testing.T) {
		r := &PreviewEnvironmentReconciler{GitCredentialsDir: writeCredentials(t, map[string]string{
			"username": "ci-bot", "password": "tok3n",
		})}
		env, err := r.gitEnv(repo)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(env, "GIT_CONFIG_VALUE_0=Authorization: "+basicHeader("ci-bot", "tok3n")) {
			t.Error("custom username not used")
		}
	})
}

// End to end with a real git client: the token reaches the Git server as an
// Authorization header, and never leaks into the error that ends up in the
// PreviewEnvironment status.
func TestResolveChartSendsGitCredentials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const token = "s3cr3t-t0ken"

	tests := []struct {
		name     string
		files    map[string]string
		wantAuth string
	}{
		{name: "with credentials", files: map[string]string{"password": token}, wantAuth: basicHeader(defaultGitUsername, token)},
		{name: "without credentials", files: nil, wantAuth: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var auths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				auths = append(auths, req.Header.Get("Authorization"))
				mu.Unlock()
				http.Error(w, "not found", http.StatusNotFound)
			}))
			defer srv.Close()
			// Tests are offline by default (suite_test.go); allow this local server.
			t.Setenv("GIT_ALLOW_PROTOCOL", "file:http")

			r := &PreviewEnvironmentReconciler{WorkDir: t.TempDir(), GitCredentialsDir: writeCredentials(t, tt.files)}
			_, _, err := r.resolveChart(context.Background(), ephoraiov1alpha1.ChartSource{
				Repo: srv.URL + "/team/app.git", ChartPath: "charts/app", Revision: "HEAD",
			})
			if err == nil {
				t.Fatal("expected the clone to fail against the stub server")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("token leaked into the error message: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(auths) == 0 {
				t.Fatal("git never reached the server")
			}
			if auths[0] != tt.wantAuth {
				t.Errorf("Authorization header = %q, want %q", auths[0], tt.wantAuth)
			}
		})
	}
}
