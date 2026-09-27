package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
)

const (
	// LabelManagedBy marks every namespace (and, for good measure, the
	// NetworkPolicy/ResourceQuota/LimitRange inside it) created by this
	// operator, so the orphan sweeper (DAT §4.5) can find them without
	// depending on naming conventions alone.
	LabelManagedBy = "ephora.io/managed-by"
	// LabelOwner records the owning PreviewEnvironment's name.
	LabelOwner = "ephora.io/owner"
	// LabelOwnerNamespace records the owning PreviewEnvironment's namespace.
	LabelOwnerNamespace = "ephora.io/owner-namespace"
	// ManagedByValue is the value used for LabelManagedBy.
	ManagedByValue = "ephora-operator"

	networkPolicyName = "ephora-default"
	resourceQuotaName = "ephora-default"
	limitRangeName    = "ephora-default"

	// DeployerServiceAccount is the identity Helm runs as (impersonation) in
	// each preview namespace. The chart comes from the PR author, so it must
	// never run with the operator's own cluster-wide rights: this account can
	// only act inside its namespace (RoleBinding below), which blocks a chart
	// from deploying elsewhere (metadata.namespace) or reading other
	// namespaces' Secrets (Helm `lookup`).
	DeployerServiceAccount  = "ephora-deployer"
	deployerRoleBindingName = "ephora-deployer"

	// DefaultDeployerClusterRole is the ClusterRole (config/deployer, name
	// prefixed by kustomize) bound to DeployerServiceAccount in each preview
	// namespace. Keep in sync with the `bind` RBAC marker in
	// previewenvironment_controller.go.
	DefaultDeployerClusterRole = "ephora-operator-preview-deployer"

	// DefaultViewerClusterRole (config/deployer/viewer_role.yaml, prefixed)
	// gives developers read access + port-forward in a preview namespace,
	// bound per namespace to the --viewer-groups. Never includes Secrets.
	// Keep in sync with the `bind` RBAC marker in the controller.
	DefaultViewerClusterRole = "ephora-operator-preview-viewer"
	viewerRoleBindingName    = "ephora-viewer"
)

// namespaceName derives the dedicated namespace for a PreviewEnvironment per
// ADR-04 / CLAUDE.md conventions: preview-pr-<prNumber>-<appName>, lowercase.
// AppName is already schema-validated as a lowercase RFC 1123 label, so no
// further normalization happens here — never hardcode this pattern anywhere
// else, always call this helper.
func namespaceName(pe *ephoraiov1alpha1.PreviewEnvironment) string {
	return fmt.Sprintf("preview-pr-%d-%s", pe.Spec.PRNumber, pe.Spec.AppName)
}

// releaseName derives the Helm release name for a PreviewEnvironment. Using
// the CR name keeps it stable across reconciles/upgrades.
func releaseName(pe *ephoraiov1alpha1.PreviewEnvironment) string {
	return pe.Name
}

// reconcileNamespace ensures the dedicated namespace for pe exists, labeled
// for ownership/orphan-detection. Idempotent.
func (r *PreviewEnvironmentReconciler) reconcileNamespace(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, name string) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: ownerLabels(pe),
			},
		}
		return r.Create(ctx, ns)
	}
	if err != nil {
		return fmt.Errorf("getting namespace %s: %w", name, err)
	}

	// Namespace already exists: make sure it's actually ours before touching
	// it further (avoid colliding with a namespace created out-of-band).
	if ns.Labels[LabelManagedBy] != ManagedByValue {
		return fmt.Errorf("namespace %s already exists and is not managed by ephora-operator", name)
	}
	// Two PreviewEnvironments with the same prNumber/appName derive the same
	// namespace: only the first one owns it, the other must fail rather than
	// share (and later tear down) an environment that isn't its own.
	if !ownsNamespace(pe, ns) {
		return fmt.Errorf("namespace %s already belongs to PreviewEnvironment %s/%s",
			name, ns.Labels[LabelOwnerNamespace], ns.Labels[LabelOwner])
	}
	return nil
}

// ownsNamespace reports whether ns was created for pe (owner labels match).
func ownsNamespace(pe *ephoraiov1alpha1.PreviewEnvironment, ns *corev1.Namespace) bool {
	return ns.Labels[LabelManagedBy] == ManagedByValue &&
		ns.Labels[LabelOwner] == pe.Name &&
		ns.Labels[LabelOwnerNamespace] == pe.Namespace
}

