package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// createDeployerClusterRole installs the real deployer ClusterRole from
// config/deployer, under the prefixed name the operator binds.
func createDeployerClusterRole(t *testing.T, ctx context.Context) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "deployer", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, role); err != nil {
		t.Fatal(err)
	}
	role.Name = DefaultDeployerClusterRole
	if err := k8sClient.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating deployer ClusterRole: %v", err)
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
