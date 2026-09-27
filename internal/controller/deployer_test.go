package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// createClusterRoleFromFile installs a real ClusterRole from config/deployer,
// under the prefixed name the operator binds.
func createClusterRoleFromFile(t *testing.T, ctx context.Context, file, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "deployer", file))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, role); err != nil {
		t.Fatal(err)
	}
	role.Name = name
	if err := k8sClient.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating ClusterRole %s: %v", name, err)
	}
}

func createDeployerClusterRole(t *testing.T, ctx context.Context) {
	createClusterRoleFromFile(t, ctx, "role.yaml", DefaultDeployerClusterRole)
}

// can asks the API server whether a user in group may perform verb on
// resource (optionally a subresource) in namespace.
func can(t *testing.T, ctx context.Context, group, verb, resource, subresource, namespace string) bool {
	t.Helper()
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   "alice",
			Groups: []string{group, "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        verb,
				Resource:    resource,
				Subresource: subresource,
			},
		},
	}
	if err := k8sClient.Create(ctx, sar); err != nil {
		t.Fatalf("SubjectAccessReview: %v", err)
	}
	return sar.Status.Allowed
}

// Developers get read access + port-forward in preview namespaces only,
// through a per-namespace RoleBinding — never Secrets, never elsewhere.
func TestViewerGroupsGetNamespaceScopedReadAccess(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	createDeployerClusterRole(t, ctx)
	createClusterRoleFromFile(t, ctx, "viewer_role.yaml", DefaultViewerClusterRole)

	pe := newTestPE(t, ctx, "pr-106-viewer", 106, "viewer")
	key := client.ObjectKeyFromObject(pe)
	r := newTestReconciler(t)
	r.ViewerGroups = []string{"devs"}
	_ = reconcileOnce(ctx, r, key) // namespace + access (chart fetch fails)
	ns := "preview-pr-106-viewer"

	rb := &rbacv1.RoleBinding{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: viewerRoleBindingName}, rb); err != nil {
		t.Fatalf("viewer RoleBinding not created: %v", err)
	}
	if rb.RoleRef.Name != DefaultViewerClusterRole || len(rb.Subjects) != 1 ||
		rb.Subjects[0].Kind != rbacv1.GroupKind || rb.Subjects[0].Name != "devs" {
		t.Errorf("viewer RoleBinding = %+v / %+v, want group devs → %s", rb.RoleRef, rb.Subjects, DefaultViewerClusterRole)
	}

	checks := []struct {
		verb, resource, subresource, namespace string
		want                                   bool
	}{
		{"list", "pods", "", ns, true},
		{"get", "pods", "log", ns, true},
		{"create", "pods", "portforward", ns, true},
		{"get", "secrets", "", ns, false},
		{"create", "pods", "exec", ns, false},
		{"delete", "pods", "", ns, false},
		{"list", "pods", "", "default", false},
	}
	for _, c := range checks {
		if got := can(t, ctx, "devs", c.verb, c.resource, c.subresource, c.namespace); got != c.want {
			t.Errorf("devs can %s %s/%s in %s = %v, want %v", c.verb, c.resource, c.subresource, c.namespace, got, c.want)
		}
	}

	// Removing the groups removes the binding on the next reconcile.
	r.ViewerGroups = nil
	_ = reconcileOnce(ctx, r, key)
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: viewerRoleBindingName}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("viewer RoleBinding still present without viewer groups (err = %v)", err)
	}
}

func chartWithTemplate(name, tpl string) *chart.Chart {
	return &chart.Chart{
		Metadata:  &chart.Metadata{APIVersion: chart.APIVersionV2, Name: name, Version: "0.1.0"},
		Templates: []*chart.File{{Name: "templates/resource.yaml", Data: []byte(tpl)}},
	}
}

// The chart is controlled by the PR author: Helm must run with rights limited
// to the preview namespace, not with the operator's cluster-wide identity.
func TestHelmRunsWithNamespaceScopedIdentity(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	createDeployerClusterRole(t, ctx)

	pe := newTestPE(t, ctx, "pr-105-rbac", 105, "rbac")
	r := newTestReconciler(t)
	_ = reconcileOnce(ctx, r, client.ObjectKeyFromObject(pe)) // namespace + identity (chart fetch fails)
	ns := "preview-pr-105-rbac"

	sa := &corev1.ServiceAccount{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: DeployerServiceAccount}, sa); err != nil {
		t.Fatalf("deployer ServiceAccount not created: %v", err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("deployer ServiceAccount must not automount a token")
	}
	rb := &rbacv1.RoleBinding{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: deployerRoleBindingName}, rb); err != nil {
		t.Fatalf("deployer RoleBinding not created: %v", err)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != DefaultDeployerClusterRole {
		t.Errorf("RoleBinding roleRef = %+v, want ClusterRole %s", rb.RoleRef, DefaultDeployerClusterRole)
	}

	cfg, err := r.newActionConfig(ns, func(string, ...interface{}) {})
	if err != nil {
		t.Fatalf("newActionConfig: %v", err)
	}
	values := map[string]interface{}{}

	t.Run("deploys into its own namespace", func(t *testing.T) {
		ch := chartWithTemplate("own", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: own\n")
		if _, err := installOrUpgrade(cfg, "own", ns, ch, values); err != nil {
			t.Fatalf("install in own namespace failed: %v", err)
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "own"}, &corev1.ConfigMap{}); err != nil {
			t.Errorf("ConfigMap not created in preview namespace: %v", err)
		}
	})

	t.Run("cannot deploy into another namespace", func(t *testing.T) {
		ch := chartWithTemplate("escape",
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: escape\n  namespace: default\n")
		if _, err := installOrUpgrade(cfg, "escape", ns, ch, values); err == nil {
			t.Fatal("chart deployed a resource into another namespace")
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "escape"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Errorf("ConfigMap escaped into namespace default (err = %v)", err)
		}
	})

	t.Run("cannot read secrets of another namespace via lookup", func(t *testing.T) {
		ch := chartWithTemplate("lookup",
			`{{- $s := lookup "v1" "Secret" "kube-system" "any" }}`+"\n"+
				"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: lookup\n")
		if _, err := installOrUpgrade(cfg, "lookup", ns, ch, values); err == nil {
			t.Fatal("chart was allowed to look up a Secret in kube-system")
		}
	})
}
