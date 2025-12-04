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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// FitProfileSpec defines the desired state of FitProfile.
type FitProfileSpec struct {
	// Strategy is the recommendation strategy this profile targets.
	// Must match a registered strategy name (e.g., "percentile", "dsp").
	// +kubebuilder:validation:Required
	Strategy string `json:"strategy"`

	// DisplayName is a human-readable name for this profile.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Description provides a description of this profile's optimization approach.
	// +optional
	Description string `json:"description,omitempty"`

	// Parameters contains strategy-specific configuration parameters.
	// The schema depends on the strategy:
	// - For "percentile": requestsBufferPercent, limitsBufferPercent, maxChangePercent
	// - For "dsp": method, marginFraction, lowAmplitudeThreshold, etc.
	// +optional
	Parameters *runtime.RawExtension `json:"parameters,omitempty"`

	// SafetyRailsOverride allows overriding global safety rails for this profile.
	// If not set, uses the Atelier's global safety rails.
	// +optional
	SafetyRailsOverride *SafetyRailsConfig `json:"safetyRailsOverride,omitempty"`

	// Priority determines the order when multiple profiles match.
	// Higher priority profiles are preferred. Default is 0.
	// +optional
	// +kubebuilder:default=0
	Priority int32 `json:"priority,omitempty"`

	// Labels are key-value pairs for categorizing profiles.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// FitProfileStatus defines the observed state of FitProfile.
type FitProfileStatus struct {
	// Conditions represent the current state of the FitProfile.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// StrategyValid indicates if the referenced strategy exists and is valid.
	// +optional
	StrategyValid bool `json:"strategyValid,omitempty"`

	// ParametersValid indicates if the parameters are valid for the strategy.
	// +optional
	ParametersValid bool `json:"parametersValid,omitempty"`

	// ValidationMessage contains details about validation errors.
	// +optional
	ValidationMessage string `json:"validationMessage,omitempty"`

	// LastValidated is when the profile was last validated.
	// +optional
	LastValidated *metav1.Time `json:"lastValidated,omitempty"`

	// UsageCount is the number of Tailorings using this profile.
	// +optional
	UsageCount int32 `json:"usageCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Strategy",type="string",JSONPath=".spec.strategy"
// +kubebuilder:printcolumn:name="Display Name",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Valid",type="boolean",JSONPath=".status.parametersValid"
// +kubebuilder:printcolumn:name="Usage",type="integer",JSONPath=".status.usageCount"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// FitProfile is the Schema for the fitprofiles API.
// It defines a reusable optimization profile that can be referenced by Tailorings.
type FitProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state of FitProfile.
	// +kubebuilder:validation:Required
	Spec FitProfileSpec `json:"spec"`

	// Status defines the observed state of FitProfile.
	// +optional
	Status FitProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FitProfileList contains a list of FitProfile.
type FitProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FitProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FitProfile{}, &FitProfileList{})
}
