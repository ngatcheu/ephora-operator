package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
)

// Metrics preserved/extended per CLAUDE.md and DAT §6 — do not rename without
// updating any existing Grafana dashboards/alerts built on these series.
var (
	// ActiveEnvironments tracks the number of PreviewEnvironments currently
	// in the Running phase.
	ActiveEnvironments = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "preview_environments_active",
		Help: "Number of preview environments currently in the Running phase.",
	})

	// ProvisioningDuration tracks the delay between CR creation and the
	// environment reaching the Running phase (NFR target: < 3 minutes, DAT §6).
	ProvisioningDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "preview_environment_provisioning_duration_seconds",
		Help: "Duration from PreviewEnvironment creation to the Running phase, in seconds.",
		Buckets: []float64{
			5, 10, 20, 30, 45, 60, 90, 120, 180, 240, 300, 600,
		},
	})

	// CleanupTotal counts completed teardowns, partitioned by reason so
	// TTL-driven cleanup can be told apart from PR-close and orphan-sweep
	// cleanup in dashboards/alerts.
	CleanupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "preview_environment_cleanup_total",
		Help: "Total number of preview environment cleanups, partitioned by reason.",
	}, []string{"reason"})
)

// Cleanup reasons recorded against CleanupTotal.
const (
	CleanupReasonDeleted     = "deleted"      // CR deleted directly (e.g. CI on PR close)
	CleanupReasonTTLExpired  = "ttl_expired"  // TTL reached
	CleanupReasonOrphanSweep = "orphan_sweep" // periodic orphan-namespace detection (DAT §4.5)
)

func init() {
	metrics.Registry.MustRegister(ActiveEnvironments, ProvisioningDuration, CleanupTotal)
}

// adjustActiveGauge nudges ActiveEnvironments on a phase transition. It's a
// best-effort, incremental update (not authoritative on its own — a manager
// restart resets the in-process gauge to 0 while Running environments may
// already exist) — OrphanSweeper.sweep periodically recomputes and sets the
// exact count as a correction pass.
func adjustActiveGauge(previous, next ephoraiov1alpha1.EnvironmentPhase) {
	if previous == next {
		return
	}
	if next == ephoraiov1alpha1.PhaseRunning {
		ActiveEnvironments.Inc()
	} else if previous == ephoraiov1alpha1.PhaseRunning {
		ActiveEnvironments.Dec()
	}
}
