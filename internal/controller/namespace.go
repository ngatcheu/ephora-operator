package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	return nil
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

// deleteNamespace issues a delete for the environment's namespace. Deletion
// is asynchronous (Kubernetes garbage-collects the namespace's contents) —
// callers should not assume the namespace is gone the moment this returns;
// see reconcileDelete's use of the CleanupTimeout mitigation from DAT §7.
func (r *PreviewEnvironmentReconciler) deleteNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := r.Delete(ctx, ns)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// namespaceGone reports whether the namespace no longer exists (fully
// terminated and garbage-collected).
func (r *PreviewEnvironmentReconciler) namespaceGone(ctx context.Context, name string) (bool, error) {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
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
