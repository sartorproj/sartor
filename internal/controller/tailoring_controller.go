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
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

const (
	// ConditionTypeReady indicates the Tailoring is ready.
	ConditionTypeReady = "Ready"
	// ConditionTypeAnalyzed indicates analysis has been performed.
	ConditionTypeAnalyzed = "Analyzed"
	// ConditionTypeTargetFound indicates the target workload exists.
	ConditionTypeTargetFound = "TargetFound"
	// ConditionTypePRCreated indicates if the PR was created.
	ConditionTypePRCreated = "PRCreated"
	// ConditionTypePRMerged indicates if the PR was merged.
	ConditionTypePRMerged = "PRMerged"

	// SartorIgnoreLabel is the label that indicates the PR should be ignored.
	SartorIgnoreLabel = "sartor-ignore"

	// DefaultRequeueInterval is the default interval between reconciliations.
	DefaultRequeueInterval = 30 * time.Second // Reduced for testing
	// AnalysisRequeueInterval is the interval after a successful analysis.
	AnalysisRequeueInterval = 1 * time.Minute // Reduced for testing (was 1 hour)
	// PRCheckInterval is the interval to check PR status.
	PRCheckInterval = 5 * time.Minute
)

// TailoringReconciler reconciles a Tailoring object.
type TailoringReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=ateliers,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=verticalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch

