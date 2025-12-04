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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Intent defines the optimization intent for a workload.
// +kubebuilder:validation:Enum=Eco;Balanced;Critical
type Intent string

const (
	// IntentEco optimizes aggressively for cost savings with tighter resource bounds.
	IntentEco Intent = "Eco"
	// IntentBalanced provides a balance between cost savings and headroom.
	IntentBalanced Intent = "Balanced"
	// IntentCritical adds extra headroom for critical workloads (e.g., +40% buffer).
	IntentCritical Intent = "Critical"
)

// WriteBackType defines the type of GitOps write-back strategy.
// +kubebuilder:validation:Enum=raw;helm;kustomize
type WriteBackType string

const (
	// WriteBackTypeRaw writes directly to Kubernetes manifest files.
	WriteBackTypeRaw WriteBackType = "raw"
	// WriteBackTypeHelm writes to Helm values.yaml files.
	WriteBackTypeHelm WriteBackType = "helm"
	// WriteBackTypeKustomize writes to Kustomize patches.
	WriteBackTypeKustomize WriteBackType = "kustomize"
)

// TargetRef identifies the workload to optimize.
type TargetRef struct {
	// APIVersion is the API version of the target resource.
	// +kubebuilder:default="apps/v1"
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind is the kind of the target resource (Deployment, StatefulSet, etc.).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet
	Kind string `json:"kind"`

	// Name is the name of the target resource.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// WriteBackConfig defines how to write recommendations back to Git.
type WriteBackConfig struct {
	// Repository is the Git repository URL (e.g., "github.com/org/repo").
	// +kubebuilder:validation:Required
	Repository string `json:"repository"`

	// Branch is the target branch for PRs. Defaults to Atelier's prSettings.baseBranch.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Path is the path to the file to modify within the repository.
	// +kubebuilder:validation:Required
	Path string `json:"path"`

	// Type is the write-back strategy type (raw, helm, kustomize).
	// +kubebuilder:validation:Required
	Type WriteBackType `json:"type"`

	// HelmValuePath is the path within values.yaml to the resource settings.
	// Only used when Type is "helm". Example: "resources" or "deployment.resources".
	// +optional
	HelmValuePath string `json:"helmValuePath,omitempty"`
}

// ResourceRequirements defines CPU and memory requirements.
type ResourceRequirements struct {
	// Requests describes the minimum amount of compute resources required.
	// +optional
	Requests *ResourceValues `json:"requests,omitempty"`

	// Limits describes the maximum amount of compute resources allowed.
	// +optional
	Limits *ResourceValues `json:"limits,omitempty"`
}

// ResourceValues defines CPU and memory values.
type ResourceValues struct {
	// CPU is the CPU resource value.
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`

	// Memory is the memory resource value.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// ContainerRecommendation holds recommendations for a specific container.
type ContainerRecommendation struct {
	// Name is the name of the container.
	Name string `json:"name"`

	// Current is the current resource configuration.
	Current ResourceRequirements `json:"current"`

	// Recommended is the recommended resource configuration.
	Recommended ResourceRequirements `json:"recommended"`

	// P95CPU is the observed P95 CPU usage.
	// +optional
	P95CPU *resource.Quantity `json:"p95CPU,omitempty"`

	// P99CPU is the observed P99 CPU usage.
	// +optional
	P99CPU *resource.Quantity `json:"p99CPU,omitempty"`

	// P95Memory is the observed P95 memory usage.
	// +optional
	P95Memory *resource.Quantity `json:"p95Memory,omitempty"`

	// P99Memory is the observed P99 memory usage.
	// +optional
	P99Memory *resource.Quantity `json:"p99Memory,omitempty"`
}

// AnalysisResult contains the results of a resource analysis.
type AnalysisResult struct {
	// Timestamp is when the analysis was performed.
	Timestamp metav1.Time `json:"timestamp"`

	// AnalysisWindow is the time window used for analysis.
	AnalysisWindow metav1.Duration `json:"analysisWindow"`

	// Containers contains per-container recommendations.
	Containers []ContainerRecommendation `json:"containers,omitempty"`

	// SavingsEstimate is the estimated monthly cost savings (if OpenCost is enabled).
	// +optional
	SavingsEstimate string `json:"savingsEstimate,omitempty"`
}

// TailoringSpec defines the desired state of Tailoring.
type TailoringSpec struct {
	// Target identifies the workload to optimize.
	// +kubebuilder:validation:Required
	Target TargetRef `json:"target"`

	// FitProfileRef references a FitProfile to use for optimization.
	// FitProfile defines the strategy and strategy-specific configuration.
	// +kubebuilder:validation:Required
	FitProfileRef FitProfileRef `json:"fitProfileRef"`

	// WriteBack defines how to write recommendations back to Git.
	// +kubebuilder:validation:Required
	WriteBack WriteBackConfig `json:"writeBack"`

	// AnalysisWindow is the time window for metrics analysis.
	// Overrides the Atelier's defaultAnalysisWindow.
	// +optional
	AnalysisWindow *metav1.Duration `json:"analysisWindow,omitempty"`

	// Paused stops Sartor from creating new Cuts for this Tailoring.
	// Recommendations are still calculated and shown in status.
	// +optional
	// +kubebuilder:default=false
	Paused bool `json:"paused,omitempty"`

	// ExcludeContainers is a list of container names to exclude from optimization.
	// +optional
	ExcludeContainers []string `json:"excludeContainers,omitempty"`
}

// FitProfileRef references a FitProfile resource.
type FitProfileRef struct {
	// Name is the name of the FitProfile to use.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the FitProfile.
	// If not specified, uses the same namespace as the Tailoring.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PRState represents the state of a Pull Request.
// +kubebuilder:validation:Enum=pending;open;merged;closed
type PRState string

const (
	// PRStatePending indicates the PR has not yet been created.
	PRStatePending PRState = "pending"
	// PRStateOpen indicates the PR is open and awaiting review/merge.
	PRStateOpen PRState = "open"
	// PRStateMerged indicates the PR has been merged.
	PRStateMerged PRState = "merged"
	// PRStateClosed indicates the PR was closed without merging.
	PRStateClosed PRState = "closed"
)

// TailoringStatus defines the observed state of Tailoring.
type TailoringStatus struct {
	// Conditions represent the current state of the Tailoring resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastAnalysis contains the results of the most recent analysis.
	// +optional
	LastAnalysis *AnalysisResult `json:"lastAnalysis,omitempty"`

	// VPADetected indicates if a VPA exists for the target workload.
	// +optional
	VPADetected bool `json:"vpaDetected,omitempty"`

	// TargetFound indicates if the target workload exists.
	// +optional
	TargetFound bool `json:"targetFound,omitempty"`

	// NextAnalysisTime is when the next analysis is scheduled.
	// +optional
	NextAnalysisTime *metav1.Time `json:"nextAnalysisTime,omitempty"`

	// PRState is the current state of the Pull Request.
	// +optional
	PRState PRState `json:"prState,omitempty"`

	// PRUrl is the URL of the Pull Request.
	// +optional
	PRUrl string `json:"prUrl,omitempty"`

	// PRNumber is the number of the Pull Request.
	// +optional
	PRNumber int `json:"prNumber,omitempty"`

	// CommitSHA is the SHA of the commit created for this recommendation.
	// +optional
	CommitSHA string `json:"commitSha,omitempty"`

	// BranchName is the name of the branch created for this recommendation.
	// +optional
	BranchName string `json:"branchName,omitempty"`

	// Ignored indicates the PR was closed with a sartor-ignore label.
	// When true, the Tailoring should be paused.
	// +optional
	Ignored bool `json:"ignored,omitempty"`

	// MergedAt is when the PR was merged.
	// +optional
	MergedAt *metav1.Time `json:"mergedAt,omitempty"`

	// PRMessage contains any error or status message.
	// +optional
	PRMessage string `json:"prMessage,omitempty"`

	// ArgoCDApp is the name of the ArgoCD Application managing this workload.
	// +optional
	ArgoCDApp string `json:"argoCDApp,omitempty"`

	// ArgoCDSyncTriggered indicates if ArgoCD sync was triggered after merge.
	// +optional
	ArgoCDSyncTriggered bool `json:"argoCDSyncTriggered,omitempty"`

	// ArgoCDSyncTime is when the ArgoCD sync was triggered.
	// +optional
	ArgoCDSyncTime *metav1.Time `json:"argoCDSyncTime,omitempty"`

	// LastPRUpdateTime is when the PR was last updated.
	// +optional
	LastPRUpdateTime *metav1.Time `json:"lastPRUpdateTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.target.name"
// +kubebuilder:printcolumn:name="Kind",type="string",JSONPath=".spec.target.kind"
// +kubebuilder:printcolumn:name="FitProfile",type="string",JSONPath=".spec.fitProfileRef.name"
// +kubebuilder:printcolumn:name="Paused",type="boolean",JSONPath=".spec.paused"
// +kubebuilder:printcolumn:name="VPA",type="boolean",JSONPath=".status.vpaDetected"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Tailoring is the Schema for the tailorings API.
// It defines the optimization settings for a specific workload.
type Tailoring struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state of Tailoring.
	// +kubebuilder:validation:Required
	Spec TailoringSpec `json:"spec"`

	// Status defines the observed state of Tailoring.
	// +optional
	Status TailoringStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TailoringList contains a list of Tailoring.
type TailoringList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tailoring `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tailoring{}, &TailoringList{})
}
