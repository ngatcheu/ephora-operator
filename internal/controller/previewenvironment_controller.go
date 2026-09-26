package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
)

// DefaultCleanupTimeout bounds how long the finalizer waits for the
// namespace to fully terminate before force-removing itself — the
// mitigation documented in DAT §7 for "finalizer bloqué (Helm uninstall
// échoue)": timeout on the finalizer, with the periodic orphan sweep
// (orphansweeper.go) as the safety net that retries deletion afterwards.
const DefaultCleanupTimeout = 10 * time.Minute

// requeueSoon paces polling while waiting for the namespace to finish
// terminating during cleanup.
const requeueSoon = 10 * time.Second

// PreviewEnvironmentReconciler reconciles a PreviewEnvironment object.
//
// Per ADR-01, this stays a Helm-orchestration reconciler, not a general
// business-logic controller: every external side effect it performs is
// either plain namespace/NetworkPolicy/ResourceQuota bookkeeping (DAT §4.1,
// §5) or a Helm SDK install/upgrade/uninstall call (DAT §4.1, §4.3, §4.4).
// The one piece that goes beyond the plugin's zero-code nominal path — the
// dynamic per-instance chart fetch in helmchart.go — is the "passage
// partiel en Go" ADR-01 already anticipates as a risk, not a silent
// reopening of that decision.
type PreviewEnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RESTConfig drives the Helm SDK with the operator's own in-cluster
	// credentials (see restClientGetter in helmchart.go).
	RESTConfig *rest.Config

	// Recorder emits Kubernetes Events (events.k8s.io API). Optional: nil in
	// tests.
	Recorder events.EventRecorder

	// CleanupTimeout overrides DefaultCleanupTimeout when set.
	CleanupTimeout time.Duration

	// WorkDir overrides the base directory used for chart checkouts
	// (defaults to os.TempDir()); mainly useful for tests.
	WorkDir string
}

// +kubebuilder:rbac:groups=ephora.io,resources=previewenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ephora.io,resources=previewenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ephora.io,resources=previewenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas;limitranges,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *PreviewEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pe := &ephoraiov1alpha1.PreviewEnvironment{}
	if err := r.Get(ctx, req.NamespacedName, pe); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching PreviewEnvironment: %w", err)
	}

	if !pe.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, pe)
	}

	if !controllerutil.ContainsFinalizer(pe, ephoraiov1alpha1.PreviewCleanupFinalizer) {
		controllerutil.AddFinalizer(pe, ephoraiov1alpha1.PreviewCleanupFinalizer)
		if err := r.Update(ctx, pe); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Carry on in the same pass: pe now holds the updated object, and a
		// metadata-only change would not re-trigger a reconcile anyway
		// (GenerationChangedPredicate in SetupWithManager).
	}

	// TTL expiry (DAT §4.2): delete the CR itself once expired so the
	// normal finalizer-driven teardown path below handles cleanup
	// uniformly, whether triggered by TTL or by CI on PR close.
	if pe.Status.ExpiresAt != nil && time.Now().After(pe.Status.ExpiresAt.Time) {
		logger.Info("TTL expired, deleting PreviewEnvironment", "expiresAt", pe.Status.ExpiresAt.Time)
		if err := r.setPhase(ctx, pe, ephoraiov1alpha1.PhaseExpiring, metav1.ConditionFalse, "TTLExpired", "time-to-live exceeded, tearing down"); err != nil {
			logger.Error(err, "failed to record Expiring phase before delete")
		}
		if err := r.Delete(ctx, pe); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting expired PreviewEnvironment: %w", err)
		}
		return ctrl.Result{}, nil
	}

	return r.reconcileNormal(ctx, pe)
}