// Reconcile implements the reconciliation loop for Tailoring resources.
func (r *TailoringReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Tailoring instance
	tailoring := &autoscalingv1alpha1.Tailoring{}
	if err := r.Get(ctx, req.NamespacedName, tailoring); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Tailoring", "name", tailoring.Name, "namespace", tailoring.Namespace)

	// Get the Atelier configuration
	atelier, err := r.getAtelier(ctx)
	if err != nil {
		logger.Error(err, "Failed to get Atelier")
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionFalse, "AtelierNotFound", err.Error())
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Check if target workload exists
	currentResources, err := r.getTargetResources(ctx, tailoring)
	if err != nil {
		logger.Error(err, "Failed to get target workload")
		tailoring.Status.TargetFound = false
		r.setCondition(tailoring, ConditionTypeTargetFound, metav1.ConditionFalse, "TargetNotFound", err.Error())
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}
	tailoring.Status.TargetFound = true
	r.setCondition(tailoring, ConditionTypeTargetFound, metav1.ConditionTrue, "TargetFound", "Target workload found")

	// Check for VPA
	vpaDetected, err := r.checkForVPA(ctx, tailoring)
	if err != nil {
		logger.Error(err, "Failed to check for VPA")
	}
	tailoring.Status.VPADetected = vpaDetected
	if vpaDetected {
		logger.Info("VPA detected for target workload, Sartor will coexist")
	}

	// Get Prometheus credentials if configured
	promConfig, err := r.buildPrometheusConfig(ctx, atelier)
	if err != nil {
		logger.Error(err, "Failed to build Prometheus config")
		r.setCondition(tailoring, ConditionTypeAnalyzed, metav1.ConditionFalse, "PrometheusConfigError", err.Error())
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Create Prometheus client
	promClient, err := prometheus.NewClient(promConfig)
	if err != nil {
		logger.Error(err, "Failed to create Prometheus client")
		r.setCondition(tailoring, ConditionTypeAnalyzed, metav1.ConditionFalse, "PrometheusClientError", err.Error())
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Determine analysis window
	analysisWindow := 7 * 24 * time.Hour // Default 7 days
	if tailoring.Spec.AnalysisWindow != nil {
		analysisWindow = tailoring.Spec.AnalysisWindow.Duration
	} else if atelier.Spec.DefaultAnalysisWindow != nil {
		analysisWindow = atelier.Spec.DefaultAnalysisWindow.Duration
	}

	// Query Prometheus for metrics
	metrics, err := promClient.GetContainerMetrics(ctx, tailoring.Namespace, tailoring.Spec.Target.Name, analysisWindow)
	if err != nil {
		logger.Error(err, "Failed to get container metrics")
		r.setCondition(tailoring, ConditionTypeAnalyzed, metav1.ConditionFalse, "MetricsQueryError", err.Error())
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Filter out excluded containers
	metrics = r.filterExcludedContainers(metrics, tailoring.Spec.ExcludeContainers)

	if len(metrics) == 0 {
		logger.Info("No metrics found for target workload")
		r.setCondition(tailoring, ConditionTypeAnalyzed, metav1.ConditionFalse, "NoMetrics", "No metrics found for target workload")
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Get safety rails and quota
	safetyRails := r.getSafetyRails(atelier)
	quota, err := r.getResourceQuota(ctx, tailoring.Namespace)
	if err != nil {
		logger.Error(err, "Failed to get ResourceQuota")
		// Continue without quota limits
	}

	// Resolve FitProfile to get strategy and parameters
	strategyName, strategyParams, enablePeakDetection, enableNormalization, safetyRailsOverride := r.resolveFitProfile(ctx, tailoring, atelier, logger)

	// Apply safety rails override from FitProfile if provided
	if safetyRailsOverride != nil {
		if safetyRailsOverride.MinCPU != nil {
			safetyRails.MinCPU = safetyRailsOverride.MinCPU
		}
		if safetyRailsOverride.MinMemory != nil {
			safetyRails.MinMemory = safetyRailsOverride.MinMemory
		}
		if safetyRailsOverride.MaxCPU != nil {
			safetyRails.MaxCPU = safetyRailsOverride.MaxCPU
		}
		if safetyRailsOverride.MaxMemory != nil {
			safetyRails.MaxMemory = safetyRailsOverride.MaxMemory
		}
	}

	// Calculate recommendations using strategy-based calculator
	calculator := recommender.NewCalculator()
	recommendations, err := calculator.Calculate(
		ctx,
		metrics,
		currentResources,
		autoscalingv1alpha1.IntentBalanced, // Default, FitProfile overrides this
		safetyRails,
		quota,
		analysisWindow,
		strategyName,
		strategyParams,
		enablePeakDetection,
		enableNormalization,
	)
	if err != nil {
		logger.Error(err, "Failed to calculate recommendations")
		r.setCondition(tailoring, ConditionTypeAnalyzed, metav1.ConditionFalse, "CalculationError", err.Error())
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// Convert strategy recommendations to old format for compatibility
	legacyRecommendations := convertStrategyRecommendations(recommendations)

	// Update analysis result in status (include current resources for comparison)
	analysisResult := r.buildAnalysisResult(metrics, legacyRecommendations, currentResources, analysisWindow)
	tailoring.Status.LastAnalysis = &analysisResult
	tailoring.Status.NextAnalysisTime = &metav1.Time{Time: time.Now().Add(AnalysisRequeueInterval)}
	r.setCondition(tailoring, ConditionTypeAnalyzed, metav1.ConditionTrue, "AnalysisComplete", "Resource analysis completed")

	// Check if Tailoring is paused (dry-run mode)
	if tailoring.Spec.Paused {
		logger.Info("Tailoring is paused, skipping Cut creation")
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Paused", "Tailoring is paused, recommendations calculated but no Cut created")
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{RequeueAfter: AnalysisRequeueInterval}, nil
	}

	// Check cooldown period
	if tailoring.Status.LastPRUpdateTime != nil {
		cooldown := 7 * 24 * time.Hour // Default 7 days
		if atelier.Spec.PRSettings != nil && atelier.Spec.PRSettings.CooldownPeriod != nil {
			cooldown = atelier.Spec.PRSettings.CooldownPeriod.Duration
		}
		if time.Since(tailoring.Status.LastPRUpdateTime.Time) < cooldown {
			logger.Info("Cooldown period not elapsed, skipping PR creation")
			r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "CooldownActive", "Cooldown period has not elapsed")
			if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
				logger.Error(updateErr, "Failed to update status")
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: AnalysisRequeueInterval}, nil
		}
	}

	// Check if changes exceed threshold
	minChangePercent := int32(20) // Default 20%
	if atelier.Spec.PRSettings != nil && atelier.Spec.PRSettings.MinChangePercent != nil {
		minChangePercent = *atelier.Spec.PRSettings.MinChangePercent
	}

	if !recommender.ShouldCreateCut(recommendations, currentResources, minChangePercent) {
		logger.Info("Changes do not exceed threshold, skipping PR creation")
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "BelowThreshold", "Resource changes below threshold")
		if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{RequeueAfter: AnalysisRequeueInterval}, nil
	}

	// TODO: Handle Git PR operations directly
	// For now, just mark as ready - Git operations will be implemented in a future update
	logger.Info("Recommendations calculated, Git operations not yet implemented")
	r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Ready", "Recommendations calculated")
	result := ctrl.Result{RequeueAfter: AnalysisRequeueInterval}

	if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
		logger.Error(updateErr, "Failed to update status")
		return ctrl.Result{}, updateErr
	}

	return result, nil
}

// getAtelier retrieves the Atelier configuration.
func (r *TailoringReconciler) getAtelier(ctx context.Context) (*autoscalingv1alpha1.Atelier, error) {
	atelierList := &autoscalingv1alpha1.AtelierList{}
	if err := r.List(ctx, atelierList); err != nil {
		return nil, fmt.Errorf("failed to list Ateliers: %w", err)
	}

	if len(atelierList.Items) == 0 {
		return nil, fmt.Errorf("no Atelier found")
	}

	// Use the first Atelier (single Atelier per cluster)
	return &atelierList.Items[0], nil
}

// getTargetResources retrieves the current resources from the target workload.
func (r *TailoringReconciler) getTargetResources(ctx context.Context, tailoring *autoscalingv1alpha1.Tailoring) (map[string]autoscalingv1alpha1.ResourceRequirements, error) {
	target := tailoring.Spec.Target
	resources := make(map[string]autoscalingv1alpha1.ResourceRequirements)

	var containers []corev1.Container

	switch target.Kind {
	case "Deployment":
		deployment := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: target.Name, Namespace: tailoring.Namespace}, deployment); err != nil {
			return nil, err
		}
		containers = deployment.Spec.Template.Spec.Containers

	case "StatefulSet":
		statefulSet := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: target.Name, Namespace: tailoring.Namespace}, statefulSet); err != nil {
			return nil, err
		}
		containers = statefulSet.Spec.Template.Spec.Containers

	case "DaemonSet":
		daemonSet := &appsv1.DaemonSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: target.Name, Namespace: tailoring.Namespace}, daemonSet); err != nil {
			return nil, err
		}
		containers = daemonSet.Spec.Template.Spec.Containers

	default:
		return nil, fmt.Errorf("unsupported target kind: %s", target.Kind)
	}

	for _, container := range containers {
		resources[container.Name] = autoscalingv1alpha1.ResourceRequirements{
			Requests: convertResourceValues(container.Resources.Requests),
			Limits:   convertResourceValues(container.Resources.Limits),
		}
	}

	return resources, nil
}

