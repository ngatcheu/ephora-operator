package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// PreviewCleanupFinalizer guarantees namespace + Helm release teardown
	// before the PreviewEnvironment CRD is actually removed from the API.
	// See CLAUDE.md and DAT §4.4 — never remove or bypass this finalizer,
	// even temporarily for debugging; strip it manually via `kubectl patch`
	// on the specific object instead if a cleanup is genuinely stuck.
	PreviewCleanupFinalizer = "ephora.io/preview-cleanup"

	// MaxTTLHours is the schema- and controller-enforced upper bound on
	// spec.ttl (DAT §3.1, §7): prevents an unbounded/misconfigured TTL from
	// causing runaway cluster cost. Keep the Go constant and the CEL rule
	// on PreviewEnvironmentSpec.TTL in sync if this ever changes.
	MaxTTLHours = 168 // 7 days

	// ConditionTypeReady is the condition type reported once the Helm
	// release backing this environment is deployed and healthy.
	ConditionTypeReady = "Ready"
)

// EnvironmentPhase is the coarse-grained lifecycle phase of a PreviewEnvironment.
// +kubebuilder:validation:Enum=Pending;Provisioning;Running;Expiring;Terminating;Failed
type EnvironmentPhase string

const (
	PhasePending      EnvironmentPhase = "Pending"
	PhaseProvisioning EnvironmentPhase = "Provisioning"
	PhaseRunning      EnvironmentPhase = "Running"
	PhaseExpiring     EnvironmentPhase = "Expiring"
	PhaseTerminating  EnvironmentPhase = "Terminating"
	PhaseFailed       EnvironmentPhase = "Failed"
)

// ChartSource identifies the Helm chart to deploy for this preview environment.
// The chart lives in the application team's own repository (ADR-01) — the
// operator resolves and fetches it dynamically per PreviewEnvironment, it does
// not bundle an application chart itself.
type ChartSource struct {
	// Repo is the Git repository URL hosting the application's Helm chart.
	// Restricted to allow-listed internal Git hosts (DAT §3.1, §5) so a CRD
	// can never point the operator at an unvetted external repository.
	//
	// NOTE: adjust this pattern to your organization's real internal Git
	// host(s) before going to production — the default only matches the
	// placeholder hosts used in the DAT/README examples.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://github\.internal/.+$`
	Repo string `json:"repo"`

	// ChartPath is the path to the chart within the repository.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ChartPath string `json:"chartPath"`

	// Revision is the Git revision (commit SHA, branch, or tag) to check out
	// before deploying the chart. Changing this on an existing object
	// triggers a `helm upgrade` (DAT §4.3). Must not start with "-" so it can
	// never be parsed as a git option (argument injection).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._/-]*$`
	// +kubebuilder:validation:MaxLength=255
	Revision string `json:"revision"`
}

// PreviewEnvironmentSpec defines the desired state of a PreviewEnvironment.
type PreviewEnvironmentSpec struct {
	// Source identifies the Git repository, chart path and revision to deploy.
	// +kubebuilder:validation:Required
	Source ChartSource `json:"source"`

	// PRNumber is the Pull Request number this environment belongs to. It is
	// immutable after creation because it (with AppName) derives the
	// namespace name — changing it would orphan the original namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="prNumber is immutable"
	PRNumber int32 `json:"prNumber"`

	// AppName is the application name, combined with PRNumber to derive the
	// namespace `preview-pr-<prNumber>-<appName>` (lowercase, RFC 1123 label).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	AppName string `json:"appName"`

	// ValuesOverride is a Helm values override applied on top of the chart's
	// own defaults. Must never contain production secrets or credentials
	// (CLAUDE.md, DAT §5) — preview environments only ever get mock/test
	// values here.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	ValuesOverride *runtime.RawExtension `json:"valuesOverride,omitempty"`

	// TTL is the time-to-live before automatic teardown, expressed as a
	// duration in whole hours (e.g. "1h", "48h"). Bounded to 168h (7 days)
	// at the schema level (DAT §3.1, §7) — never remove this bound to
	// "simplify" testing; lower it locally with a shorter literal instead.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]{1,4}h$`
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('168h')",message="ttl must not exceed 168h (7 days)"
	TTL string `json:"ttl"`
}

// PreviewEnvironmentStatus defines the observed state of a PreviewEnvironment.
type PreviewEnvironmentStatus struct {
	// Phase is the current coarse-grained lifecycle phase.
	// +optional
	Phase EnvironmentPhase `json:"phase,omitempty"`

	// Namespace is the dedicated namespace created for this environment
	// (`preview-pr-<prNumber>-<appName>`, ADR-04).
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// HelmReleaseName is the name of the Helm release deployed into Namespace.
	// +optional
	HelmReleaseName string `json:"helmReleaseName,omitempty"`

	// ObservedRevision is the last spec.source.revision successfully
	// reconciled (installed or upgraded).
	// +optional
	ObservedRevision string `json:"observedRevision,omitempty"`

	// ObservedGeneration is the metadata.generation of the spec last
	// successfully deployed; a mismatch (any spec change, e.g.
	// valuesOverride) triggers a `helm upgrade`.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ExpiresAt is the computed expiry time: creationTimestamp + spec.ttl,
	// set once and never recomputed so editing spec.ttl after creation does
	// not retroactively extend an environment already close to expiry.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// CleanupStartedAt records when finalizer-driven teardown began, used to
	// enforce the cleanup timeout mitigation from DAT §7 (finalizer stuck on
	// a failed Helm uninstall).
	// +optional
	CleanupStartedAt *metav1.Time `json:"cleanupStartedAt,omitempty"`

	// Conditions represent the latest available observations of this
	// environment's state, keyed by Type (e.g. "Ready").
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=penv
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="PR",type=integer,JSONPath=".spec.prNumber"
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=".status.namespace"
// type=string, not date: kubectl renders future dates as "<invalid>" (it only
// formats them as an age in the past).
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=".status.expiresAt"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// The object name is used as the Helm release name, which Helm caps at 53 chars.
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 53",message="metadata.name must be at most 53 characters (used as the Helm release name)"

// PreviewEnvironment is the Schema for the previewenvironments API.
type PreviewEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PreviewEnvironmentSpec   `json:"spec,omitempty"`
	Status PreviewEnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PreviewEnvironmentList contains a list of PreviewEnvironment.
type PreviewEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreviewEnvironment `json:"items"`
}
