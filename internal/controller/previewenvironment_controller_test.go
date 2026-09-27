package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
)

// envtest runs no kube-controller-manager, so deleted namespaces stay
// Terminating forever. Each test therefore uses its own prNumber/appName to
// get a distinct preview namespace, and a near-zero CleanupTimeout so the
// finalizer releases instead of waiting for the namespace to disappear.

func newTestPE(t *testing.T, ctx context.Context, name string, pr int32, app string) *ephoraiov1alpha1.PreviewEnvironment {
	t.Helper()
	mgmt := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mgmt-"}}
	if err := k8sClient.Create(ctx, mgmt); err != nil {
		t.Fatalf("creating management namespace: %v", err)
	}
	pe := &ephoraiov1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mgmt.Name},
		Spec: ephoraiov1alpha1.PreviewEnvironmentSpec{
			Source: ephoraiov1alpha1.ChartSource{
				// Unreachable on purpose: GIT_ALLOW_PROTOCOL=file (suite_test.go)
				// makes the chart fetch fail fast, exercising the Failed path.
				Repo:      "https://github.internal/team/" + app,
				ChartPath: "charts/app",
				Revision:  "main",
			},
			PRNumber: pr,
			AppName:  app,
			TTL:      "48h",
		},
	}
	if err := k8sClient.Create(ctx, pe); err != nil {
		t.Fatalf("creating PreviewEnvironment: %v", err)
	}
	return pe
}

func newTestReconciler(t *testing.T) *PreviewEnvironmentReconciler {
	return &PreviewEnvironmentReconciler{
		Client:         k8sClient,
		Scheme:         k8sClient.Scheme(),
		RESTConfig:     testCfg,
		CleanupTimeout: time.Nanosecond,
		WorkDir:        t.TempDir(),
	}
}

func reconcileOnce(ctx context.Context, r *PreviewEnvironmentReconciler, key types.NamespacedName) error {
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	return err
}

func getPE(t *testing.T, ctx context.Context, key types.NamespacedName) *ephoraiov1alpha1.PreviewEnvironment {
	t.Helper()
	pe := &ephoraiov1alpha1.PreviewEnvironment{}
	if err := k8sClient.Get(ctx, key, pe); err != nil {
		t.Fatalf("getting PreviewEnvironment: %v", err)
	}
	return pe
}

func TestReconcileProvisionsGuardrailsAndReportsFailure(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	pe := newTestPE(t, ctx, "pr-101-guardrails", 101, "guardrails")
	key := client.ObjectKeyFromObject(pe)
	r := newTestReconciler(t)

	// A single pass adds the finalizer, creates the guardrails, then fails
	// on the (offline) chart fetch.
	if err := reconcileOnce(ctx, r, key); err == nil {
		t.Fatal("reconcile: expected chart fetch error, got nil")
	}

	got := getPE(t, ctx, key)
	if !controllerutil.ContainsFinalizer(got, ephoraiov1alpha1.PreviewCleanupFinalizer) {
		t.Fatal("cleanup finalizer not added")
	}
	nsName := "preview-pr-101-guardrails"
	if got.Status.Phase != ephoraiov1alpha1.PhaseFailed {
		t.Errorf("phase = %q, want %q", got.Status.Phase, ephoraiov1alpha1.PhaseFailed)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, ephoraiov1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "HelmDeployFailed" {
		t.Errorf("Ready condition = %+v, want False/HelmDeployFailed", ready)
	}
	if got.Status.Namespace != nsName {
		t.Errorf("status.namespace = %q, want %q", got.Status.Namespace, nsName)
	}
	// Set even though the deploy failed, so a broken environment still expires.
	wantExpiry := got.CreationTimestamp.Add(48 * time.Hour)
	if got.Status.ExpiresAt == nil || !got.Status.ExpiresAt.Time.Equal(wantExpiry) {
		t.Errorf("status.expiresAt = %v, want %v", got.Status.ExpiresAt, wantExpiry)
	}

	ns := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nsName}, ns); err != nil {
		t.Fatalf("preview namespace not created: %v", err)
	}
	wantLabels := map[string]string{
		LabelManagedBy:      ManagedByValue,
		LabelOwner:          pe.Name,
		LabelOwnerNamespace: pe.Namespace,
	}
	for k, v := range wantLabels {
		if ns.Labels[k] != v {
			t.Errorf("namespace label %s = %q, want %q", k, ns.Labels[k], v)
		}
	}

	np := &networkingv1.NetworkPolicy{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: networkPolicyName}, np); err != nil {
		t.Fatalf("default-deny NetworkPolicy not created: %v", err)
	}
	if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Error("NetworkPolicy must select every pod in the namespace")
	}
	if len(np.Spec.PolicyTypes) != 2 {
		t.Errorf("NetworkPolicy policyTypes = %v, want Ingress and Egress", np.Spec.PolicyTypes)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: resourceQuotaName}, &corev1.ResourceQuota{}); err != nil {
		t.Errorf("ResourceQuota not created: %v", err)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: limitRangeName}, &corev1.LimitRange{}); err != nil {
		t.Errorf("LimitRange not created: %v", err)
	}
}