// convertResourceValues converts corev1.ResourceList to autoscalingv1alpha1.ResourceValues.
func convertResourceValues(rl corev1.ResourceList) *autoscalingv1alpha1.ResourceValues {
	if rl == nil {
		return nil
	}

	rv := &autoscalingv1alpha1.ResourceValues{}

	if cpu, ok := rl[corev1.ResourceCPU]; ok {
		rv.CPU = &cpu
	}
	if memory, ok := rl[corev1.ResourceMemory]; ok {
		rv.Memory = &memory
	}

	return rv
}

// checkForVPA checks if a VPA exists for the target workload.
func (r *TailoringReconciler) checkForVPA(ctx context.Context, tailoring *autoscalingv1alpha1.Tailoring) (bool, error) {
	// Try to list VPAs in the namespace
	vpaList := &autoscalingv2.HorizontalPodAutoscalerList{} // Using HPA as placeholder; real VPA would require vpav1 API
	if err := r.List(ctx, vpaList, client.InNamespace(tailoring.Namespace)); err != nil {
		// VPA CRD might not be installed, which is fine
		return false, nil
	}

	// For now, return false as we're using HPA as placeholder
	// Real implementation would check VerticalPodAutoscaler resources
	return false, nil
}

// buildPrometheusConfig builds the Prometheus client configuration.
func (r *TailoringReconciler) buildPrometheusConfig(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) (prometheus.Config, error) {
	config := prometheus.Config{
		URL:                atelier.Spec.Prometheus.URL,
		InsecureSkipVerify: atelier.Spec.Prometheus.InsecureSkipVerify,
	}

	if atelier.Spec.Prometheus.SecretRef != nil {
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      atelier.Spec.Prometheus.SecretRef.Name,
			Namespace: atelier.Spec.Prometheus.SecretRef.Namespace,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return config, fmt.Errorf("failed to get Prometheus secret: %w", err)
		}

		if username, ok := secret.Data["username"]; ok {
			config.Username = string(username)
		}
		if password, ok := secret.Data["password"]; ok {
			config.Password = string(password)
		}
		if token, ok := secret.Data["token"]; ok {
			config.Token = string(token)
		}
	}

	return config, nil
}

