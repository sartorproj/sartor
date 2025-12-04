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

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

const (
	// ConditionTypeStrategyValid indicates if the strategy is valid.
	ConditionTypeStrategyValid = "StrategyValid"
	// ConditionTypeParametersValid indicates if the parameters are valid.
	ConditionTypeParametersValid = "ParametersValid"
)

// FitProfileReconciler reconciles a FitProfile object.
type FitProfileReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	StrategyRegistry *strategy.StrategyRegistry
}

// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=fitprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=fitprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=fitprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings,verbs=get;list;watch

// Reconcile implements the reconciliation loop for FitProfile resources.
func (r *FitProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the FitProfile instance
	fitProfile := &autoscalingv1alpha1.FitProfile{}
	if err := r.Get(ctx, req.NamespacedName, fitProfile); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling FitProfile", "name", fitProfile.Name, "namespace", fitProfile.Namespace)

	// Validate strategy exists
	strategyValid := false
	if r.StrategyRegistry != nil {
		_, exists := r.StrategyRegistry.Get(fitProfile.Spec.Strategy)
		strategyValid = exists
	}

	fitProfile.Status.StrategyValid = strategyValid
	if strategyValid {
		r.setCondition(fitProfile, ConditionTypeStrategyValid, metav1.ConditionTrue, "StrategyFound", fmt.Sprintf("Strategy '%s' is registered", fitProfile.Spec.Strategy))
	} else {
		r.setCondition(fitProfile, ConditionTypeStrategyValid, metav1.ConditionFalse, "StrategyNotFound", fmt.Sprintf("Strategy '%s' is not registered", fitProfile.Spec.Strategy))
	}

	// Validate parameters if strategy is valid
	parametersValid := false
	validationMessage := ""
	if strategyValid && r.StrategyRegistry != nil {
		s, _ := r.StrategyRegistry.Get(fitProfile.Spec.Strategy)
		if s != nil {
			parameters := make(map[string]interface{})
			if fitProfile.Spec.Parameters != nil && fitProfile.Spec.Parameters.Raw != nil {
				if err := json.Unmarshal(fitProfile.Spec.Parameters.Raw, &parameters); err != nil {
					validationMessage = fmt.Sprintf("Failed to parse parameters: %v", err)
				} else {
					if err := s.ValidateFitProfileParameters(parameters); err != nil {
						validationMessage = err.Error()
					} else {
						parametersValid = true
					}
				}
			} else {
				// No parameters provided, which is valid
				parametersValid = true
			}
		}
	}

	fitProfile.Status.ParametersValid = parametersValid
	fitProfile.Status.ValidationMessage = validationMessage
	if parametersValid {
		r.setCondition(fitProfile, ConditionTypeParametersValid, metav1.ConditionTrue, "ParametersValid", "Parameters are valid for the strategy")
	} else {
		r.setCondition(fitProfile, ConditionTypeParametersValid, metav1.ConditionFalse, "ParametersInvalid", validationMessage)
	}

	// Count usage (number of Tailorings using this profile)
	usageCount, err := r.countUsage(ctx, fitProfile)
	if err != nil {
		logger.Error(err, "Failed to count FitProfile usage")
	} else {
		fitProfile.Status.UsageCount = usageCount
	}

	fitProfile.Status.LastValidated = &metav1.Time{Time: metav1.Now().Time}

	// Update status
	if err := r.Status().Update(ctx, fitProfile); err != nil {
		logger.Error(err, "Failed to update FitProfile status")
		return ctrl.Result{}, err
	}

	logger.Info("FitProfile reconciled",
		"strategyValid", strategyValid,
		"parametersValid", parametersValid,
		"usageCount", usageCount)

	return ctrl.Result{}, nil
}

// countUsage counts how many Tailorings reference this FitProfile.
func (r *FitProfileReconciler) countUsage(ctx context.Context, fitProfile *autoscalingv1alpha1.FitProfile) (int32, error) {
	tailoringList := &autoscalingv1alpha1.TailoringList{}
	if err := r.List(ctx, tailoringList); err != nil {
		return 0, err
	}

	count := int32(0)
	for _, t := range tailoringList.Items {
		refNamespace := t.Spec.FitProfileRef.Namespace
		if refNamespace == "" {
			refNamespace = t.Namespace
		}

		if t.Spec.FitProfileRef.Name == fitProfile.Name &&
			refNamespace == fitProfile.Namespace {
			count++
		}
	}

	return count, nil
}

// setCondition sets a condition on the FitProfile status.
func (r *FitProfileReconciler) setCondition(fitProfile *autoscalingv1alpha1.FitProfile, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	meta.SetStatusCondition(&fitProfile.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *FitProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.FitProfile{}).
		Named("fitprofile").
		Complete(r)
}
