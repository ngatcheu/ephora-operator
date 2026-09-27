package controller

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultGitCredentialsDir is where the optional git credentials Secret is
// mounted (config/manager/manager.yaml). Files:
//   - password: HTTPS token (read-only access to the chart repositories);
//   - username: optional, defaults to "x-access-token".
//
// Read on every clone, so a rotated Secret is picked up without a restart.
const DefaultGitCredentialsDir = "/var/run/ephora/git"

const defaultGitUsername = "x-access-token"

// gitEnv returns the environment for git commands fetching repo.
//
// Credentials, when configured, are passed as an HTTP Authorization header
// through GIT_CONFIG_* environment variables (git >= 2.31), scoped to the
// repository's own scheme+host. The token therefore never appears on the
// command line (visible in `ps`), in the repository URL, in the clone's
// .git/config, or in git's error output (which ends up in the Ready
// condition). Redirects are disabled so the header can't follow the request
// to another host.
func (r *PreviewEnvironmentReconciler) gitEnv(repo string) ([]string, error) {
	// Fail fast instead of hanging on an interactive credential prompt.
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	password, err := readGitCredential(r.gitCredentialsDir(), "password")
	if err != nil || password == "" {
		return env, err
	}
	username, err := readGitCredential(r.gitCredentialsDir(), "username")
	if err != nil {
		return nil, err
	}
	if username == "" {
		username = defaultGitUsername
	}

	u, err := url.Parse(repo)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid repository URL %q: cannot scope git credentials to its host", repo)
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))

	return append(env,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http."+u.Scheme+"://"+u.Host+"/.extraheader",
		"GIT_CONFIG_VALUE_0="+header,
		"GIT_CONFIG_KEY_1=http.followRedirects",
		"GIT_CONFIG_VALUE_1=false",
	), nil
}

func (r *PreviewEnvironmentReconciler) gitCredentialsDir() string {
	if r.GitCredentialsDir != "" {
		return r.GitCredentialsDir
	}
	return DefaultGitCredentialsDir
}

// readGitCredential returns the trimmed content of dir/name, or "" if the
// file doesn't exist (credentials are optional: public repositories).
func readGitCredential(dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading git credential %q: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}