// filterExcludedContainers removes excluded containers from the metrics.
func (r *TailoringReconciler) filterExcludedContainers(metrics []prometheus.ContainerMetrics, excluded []string) []prometheus.ContainerMetrics {
	if len(excluded) == 0 {
		return metrics
	}

	excludeSet := make(map[string]bool)
	for _, name := range excluded {
		excludeSet[name] = true
	}

	filtered := make([]prometheus.ContainerMetrics, 0, len(metrics))
	for _, m := range metrics {
		if !excludeSet[m.ContainerName] {
			filtered = append(filtered, m)
		}
	}

	return filtered
}

// getSafetyRails extracts safety rails from the Atelier.
func (r *TailoringReconciler) getSafetyRails(atelier *autoscalingv1alpha1.Atelier) recommender.SafetyRails {
	rails := recommender.SafetyRails{}

	if atelier.Spec.SafetyRails != nil {
		rails.MinCPU = atelier.Spec.SafetyRails.MinCPU
		rails.MinMemory = atelier.Spec.SafetyRails.MinMemory
		rails.MaxCPU = atelier.Spec.SafetyRails.MaxCPU
		rails.MaxMemory = atelier.Spec.SafetyRails.MaxMemory
	}

	return rails
}

// resolveFitProfile resolves the FitProfile reference and extracts configuration.
// Returns: strategyName, strategyParams, enablePeakDetection, enableNormalization, safetyRailsOverride
func (r *TailoringReconciler) resolveFitProfile(
	ctx context.Context,
	tailoring *autoscalingv1alpha1.Tailoring,
	atelier *autoscalingv1alpha1.Atelier,
	logger logr.Logger,
) (string, map[string]interface{}, bool, bool, *autoscalingv1alpha1.SafetyRailsConfig) {
	// FitProfileRef is required
	fitProfileNamespace := tailoring.Spec.FitProfileRef.Namespace
	if fitProfileNamespace == "" {
		fitProfileNamespace = tailoring.Namespace
	}

	fitProfile := &autoscalingv1alpha1.FitProfile{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      tailoring.Spec.FitProfileRef.Name,
		Namespace: fitProfileNamespace,
	}, fitProfile); err != nil {
		logger.Error(err, "Failed to get FitProfile")
		// Return defaults - this will cause an error downstream but we'll handle it gracefully
		return "percentile", make(map[string]interface{}), false, false, nil
	}

	// Extract parameters from FitProfile
	params := make(map[string]interface{})
	if fitProfile.Spec.Parameters != nil && fitProfile.Spec.Parameters.Raw != nil {
		if err := json.Unmarshal(fitProfile.Spec.Parameters.Raw, &params); err != nil {
			logger.Error(err, "Failed to parse FitProfile parameters")
		}
	}

	// Extract enable flags from parameters (for DSP strategy)
	enablePeakDetection := false
	enableNormalization := false
	if enablePeak, ok := params["enablePeakDetection"].(bool); ok {
		enablePeakDetection = enablePeak
	}
	if enableNorm, ok := params["enableNormalization"].(bool); ok {
		enableNormalization = enableNorm
	}

	return fitProfile.Spec.Strategy, params, enablePeakDetection, enableNormalization, fitProfile.Spec.SafetyRailsOverride
}