func TestReconcileDeletionRunsFinalizer(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	pe := newTestPE(t, ctx, "pr-102-deletion", 102, "deletion")
	key := client.ObjectKeyFromObject(pe)
	r := newTestReconciler(t)

	_ = reconcileOnce(ctx, r, key) // finalizer + namespace + guardrails (chart fetch fails)

	before := testutil.ToFloat64(CleanupTotal.WithLabelValues(CleanupReasonDeleted))
	if err := k8sClient.Delete(ctx, getPE(t, ctx, key)); err != nil {
		t.Fatalf("deleting PreviewEnvironment: %v", err)
	}
	if err := reconcileOnce(ctx, r, key); err != nil {
		t.Fatalf("cleanup reconcile: %v", err)
	}

	if err := k8sClient.Get(ctx, key, &ephoraiov1alpha1.PreviewEnvironment{}); !apierrors.IsNotFound(err) {
		t.Errorf("PreviewEnvironment still present after cleanup (err = %v)", err)
	}
	ns := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "preview-pr-102-deletion"}, ns); err == nil && ns.DeletionTimestamp == nil {
		t.Error("preview namespace deletion was not requested")
	}
	if after := testutil.ToFloat64(CleanupTotal.WithLabelValues(CleanupReasonDeleted)); after != before+1 {
		t.Errorf("cleanup_total{reason=deleted} = %v, want %v", after, before+1)
	}
}

func TestReconcileTTLExpiryDeletesEnvironment(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	pe := newTestPE(t, ctx, "pr-103-ttl", 103, "ttl")
	key := client.ObjectKeyFromObject(pe)
	r := newTestReconciler(t)

	_ = reconcileOnce(ctx, r, key) // finalizer + guardrails (chart fetch fails)

	// Simulate an environment past its expiry.
	expired := getPE(t, ctx, key)
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	expired.Status.ExpiresAt = &past
	if err := k8sClient.Status().Update(ctx, expired); err != nil {
		t.Fatalf("setting expiresAt: %v", err)
	}

	before := testutil.ToFloat64(CleanupTotal.WithLabelValues(CleanupReasonTTLExpired))

	// Pass 1: expiry detected, phase Expiring, CR deletion requested.
	if err := reconcileOnce(ctx, r, key); err != nil {
		t.Fatalf("expiry reconcile: %v", err)
	}
	got := getPE(t, ctx, key)
	if got.DeletionTimestamp == nil {
		t.Fatal("expired PreviewEnvironment was not deleted")
	}
	if got.Status.Phase != ephoraiov1alpha1.PhaseExpiring {
		t.Errorf("phase = %q, want %q", got.Status.Phase, ephoraiov1alpha1.PhaseExpiring)
	}

	// Pass 2: finalizer-driven cleanup, recorded as a TTL cleanup.
	if err := reconcileOnce(ctx, r, key); err != nil {
		t.Fatalf("cleanup reconcile: %v", err)
	}
	if err := k8sClient.Get(ctx, key, &ephoraiov1alpha1.PreviewEnvironment{}); !apierrors.IsNotFound(err) {
		t.Errorf("PreviewEnvironment still present after TTL cleanup (err = %v)", err)
	}
	if after := testutil.ToFloat64(CleanupTotal.WithLabelValues(CleanupReasonTTLExpired)); after != before+1 {
		t.Errorf("cleanup_total{reason=ttl_expired} = %v, want %v", after, before+1)
	}
}