func (r *PreviewEnvironmentReconciler) reconcileNormal(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment) (ctrl.Result, error) {
	ns := namespaceName(pe)
	rel := releaseName(pe)

	// Computed before anything can fail, so an environment stuck in Failed
	// (e.g. chart never deployable) still expires and gets cleaned up. It is
	// persisted by the first status write below (setPhase / failAndRequeue).
	if pe.Status.ExpiresAt == nil {
		ttl, err := time.ParseDuration(pe.Spec.TTL)
		if err != nil {
			return r.failAndRequeue(ctx, pe, "InvalidTTL", fmt.Errorf("parsing spec.ttl %q: %w", pe.Spec.TTL, err))
		}
		expiresAt := metav1.NewTime(pe.CreationTimestamp.Add(ttl))
		pe.Status.ExpiresAt = &expiresAt
	}

	if err := r.reconcileNamespace(ctx, pe, ns); err != nil {
		return r.failAndRequeue(ctx, pe, "NamespaceReconcileFailed", err)
	}
	// Recorded as soon as the namespace exists (not only once Running), so
	// `kubectl get penv` shows where a Failed environment lives.
	pe.Status.Namespace = ns

	if err := r.reconcileNetworkPolicy(ctx, pe, ns); err != nil {
		return r.failAndRequeue(ctx, pe, "NetworkPolicyReconcileFailed", err)
	}
	if err := r.reconcileResourceQuota(ctx, pe, ns); err != nil {
		return r.failAndRequeue(ctx, pe, "ResourceQuotaReconcileFailed", err)
	}

	if pe.Status.Phase == "" || pe.Status.Phase == ephoraiov1alpha1.PhasePending {
		if err := r.setPhase(ctx, pe, ephoraiov1alpha1.PhaseProvisioning, metav1.ConditionFalse, "Provisioning", "namespace and guardrails ready, deploying chart"); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Skip the relatively expensive chart fetch + Helm upgrade if this exact
	// spec (metadata.generation) was already deployed. Comparing the
	// generation rather than only spec.source.revision means a
	// spec.valuesOverride change also triggers a `helm upgrade` (DAT §4.3).
	firstInstall := pe.Status.HelmReleaseName == ""
	needsDeploy := firstInstall || pe.Status.ObservedGeneration != pe.Generation

	if needsDeploy {
		if err := r.deployChart(ctx, pe, ns, rel); err != nil {
			return r.failAndRequeue(ctx, pe, "HelmDeployFailed", err)
		}
		// Creation-to-Running delay (DAT §6 NFR) — first install only, so
		// later upgrades of a long-lived environment don't skew the histogram.
		if firstInstall {
			ProvisioningDuration.Observe(time.Since(pe.CreationTimestamp.Time).Seconds())
		}
	}

	pe.Status.Namespace = ns
	pe.Status.HelmReleaseName = rel
	pe.Status.ObservedRevision = pe.Spec.Source.Revision
	pe.Status.ObservedGeneration = pe.Generation
	if err := r.setPhase(ctx, pe, ephoraiov1alpha1.PhaseRunning, metav1.ConditionTrue, "HelmReleaseDeployed", "helm release deployed and reconciled"); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Until(pe.Status.ExpiresAt.Time)}, nil
}

func (r *PreviewEnvironmentReconciler) deployChart(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, namespace, release string) error {
	logger := log.FromContext(ctx)

	ch, cleanup, err := r.resolveChart(ctx, pe.Spec.Source)
	if err != nil {
		return err
	}
	defer cleanup()

	values, err := helmValues(&pe.Spec)
	if err != nil {
		return err
	}

	cfg, err := r.newActionConfig(namespace, func(format string, v ...interface{}) {
		logger.V(1).Info(fmt.Sprintf(format, v...))
	})
	if err != nil {
		return err
	}

	_, err = installOrUpgrade(cfg, release, namespace, ch, values)
	return err
}