// reconcileNetworkPolicy ensures a default-deny NetworkPolicy exists in the
// given namespace, per CLAUDE.md / DAT §5 ("chaque namespace créé pour une
// preview environment doit obtenir un default-deny NetworkPolicy").
//
// The default here denies all egress except DNS and intra-namespace traffic,
// and allows ingress only from within the cluster (no path from outside is
// exposed anyway per ADR-03 — no Ingress/LoadBalancer is ever created for
// these namespaces). Tune the ingress `From` selector to your actual internal
// access path (e.g. a specific VPN-gateway or ingress-controller namespace)
// rather than leaving it open to every in-cluster namespace in production.
func (r *PreviewEnvironmentReconciler) reconcileNetworkPolicy(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, namespace string) error {
	protoTCP := corev1.ProtocolTCP
	protoUDP := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)

	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName,
			Namespace: namespace,
			Labels:    ownerLabels(pe),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // all pods in the namespace
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Same-namespace traffic (app <-> app) is always allowed.
					From: []networkingv1.NetworkPolicyPeer{
						{PodSelector: &metav1.LabelSelector{}},
					},
				},
				{
					// Internal cluster access for review (ADR-03): any
					// in-cluster namespace may reach this environment.
					// Scope this down to your real internal gateway/VPN
					// namespace before relying on it for isolation.
					From: []networkingv1.NetworkPolicyPeer{
						{NamespaceSelector: &metav1.LabelSelector{}},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// DNS resolution — without this nothing in the
					// namespace can resolve any hostname.
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &protoUDP, Port: &dnsPort},
						{Protocol: &protoTCP, Port: &dnsPort},
					},
				},
				{
					// Same-namespace and other in-cluster destinations
					// (e.g. shared internal databases). No egress to the
					// public internet by default.
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: &metav1.LabelSelector{}},
						{NamespaceSelector: &metav1.LabelSelector{}},
					},
				},
			},
		},
	}

	return r.upsertNetworkPolicy(ctx, desired)
}

func (r *PreviewEnvironmentReconciler) upsertNetworkPolicy(ctx context.Context, desired *networkingv1.NetworkPolicy) error {
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting network policy %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

// reconcileResourceQuota ensures a default ResourceQuota and LimitRange exist
// in the namespace, per CLAUDE.md / DAT §5 ("ResourceQuota/LimitRange"),
// bounding the blast radius of a misbehaving application chart (DAT §7).
//
// These defaults are deliberately conservative placeholders — tune them to
// your actual preview cluster capacity and the NFR of 50 concurrent
// environments (DAT §6).
func (r *PreviewEnvironmentReconciler) reconcileResourceQuota(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, namespace string) error {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceQuotaName,
			Namespace: namespace,
			Labels:    ownerLabels(pe),
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU:    resource.MustParse("2"),
				corev1.ResourceRequestsMemory: resource.MustParse("4Gi"),
				corev1.ResourceLimitsCPU:      resource.MustParse("4"),
				corev1.ResourceLimitsMemory:   resource.MustParse("8Gi"),
				corev1.ResourcePods:           resource.MustParse("20"),
			},
		},
	}
	if err := r.upsertResourceQuota(ctx, quota); err != nil {
		return err
	}

	defaultCPURequest := resource.MustParse("100m")
	defaultMemRequest := resource.MustParse("128Mi")
	defaultCPULimit := resource.MustParse("500m")
	defaultMemLimit := resource.MustParse("512Mi")

	limitRange := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      limitRangeName,
			Namespace: namespace,
			Labels:    ownerLabels(pe),
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Default: corev1.ResourceList{
						corev1.ResourceCPU:    defaultCPULimit,
						corev1.ResourceMemory: defaultMemLimit,
					},
					DefaultRequest: corev1.ResourceList{
						corev1.ResourceCPU:    defaultCPURequest,
						corev1.ResourceMemory: defaultMemRequest,
					},
				},
			},
		},
	}
	return r.upsertLimitRange(ctx, limitRange)
}