// Two PreviewEnvironments with the same prNumber/appName derive the same
// namespace: the second must be refused, and deleting it must not tear down
// the first one's environment.
func TestReconcileRefusesSharedNamespace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	r := newTestReconciler(t)
	nsName := "preview-pr-104-shared"

	owner := newTestPE(t, ctx, "pr-104-owner", 104, "shared")
	_ = reconcileOnce(ctx, r, client.ObjectKeyFromObject(owner)) // creates the namespace (chart fetch fails)

	intruder := newTestPE(t, ctx, "pr-104-intruder", 104, "shared")
	ikey := client.ObjectKeyFromObject(intruder)
	if err := reconcileOnce(ctx, r, ikey); err == nil {
		t.Fatal("reconcile of the second PreviewEnvironment: expected an error, got nil")
	}
	ready := meta.FindStatusCondition(getPE(t, ctx, ikey).Status.Conditions, ephoraiov1alpha1.ConditionTypeReady)
	if ready == nil || ready.Reason != "NamespaceReconcileFailed" {
		t.Errorf("Ready condition = %+v, want reason NamespaceReconcileFailed", ready)
	}

	if err := k8sClient.Delete(ctx, getPE(t, ctx, ikey)); err != nil {
		t.Fatalf("deleting the second PreviewEnvironment: %v", err)
	}
	if err := reconcileOnce(ctx, r, ikey); err != nil {
		t.Fatalf("cleanup reconcile: %v", err)
	}
	if err := k8sClient.Get(ctx, ikey, &ephoraiov1alpha1.PreviewEnvironment{}); !apierrors.IsNotFound(err) {
		t.Errorf("second PreviewEnvironment still present after cleanup (err = %v)", err)
	}

	ns := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nsName}, ns); err != nil {
		t.Fatalf("getting the owner's namespace: %v", err)
	}
	if ns.DeletionTimestamp != nil {
		t.Error("the owner's namespace was deleted when the second PreviewEnvironment was removed")
	}
}

// TestCRDValidation guards the schema-level security/cost controls from
// CLAUDE.md: TTL bound, repo allow-list, revision format, immutable prNumber,
// and the Helm release name length limit.
func TestCRDValidation(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	mgmt := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mgmt-"}}
	if err := k8sClient.Create(ctx, mgmt); err != nil {
		t.Fatal(err)
	}

	valid := func(name string) *ephoraiov1alpha1.PreviewEnvironment {
		return &ephoraiov1alpha1.PreviewEnvironment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mgmt.Name},
			Spec: ephoraiov1alpha1.PreviewEnvironmentSpec{
				Source: ephoraiov1alpha1.ChartSource{
					Repo: "https://github.internal/team/app", ChartPath: "charts/app", Revision: "main",
				},
				PRNumber: 1,
				AppName:  "app",
				TTL:      "48h",
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*ephoraiov1alpha1.PreviewEnvironment)
	}{
		{"ttl above 168h", func(pe *ephoraiov1alpha1.PreviewEnvironment) { pe.Spec.TTL = "169h" }},
		{"ttl not in hours", func(pe *ephoraiov1alpha1.PreviewEnvironment) { pe.Spec.TTL = "30m" }},
		{"repo not allow-listed", func(pe *ephoraiov1alpha1.PreviewEnvironment) { pe.Spec.Source.Repo = "https://github.com/evil/repo" }},
		{"repo not https", func(pe *ephoraiov1alpha1.PreviewEnvironment) {
			pe.Spec.Source.Repo = "git@github.internal:team/app.git"
		}},
		{"revision starting with dash", func(pe *ephoraiov1alpha1.PreviewEnvironment) { pe.Spec.Source.Revision = "--upload-pack=x" }},
		{"appName not a DNS label", func(pe *ephoraiov1alpha1.PreviewEnvironment) { pe.Spec.AppName = "Bad_Name" }},
		{"prNumber not positive", func(pe *ephoraiov1alpha1.PreviewEnvironment) { pe.Spec.PRNumber = 0 }},
		{"name longer than 53 chars", func(pe *ephoraiov1alpha1.PreviewEnvironment) { pe.Name = "pr-1-" + strings.Repeat("a", 49) }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := valid("invalid-" + string(rune('a'+i)))
			tt.mutate(pe)
			if err := k8sClient.Create(ctx, pe); !apierrors.IsInvalid(err) {
				t.Errorf("Create() error = %v, want Invalid", err)
			}
		})
	}

	t.Run("valid object accepted and prNumber immutable", func(t *testing.T) {
		pe := valid("valid")
		if err := k8sClient.Create(ctx, pe); err != nil {
			t.Fatalf("valid object rejected: %v", err)
		}
		pe.Spec.PRNumber = 2
		if err := k8sClient.Update(ctx, pe); !apierrors.IsInvalid(err) {
			t.Errorf("updating prNumber: error = %v, want Invalid", err)
		}
	})
}