func (r *PreviewEnvironmentReconciler) reconcileDelete(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(pe, ephoraiov1alpha1.PreviewCleanupFinalizer) {
		return ctrl.Result{}, nil
	}

	// This block runs exactly once per deletion (guarded by CleanupStartedAt
	// being unset) — it's the only safe place to decrement the active gauge
	// for a direct CR delete (e.g. CI on PR close), since pe.Status.Phase
	// itself gets overwritten to Terminating right here and would no longer
	// read as "Running" on any later reconcile of this same deletion.
	if pe.Status.CleanupStartedAt == nil {
		if pe.Status.Phase == ephoraiov1alpha1.PhaseRunning {
			ActiveEnvironments.Dec()
		}
		now := metav1.Now()
		pe.Status.CleanupStartedAt = &now
		if pe.Status.Phase != ephoraiov1alpha1.PhaseExpiring {
			pe.Status.Phase = ephoraiov1alpha1.PhaseTerminating
		}
		if err := r.updateStatus(ctx, pe); err != nil {
			return ctrl.Result{}, err
		}
	}

	ns := pe.Status.Namespace
	if ns == "" {
		ns = namespaceName(pe)
	}

	// Best-effort Helm uninstall before deleting the namespace outright —
	// namespace deletion alone eventually reclaims every resource in it
	// regardless, but an explicit uninstall gives the chart's pre-delete
	// hooks (if any) a chance to run first.
	if pe.Status.HelmReleaseName != "" {
		cfg, err := r.newActionConfig(ns, func(format string, v ...interface{}) {
			logger.V(1).Info(fmt.Sprintf(format, v...))
		})
		if err != nil {
			logger.Error(err, "failed to init helm action config during cleanup, proceeding to namespace delete anyway")
		} else if err := uninstallRelease(cfg, pe.Status.HelmReleaseName); err != nil {
			logger.Error(err, "helm uninstall failed during cleanup, proceeding to namespace delete anyway")
		}
	}

	if err := r.deleteNamespace(ctx, ns); err != nil {
		return ctrl.Result{}, fmt.Errorf("deleting namespace %s: %w", ns, err)
	}

	gone, err := r.namespaceGone(ctx, ns)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking namespace %s: %w", ns, err)
	}

	timedOut := time.Since(pe.Status.CleanupStartedAt.Time) > r.effectiveCleanupTimeout()
	if !gone && !timedOut {
		logger.Info("waiting for namespace to terminate before releasing finalizer", "namespace", ns)
		return ctrl.Result{RequeueAfter: requeueSoon}, nil
	}
	if !gone && timedOut {
		logger.Error(fmt.Errorf("cleanup timeout exceeded"),
			"namespace still present after cleanup timeout, releasing finalizer anyway — periodic orphan sweep will retry deletion",
			"namespace", ns)
	}

	reason := CleanupReasonDeleted
	if pe.Status.Phase == ephoraiov1alpha1.PhaseExpiring {
		reason = CleanupReasonTTLExpired
	}
	CleanupTotal.WithLabelValues(reason).Inc()

	controllerutil.RemoveFinalizer(pe, ephoraiov1alpha1.PreviewCleanupFinalizer)
	if err := r.Update(ctx, pe); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *PreviewEnvironmentReconciler) effectiveCleanupTimeout() time.Duration {
	if r.CleanupTimeout > 0 {
		return r.CleanupTimeout
	}
	return DefaultCleanupTimeout
}

func (r *PreviewEnvironmentReconciler) failAndRequeue(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, reason string, cause error) (ctrl.Result, error) {
	// No log here: the returned error is already logged (with the same
	// context) by controller-runtime as "Reconciler error".
	if err := r.setPhase(ctx, pe, ephoraiov1alpha1.PhaseFailed, metav1.ConditionFalse, reason, cause.Error()); err != nil {
		log.FromContext(ctx).Error(err, "failed to record Failed phase")
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(pe, nil, corev1.EventTypeWarning, reason, "Reconcile", "%s", cause.Error())
	}
	return ctrl.Result{}, cause
}

func (r *PreviewEnvironmentReconciler) setPhase(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment, phase ephoraiov1alpha1.EnvironmentPhase, readyStatus metav1.ConditionStatus, reason, message string) error {
	previousPhase := pe.Status.Phase
	pe.Status.Phase = phase
	meta.SetStatusCondition(&pe.Status.Conditions, metav1.Condition{
		Type:    ephoraiov1alpha1.ConditionTypeReady,
		Status:  readyStatus,
		Reason:  reason,
		Message: message,
	})
	if err := r.updateStatus(ctx, pe); err != nil {
		return err
	}
	adjustActiveGauge(previousPhase, phase)
	return nil
}

// updateStatus persists pe.Status against the latest server copy, retrying
// on write conflicts (routine under concurrent reconciles/edits).
func (r *PreviewEnvironmentReconciler) updateStatus(ctx context.Context, pe *ephoraiov1alpha1.PreviewEnvironment) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &ephoraiov1alpha1.PreviewEnvironment{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(pe), latest); err != nil {
			return err
		}
		latest.Status = pe.Status
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}
		pe.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

// SetupWithManager wires this reconciler into the manager.
func (r *PreviewEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only spec changes and the start of a deletion trigger a reconcile.
	// Without this filter, every status write (e.g. recording a Failed
	// condition) re-triggers an immediate reconcile, bypassing the rate
	// limiter's exponential backoff and hot-looping on a persistent error.
	// Deletion is matched explicitly rather than relying on the API server
	// bumping metadata.generation when deletionTimestamp is set — missing it
	// would leave the cleanup finalizer (DAT §4.4) never running.
	// Time-based work (TTL, cleanup polling) uses RequeueAfter.
	return ctrl.NewControllerManagedBy(mgr).
		For(&ephoraiov1alpha1.PreviewEnvironment{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, deletionStartedPredicate()),
		)).
		Complete(r)
}

// deletionStartedPredicate matches the update that sets deletionTimestamp.
func deletionStartedPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetDeletionTimestamp() == nil && e.ObjectNew.GetDeletionTimestamp() != nil
		},
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
