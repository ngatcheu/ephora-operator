package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
)

// DefaultOrphanSweepInterval matches the periodic reconciliation cadence
// from DAT §4.5 ("toutes les 30 min").
const DefaultOrphanSweepInterval = 30 * time.Minute

// OrphanSweeper periodically lists namespaces labeled as managed by this
// operator and deletes any that no longer have a corresponding
// PreviewEnvironment CR — the safety net DAT §4.5 / §7 call for when a
// finalizer gets stuck (Helm uninstall failing) or is force-released after
// its cleanup timeout (see DefaultCleanupTimeout).
//
// It also recomputes preview_environments_active from scratch on every tick
// as a correction pass, since the reconciler's own incremental updates to
// that gauge (adjustActiveGauge) reset to zero on manager restart.
type OrphanSweeper struct {
	client.Client
	Interval time.Duration
}

// NeedLeaderElection ensures only the leader replica runs the sweep, so a
// multi-replica deployment doesn't race itself deleting namespaces.
func (s *OrphanSweeper) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable.
func (s *OrphanSweeper) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultOrphanSweepInterval
	}
	logger := log.FromContext(ctx).WithName("orphan-sweeper")

	// Sweep once right away: catches orphans left while the operator was down
	// and restores an accurate preview_environments_active after a restart,
	// instead of waiting a full interval.
	if err := s.sweep(ctx); err != nil {
		logger.Error(err, "orphan sweep failed")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.sweep(ctx); err != nil {
				logger.Error(err, "orphan sweep failed")
			}
		}
	}
}

func (s *OrphanSweeper) sweep(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("orphan-sweeper")

	var envs ephoraiov1alpha1.PreviewEnvironmentList
	if err := s.List(ctx, &envs); err != nil {
		return fmt.Errorf("listing PreviewEnvironments: %w", err)
	}

	owned := make(map[string]bool, len(envs.Items))
	active := 0
	for _, pe := range envs.Items {
		owned[pe.Namespace+"/"+pe.Name] = true
		if pe.Status.Phase == ephoraiov1alpha1.PhaseRunning {
			active++
		}
	}
	ActiveEnvironments.Set(float64(active))

	var namespaces corev1.NamespaceList
	if err := s.List(ctx, &namespaces, client.MatchingLabels{LabelManagedBy: ManagedByValue}); err != nil {
		return fmt.Errorf("listing managed namespaces: %w", err)
	}

	for i := range namespaces.Items {
		ns := &namespaces.Items[i]
		if ns.DeletionTimestamp != nil {
			continue
		}
		ownerKey := ns.Labels[LabelOwnerNamespace] + "/" + ns.Labels[LabelOwner]
		if owned[ownerKey] {
			continue
		}
		logger.Info("deleting orphaned preview namespace", "namespace", ns.Name)
		if err := s.Delete(ctx, ns); err != nil {
			logger.Error(err, "failed to delete orphaned namespace", "namespace", ns.Name)
			continue
		}
		CleanupTotal.WithLabelValues(CleanupReasonOrphanSweep).Inc()
	}
	return nil
}
