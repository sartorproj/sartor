/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PrometheusConfig defines the Prometheus connection settings.
type PrometheusConfig struct {
	// URL is the Prometheus server URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// SecretRef references a Secret containing Prometheus authentication credentials.
	// The Secret should contain 'username' and 'password' keys for basic auth,
	// or 'token' key for bearer token authentication.
	// +optional
	SecretRef *corev1.SecretReference `json:"secretRef,omitempty"`

	// InsecureSkipVerify skips TLS certificate verification.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// GitProviderType defines the type of Git provider.
// +kubebuilder:validation:Enum=github;gitlab;bitbucket
type GitProviderType string

const (
	GitProviderGitHub    GitProviderType = "github"
	GitProviderGitLab    GitProviderType = "gitlab"
	GitProviderBitbucket GitProviderType = "bitbucket"
)

// GitProviderConfig defines the Git provider connection settings.
type GitProviderConfig struct {
	// Type is the Git provider type (github, gitlab, bitbucket).
	// +kubebuilder:validation:Required
	Type GitProviderType `json:"type"`

	// SecretRef references a Secret containing Git provider credentials.
	// The Secret should contain a 'token' key with a personal access token.
	// +kubebuilder:validation:Required
	SecretRef corev1.SecretReference `json:"secretRef"`

	// BaseURL is the base URL for self-hosted Git providers.
	// Leave empty for cloud-hosted providers.
	// +optional
	BaseURL string `json:"baseURL,omitempty"`
}

// SafetyRailsConfig defines the minimum resource thresholds.
type SafetyRailsConfig struct {
	// MinCPU is the minimum CPU request that Sartor will recommend.
	// +optional
	MinCPU *resource.Quantity `json:"minCPU,omitempty"`

	// MinMemory is the minimum memory request that Sartor will recommend.
	// +optional
	MinMemory *resource.Quantity `json:"minMemory,omitempty"`

	// MaxCPU is the maximum CPU request that Sartor will recommend.
	// +optional
	MaxCPU *resource.Quantity `json:"maxCPU,omitempty"`

	// MaxMemory is the maximum memory request that Sartor will recommend.
	// +optional
	MaxMemory *resource.Quantity `json:"maxMemory,omitempty"`
}

// PRSettingsConfig defines settings for PR creation behavior.
type PRSettingsConfig struct {
	// CooldownPeriod is the minimum time between PRs for the same workload.
	// +optional
	// +kubebuilder:default="168h"
	CooldownPeriod *metav1.Duration `json:"cooldownPeriod,omitempty"`

	// MinChangePercent is the minimum percentage change required to create a PR.
	// +optional
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MinChangePercent *int32 `json:"minChangePercent,omitempty"`

	// BaseBranch is the default base branch for PRs.
	// +optional
	// +kubebuilder:default="main"
	BaseBranch string `json:"baseBranch,omitempty"`

	// BranchPrefix is the prefix for branches created by Sartor.
	// +optional
	// +kubebuilder:default="sartor/"
	BranchPrefix string `json:"branchPrefix,omitempty"`

	// Labels are labels to add to PRs created by Sartor.
	// +optional
	Labels []string `json:"labels,omitempty"`
}

// ArgoCDConfig defines ArgoCD integration settings.
type ArgoCDConfig struct {
	// Enabled enables ArgoCD integration.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// ServerURL is the ArgoCD server URL (e.g., "https://argocd.example.com").
	// If not set, Sartor will use the Kubernetes API to interact with ArgoCD.
	// +optional
	ServerURL string `json:"serverURL,omitempty"`

	// SecretRef references a Secret containing ArgoCD credentials.
	// The Secret should contain a 'token' key with an ArgoCD API token.
	// Only needed if ServerURL is set.
	// +optional
	SecretRef *corev1.SecretReference `json:"secretRef,omitempty"`

	// InsecureSkipVerify skips TLS certificate verification.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// Namespace is the namespace where ArgoCD Applications are deployed.
	// +optional
	// +kubebuilder:default="argocd"
	Namespace string `json:"namespace,omitempty"`

	// SyncOnMerge triggers an ArgoCD sync when a PR is merged.
	// +optional
	// +kubebuilder:default=true
	SyncOnMerge bool `json:"syncOnMerge,omitempty"`

	// RefreshOnPRCreate triggers an ArgoCD refresh when a PR is created.
	// This helps ArgoCD detect the new branch.
	// +optional
	// +kubebuilder:default=false
	RefreshOnPRCreate bool `json:"refreshOnPRCreate,omitempty"`

	// HardRefresh uses hard refresh instead of normal refresh.
	// Hard refresh clears the cache and performs a full resync.
	// +optional
	// +kubebuilder:default=false
	HardRefresh bool `json:"hardRefresh,omitempty"`
}

// OpenCostConfig defines OpenCost integration settings.
type OpenCostConfig struct {
	// Enabled enables OpenCost integration.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// URL is the OpenCost server URL (e.g., "http://opencost.opencost:9003").
	// +optional
	URL string `json:"url,omitempty"`

	// InsecureSkipVerify skips TLS certificate verification.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// AtelierSpec defines the desired state of Atelier.
// Atelier is the global configuration for Sartor, defining how to connect
// to Prometheus, Git providers, and setting safety rails for recommendations.
type AtelierSpec struct {
	// Prometheus defines the Prometheus server connection settings.
	// +kubebuilder:validation:Required
	Prometheus PrometheusConfig `json:"prometheus"`

	// GitProvider defines the Git provider connection settings.
	// +kubebuilder:validation:Required
	GitProvider GitProviderConfig `json:"gitProvider"`

	// SafetyRails defines minimum and maximum resource thresholds.
	// +optional
	SafetyRails *SafetyRailsConfig `json:"safetyRails,omitempty"`

	// PRSettings defines settings for PR creation behavior.
	// +optional
	PRSettings *PRSettingsConfig `json:"prSettings,omitempty"`

	// ArgoCD defines ArgoCD integration settings.
	// +optional
	ArgoCD *ArgoCDConfig `json:"argocd,omitempty"`

	// OpenCost defines OpenCost integration settings for cost visibility.
	// +optional
	OpenCost *OpenCostConfig `json:"opencost,omitempty"`

	// BatchMode enables batching multiple changes to the same repository into a single PR.
	// +optional
	// +kubebuilder:default=false
	BatchMode bool `json:"batchMode,omitempty"`

	// DefaultAnalysisWindow is the default time window for metrics analysis.
	// +optional
	// +kubebuilder:default="168h"
	DefaultAnalysisWindow *metav1.Duration `json:"defaultAnalysisWindow,omitempty"`
}

// AtelierStatus defines the observed state of Atelier.
type AtelierStatus struct {
	// Conditions represent the current state of the Atelier resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PrometheusConnected indicates whether the Prometheus connection is healthy.
	// +optional
	PrometheusConnected bool `json:"prometheusConnected,omitempty"`

	// GitProviderConnected indicates whether the Git provider connection is healthy.
	// +optional
	GitProviderConnected bool `json:"gitProviderConnected,omitempty"`

	// ArgoCDConnected indicates whether the ArgoCD connection is healthy.
	// +optional
	ArgoCDConnected bool `json:"argoCDConnected,omitempty"`

	// OpenCostConnected indicates whether the OpenCost connection is healthy.
	// +optional
	OpenCostConnected bool `json:"openCostConnected,omitempty"`

	// LastPrometheusCheck is the timestamp of the last Prometheus connectivity check.
	// +optional
	LastPrometheusCheck *metav1.Time `json:"lastPrometheusCheck,omitempty"`

	// LastGitProviderCheck is the timestamp of the last Git provider connectivity check.
	// +optional
	LastGitProviderCheck *metav1.Time `json:"lastGitProviderCheck,omitempty"`

	// LastArgoCDCheck is the timestamp of the last ArgoCD connectivity check.
	// +optional
	LastArgoCDCheck *metav1.Time `json:"lastArgoCDCheck,omitempty"`

	// LastOpenCostCheck is the timestamp of the last OpenCost connectivity check.
	// +optional
	LastOpenCostCheck *metav1.Time `json:"lastOpenCostCheck,omitempty"`

	// ManagedTailorings is the count of Tailoring resources using this Atelier.
	// +optional
	ManagedTailorings int32 `json:"managedTailorings,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Prometheus",type="boolean",JSONPath=".status.prometheusConnected"
// +kubebuilder:printcolumn:name="Git",type="boolean",JSONPath=".status.gitProviderConnected"
// +kubebuilder:printcolumn:name="Tailorings",type="integer",JSONPath=".status.managedTailorings"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Atelier is the Schema for the ateliers API.
// It is cluster-scoped and defines the global configuration for Sartor.
type Atelier struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state of Atelier.
	// +kubebuilder:validation:Required
	Spec AtelierSpec `json:"spec"`

	// Status defines the observed state of Atelier.
	// +optional
	Status AtelierStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AtelierList contains a list of Atelier.
type AtelierList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Atelier `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Atelier{}, &AtelierList{})
}