func (r *PreviewEnvironmentReconciler) upsertResourceQuota(ctx context.Context, desired *corev1.ResourceQuota) error {
	existing := &corev1.ResourceQuota{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting resource quota %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *PreviewEnvironmentReconciler) upsertLimitRange(ctx context.Context, desired *corev1.LimitRange) error {
	existing := &corev1.LimitRange{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting limit range %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

// reconcileDeployerIdentity ensures the namespace-scoped identity Helm
// impersonates: a ServiceAccount plus a RoleBinding granting it the deployer
// ClusterRole in this namespace only. Idempotent.
func (r *PreviewEnvironmentReconciler) reconcileDeployerIdentity(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, namespace string) error {
	noToken := false
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeployerServiceAccount,
			Namespace: namespace,
			Labels:    ownerLabels(pe),
		},
		// Only ever impersonated by the operator, never mounted into pods.
		AutomountServiceAccountToken: &noToken,
	}
	if err := r.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating service account %s/%s: %w", namespace, DeployerServiceAccount, err)
	}

	desired := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployerRoleBindingName,
			Namespace: namespace,
			Labels:    ownerLabels(pe),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     r.deployerClusterRole(),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      DeployerServiceAccount,
			Namespace: namespace,
		}},
	}

	return r.upsertRoleBinding(ctx, desired)
}

// reconcileViewerAccess grants ViewerGroups (typically the developers) read
// access and port-forward in this preview namespace only, through a
// RoleBinding to the viewer ClusterRole. With no ViewerGroups configured, any
// previously created binding is removed. Idempotent.
func (r *PreviewEnvironmentReconciler) reconcileViewerAccess(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, namespace string) error {
	if len(r.ViewerGroups) == 0 {
		stale := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: viewerRoleBindingName, Namespace: namespace}}
		if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting role binding %s/%s: %w", namespace, viewerRoleBindingName, err)
		}
		return nil
	}

	subjects := make([]rbacv1.Subject, 0, len(r.ViewerGroups))
	for _, group := range r.ViewerGroups {
		subjects = append(subjects, rbacv1.Subject{
			Kind:     rbacv1.GroupKind,
			APIGroup: rbacv1.GroupName,
			Name:     group,
		})
	}
	return r.upsertRoleBinding(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      viewerRoleBindingName,
			Namespace: namespace,
			Labels:    ownerLabels(pe),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     r.viewerClusterRole(),
		},
		Subjects: subjects,
	})
}

func (r *PreviewEnvironmentReconciler) upsertRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	existing := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return fmt.Errorf("getting role binding %s/%s: %w", desired.Namespace, desired.Name, err)
	case existing.RoleRef != desired.RoleRef:
		// roleRef is immutable: recreate.
		if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting stale role binding %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		return r.Create(ctx, desired)
	default:
		existing.Subjects = desired.Subjects
		existing.Labels = desired.Labels
		return r.Update(ctx, existing)
	}
}

func (r *PreviewEnvironmentReconciler) deployerClusterRole() string {
	if r.DeployerClusterRole != "" {
		return r.DeployerClusterRole
	}
	return DefaultDeployerClusterRole
}

func (r *PreviewEnvironmentReconciler) viewerClusterRole() string {
	if r.ViewerClusterRole != "" {
		return r.ViewerClusterRole
	}
	return DefaultViewerClusterRole
}

// deleteOwnedNamespace requests deletion of the environment's namespace and
// reports whether there is nothing left to wait for. Deletion is
// asynchronous (Kubernetes garbage-collects the contents), so callers poll
// until done=true — see reconcileDelete's CleanupTimeout (DAT §7).
//
// A namespace owned by another PreviewEnvironment (same prNumber/appName) is
// never touched: done=true, since there is nothing of pe's to clean up.
//
// beforeDelete runs once, right before the namespace deletion is requested —
// while the namespace's contents (incl. the deployer RoleBinding) still
// exist. It is not called again on later polls of a terminating namespace.
func (r *PreviewEnvironmentReconciler) deleteOwnedNamespace(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, name string, beforeDelete func()) (done bool, err error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("getting namespace %s: %w", name, err)
	}
	if !ownsNamespace(pe, ns) {
		return true, nil
	}
	if ns.DeletionTimestamp == nil {
		if beforeDelete != nil {
			beforeDelete()
		}
		if err := r.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("deleting namespace %s: %w", name, err)
		}
	}
	return false, nil
}

func ownerLabels(pe *ephoraiov1alpha1.PreviewEnvironment) map[string]string {
	return map[string]string{
		LabelManagedBy:      ManagedByValue,
		LabelOwner:          pe.Name,
		LabelOwnerNamespace: pe.Namespace,
	}
}