// convertStrategyRecommendations converts strategy.Recommendation to the old format for compatibility.
func convertStrategyRecommendations(strategyRecs []strategy.Recommendation) []recommender.Recommendation {
	recs := make([]recommender.Recommendation, 0, len(strategyRecs))
	for _, sr := range strategyRecs {
		recs = append(recs, recommender.Recommendation{
			ContainerName: sr.ContainerName,
			Requests: recommender.ResourceValues{
				CPU:    sr.Requests.CPU,
				Memory: sr.Requests.Memory,
			},
			Limits: recommender.ResourceValues{
				CPU:    sr.Limits.CPU,
				Memory: sr.Limits.Memory,
			},
			Capped:       sr.Capped,
			CappedReason: sr.CappedReason,
		})
	}
	return recs
}

// getResourceQuota retrieves the ResourceQuota for a namespace.
func (r *TailoringReconciler) getResourceQuota(ctx context.Context, namespace string) (*recommender.ResourceQuota, error) {
	quotaList := &corev1.ResourceQuotaList{}
	if err := r.List(ctx, quotaList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	if len(quotaList.Items) == 0 {
		return nil, nil
	}

	// Use the first quota's limits
	quota := &recommender.ResourceQuota{}
	spec := quotaList.Items[0].Spec

	if cpu, ok := spec.Hard[corev1.ResourceLimitsCPU]; ok {
		quota.CPULimit = &cpu
	}
	if memory, ok := spec.Hard[corev1.ResourceLimitsMemory]; ok {
		quota.MemoryLimit = &memory
	}

	return quota, nil
}

// buildAnalysisResult creates an AnalysisResult from metrics and recommendations.
func (r *TailoringReconciler) buildAnalysisResult(metrics []prometheus.ContainerMetrics, recommendations []recommender.Recommendation, currentResources map[string]autoscalingv1alpha1.ResourceRequirements, window time.Duration) autoscalingv1alpha1.AnalysisResult {
	result := autoscalingv1alpha1.AnalysisResult{
		Timestamp:      metav1.Now(),
		AnalysisWindow: metav1.Duration{Duration: window},
		Containers:     make([]autoscalingv1alpha1.ContainerRecommendation, 0, len(recommendations)),
	}

	metricsMap := make(map[string]prometheus.ContainerMetrics)
	for _, m := range metrics {
		metricsMap[m.ContainerName] = m
	}

	for _, rec := range recommendations {
		m := metricsMap[rec.ContainerName]
		current, _ := currentResources[rec.ContainerName]

		containerRec := autoscalingv1alpha1.ContainerRecommendation{
			Name:    rec.ContainerName,
			Current: current, // Include current resources for UI comparison
			Recommended: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{
					CPU:    rec.Requests.CPU,
					Memory: rec.Requests.Memory,
				},
				Limits: &autoscalingv1alpha1.ResourceValues{
					CPU:    rec.Limits.CPU,
					Memory: rec.Limits.Memory,
				},
			},
			P95CPU:    m.P95CPU,
			P99CPU:    m.P99CPU,
			P95Memory: m.P95Memory,
			P99Memory: m.P99Memory,
		}
		result.Containers = append(result.Containers, containerRec)
	}

	return result
}

// setCondition sets a condition on the Tailoring status.
func (r *TailoringReconciler) setCondition(tailoring *autoscalingv1alpha1.Tailoring, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	meta.SetStatusCondition(&tailoring.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TailoringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.Tailoring{}).
		Named("tailoring").
		Complete(r)
}
