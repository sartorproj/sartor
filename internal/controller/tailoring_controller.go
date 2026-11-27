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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/writer"
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
	// ConditionTypeFitProfileResolved indicates whether the referenced
	// FitProfile was used as configured.
	ConditionTypeFitProfileResolved = "FitProfileResolved"

	// ReasonFitProfileFallback is set on the FitProfileResolved and Analyzed
	// conditions when the analysis could not use the FitProfile as
	// configured and fell back to default strategy settings.
	ReasonFitProfileFallback = "FitProfileFallback"

	// SartorIgnoreLabel is the label that indicates the PR should be ignored.
	SartorIgnoreLabel = "sartor-ignore"

	// DefaultRequeueInterval is the retry interval after a failed reconciliation.
	DefaultRequeueInterval = 5 * time.Minute
	// DefaultAnalysisInterval is the default minimum time between analyses of
	// an unchanged Tailoring (--analysis-interval).
	DefaultAnalysisInterval = 6 * time.Hour
	// PRCheckInterval is the interval to check PR status.
	PRCheckInterval = 5 * time.Minute
)

// TailoringReconciler reconciles a Tailoring object.
type TailoringReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AnalysisInterval is the minimum time between analyses of a Tailoring
	// whose spec has not changed. Defaults to DefaultAnalysisInterval.
	AnalysisInterval time.Duration

	// ControllerNamespace is the only namespace credential Secrets are read from.
	ControllerNamespace string

	// EnableGitWriteBack allows the controller to push changes and open PRs
	// (--enable-git-writeback). When false the controller never touches git.
	EnableGitWriteBack bool

	// NewGitProvider creates the Git provider; defaults to DefaultGitProviderFactory.
	NewGitProvider GitProviderFactory

	// WorkloadSource selects where workload state is read from
	// (--workload-source). Defaults to WorkloadSourceKubernetes.
	WorkloadSource WorkloadSource

	// APIReader reads autoscalers without the cache; defaults to the client.
	APIReader client.Reader

	// now returns the current time; overridable in tests.
	now func() time.Time
	// jitter returns the spread offset in [0, 1); overridable in tests.
	jitter func(*autoscalingv1alpha1.Tailoring) float64
	// startedAt is when the controller started; analyses that fell due
	// before it are spread after it.
	startedAt time.Time
}

// analysisInterval returns the configured analysis interval or the default.
func (r *TailoringReconciler) analysisInterval() time.Duration {
	if r.AnalysisInterval > 0 {
		return r.AnalysisInterval
	}
	return DefaultAnalysisInterval
}

func (r *TailoringReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=ateliers,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list
// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
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

	if dueIn := r.analysisDueIn(tailoring); dueIn > 0 {
		logger.V(1).Info("Last analysis is fresh, skipping", "name", tailoring.Name, "namespace", tailoring.Namespace, "nextAnalysisIn", dueIn)
		return ctrl.Result{RequeueAfter: dueIn}, nil
	}

	logger.Info("Reconciling Tailoring", "name", tailoring.Name, "namespace", tailoring.Namespace)

	// Get the Atelier configuration
	atelier, err := r.getAtelier(ctx)
	if err != nil {
		logger.Error(err, "Failed to get Atelier")
		return r.handleReconcileError(ctx, tailoring, logger, ConditionTypeReady, "AtelierNotFound", err.Error())
	}

	// Get Prometheus credentials if configured
	promConfig, err := r.buildPrometheusConfig(ctx, atelier)
	if err != nil {
		logger.Error(err, "Failed to build Prometheus config")
		reason := "PrometheusConfigError"
		if IsSecretNamespaceError(err) {
			reason = ReasonSecretNamespaceNotAllowed
		}
		return r.handleReconcileError(ctx, tailoring, logger, ConditionTypeAnalyzed, reason, err.Error())
	}

	// Create Prometheus client
	promClient, err := prometheus.NewClient(promConfig)
	if err != nil {
		logger.Error(err, "Failed to create Prometheus client")
		return r.handleReconcileError(ctx, tailoring, logger, ConditionTypeAnalyzed, "PrometheusClientError", err.Error())
	}

	view, err := r.readWorkload(ctx, tailoring, promClient, logger)
	if err != nil {
		logger.Error(err, "Failed to read target workload")
		if errors.Is(err, errTargetNotFound) || !r.metricsOnly() {
			tailoring.Status.TargetFound = false
			return r.handleReconcileError(ctx, tailoring, logger, ConditionTypeTargetFound, "TargetNotFound", err.Error())
		}
		return r.handleReconcileError(ctx, tailoring, logger, ConditionTypeAnalyzed, "MetricsQueryError", err.Error())
	}
	currentResources := view.current
	tailoring.Status.TargetFound = true
	r.setCondition(tailoring, ConditionTypeTargetFound, metav1.ConditionTrue, "TargetFound", "Target workload found")
	r.setWorkloadStatus(tailoring, view)

	// Determine analysis window
	analysisWindow := 7 * 24 * time.Hour // Default 7 days
	if tailoring.Spec.AnalysisWindow != nil {
		analysisWindow = tailoring.Spec.AnalysisWindow.Duration
	} else if atelier.Spec.DefaultAnalysisWindow != nil {
		analysisWindow = atelier.Spec.DefaultAnalysisWindow.Duration
	}

	// Query Prometheus for metrics
	metrics, err := promClient.GetContainerMetrics(ctx, tailoring.TargetNamespace(), targetWorkload(tailoring), analysisWindow)
	if err != nil {
		logger.Error(err, "Failed to get container metrics")
		return r.handleReconcileError(ctx, tailoring, logger, ConditionTypeAnalyzed, "MetricsQueryError", err.Error())
	}

	// Filter out excluded containers
	metrics = r.filterExcludedContainers(metrics, tailoring.Spec.ExcludeContainers)

	if len(metrics) == 0 {
		logger.Info("No metrics found for target workload")
		return r.handleReconcileError(ctx, tailoring, logger, ConditionTypeAnalyzed, "NoMetrics", "No metrics found for target workload")
	}

	safetyRails := r.getSafetyRails(atelier)
	quota := view.quota

	calculator := recommender.NewCalculator()

	// Resolve FitProfile to get strategy and parameters
	profile := r.resolveFitProfile(ctx, tailoring, calculator.GetRegistry(), logger)
	r.setFitProfileCondition(tailoring, profile)
	strategyName, strategyParams := profile.strategy, profile.params

	// Apply safety rails override from FitProfile if provided
	applySafetyRailsOverride(&safetyRails, profile.safetyRails)

	// Calculate recommendations using strategy-based calculator
	if s, ok := calculator.GetRegistry().Get(strategyName); ok {
		if hs, ok := s.(strategy.HistoryStrategy); ok {
			addUsageHistory(ctx, promClient, tailoring, metrics, analysisWindow, hs.HistoryStep(analysisWindow), logger)
		}
	}
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
		profile.enablePeakDetection,
		profile.enableNormalization,
	)
	if err != nil {
		logger.Error(err, "Failed to calculate recommendations")
		return r.handleReconcileError(ctx, tailoring, logger, ConditionTypeAnalyzed, "CalculationError", err.Error())
	}

	applyOOMFloor(recommendations, currentResources, view.oomKilled)
	tailoring.Status.Scaling = applyScaling(ctx, tailoring, promClient, view, metrics, recommendations, safetyRails, analysisWindow, logger)
	r.setConflictCondition(tailoring)
	r.setScalingPointCondition(tailoring, recommendations, currentResources, assumedKeptBy(tailoring))

	// Convert strategy recommendations to old format for compatibility
	legacyRecommendations := convertStrategyRecommendations(recommendations)

	// Update analysis result in status (include current resources for comparison)
	analysisResult := r.buildAnalysisResult(metrics, legacyRecommendations, currentResources, analysisWindow)
	analysisResult.Timestamp = metav1.NewTime(r.clock())
	analysisResult.Replicas = view.replicas
	tailoring.Status.LastAnalysis = &analysisResult
	tailoring.Status.ObservedGeneration = tailoring.Generation
	tailoring.Status.ConsecutiveFailures = 0
	tailoring.Status.NextAnalysisTime = &metav1.Time{Time: r.nextAnalysisAt(tailoring)}
	if profile.fallback != "" {
		r.setCondition(tailoring, ConditionTypeAnalyzed, metav1.ConditionTrue, ReasonFitProfileFallback,
			"Resource analysis completed with a fallback: "+profile.fallback)
	} else {
		r.setCondition(tailoring, ConditionTypeAnalyzed, metav1.ConditionTrue, "AnalysisComplete", "Resource analysis completed")
	}

	// Check if Tailoring is paused (dry-run mode)
	if tailoring.Spec.Paused {
		logger.Info("Tailoring is paused, skipping Cut creation")
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Paused", "Tailoring is paused, recommendations calculated but no Cut created")
		return r.updateStatusAndRequeue(ctx, tailoring, logger)
	}

	if len(view.disagreeing) > 0 {
		logger.Info("Pods disagree on requests/limits, skipping write-back until the next analysis", "containers", view.disagreeing)
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionFalse, ReasonPodsDisagree,
			fmt.Sprintf("Pods of the target report different requests/limits for %s (e.g. during a rollout); write-back skipped until the next analysis",
				strings.Join(view.disagreeing, ", ")))
		return r.updateStatusAndRequeue(ctx, tailoring, logger)
	}

	// Check cooldown period
	if !r.checkCooldownPeriod(tailoring, atelier) {
		logger.Info("Cooldown period not elapsed, skipping PR creation")
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "CooldownActive", "Cooldown period has not elapsed")
		return r.updateStatusAndRequeue(ctx, tailoring, logger)
	}

	// Check if changes exceed threshold
	minChangePercent := getMinChangePercent(atelier)
	if !recommender.ShouldCreateCut(recommendations, currentResources, minChangePercent) {
		logger.Info("Changes do not exceed threshold, skipping PR creation")
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "BelowThreshold", "Resource changes below threshold")
		return r.updateStatusAndRequeue(ctx, tailoring, logger)
	}

	if notes := r.reconcileGitOps(ctx, tailoring, atelier, currentResources, logger); len(notes) > 0 {
		keptBy := map[string]bool{}
		for _, n := range notes {
			if n.Kind == writer.KindScaledObject && n.KeepsScalingPoint {
				keptBy[n.Name] = true
			}
		}
		r.setScalingPointCondition(tailoring, recommendations, currentResources, keptBy)
	}
	return r.updateStatusAndRequeue(ctx, tailoring, logger)
}

// updateStatusAndRequeue persists the status after an analysis and schedules
// the next one. The status write itself does not trigger a reconcile because
// the controller only reacts to generation (spec) changes.
func (r *TailoringReconciler) updateStatusAndRequeue(
	ctx context.Context,
	tailoring *autoscalingv1alpha1.Tailoring,
	logger logr.Logger,
) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, tailoring); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: max(r.analysisDueIn(tailoring), time.Second)}, nil
}

// handleReconcileError records a failed analysis and retries with exponential
// backoff (persisted in the status, so restarts and spec edits keep it).
func (r *TailoringReconciler) handleReconcileError(
	ctx context.Context,
	tailoring *autoscalingv1alpha1.Tailoring,
	logger logr.Logger,
	conditionType, reason, message string,
) (ctrl.Result, error) {
	r.setCondition(tailoring, conditionType, metav1.ConditionFalse, reason, message)
	wait := r.scheduleRetry(tailoring)
	if updateErr := r.Status().Update(ctx, tailoring); updateErr != nil {
		logger.Error(updateErr, "Failed to update status")
	}
	return ctrl.Result{RequeueAfter: wait}, nil
}

// applySafetyRailsOverride applies FitProfile safety rails overrides to the base safety rails.
func applySafetyRailsOverride(safetyRails *recommender.SafetyRails, override *autoscalingv1alpha1.SafetyRailsConfig) {
	if override == nil {
		return
	}
	if override.MinCPU != nil {
		safetyRails.MinCPU = override.MinCPU
	}
	if override.MinMemory != nil {
		safetyRails.MinMemory = override.MinMemory
	}
	if override.MaxCPU != nil {
		safetyRails.MaxCPU = override.MaxCPU
	}
	if override.MaxMemory != nil {
		safetyRails.MaxMemory = override.MaxMemory
	}
}

// checkCooldownPeriod checks if the cooldown period has elapsed since the last PR update.
func (r *TailoringReconciler) checkCooldownPeriod(tailoring *autoscalingv1alpha1.Tailoring, atelier *autoscalingv1alpha1.Atelier) bool {
	if tailoring.Status.LastPRUpdateTime == nil {
		return true // No previous PR, cooldown doesn't apply
	}
	cooldown := 7 * 24 * time.Hour // Default 7 days
	if atelier.Spec.PRSettings != nil && atelier.Spec.PRSettings.CooldownPeriod != nil {
		cooldown = atelier.Spec.PRSettings.CooldownPeriod.Duration
	}
	return time.Since(tailoring.Status.LastPRUpdateTime.Time) >= cooldown
}

// getMinChangePercent returns the minimum change percentage threshold from atelier settings.
func getMinChangePercent(atelier *autoscalingv1alpha1.Atelier) int32 {
	minChangePercent := int32(20) // Default 20%
	if atelier.Spec.PRSettings != nil && atelier.Spec.PRSettings.MinChangePercent != nil {
		minChangePercent = *atelier.Spec.PRSettings.MinChangePercent
	}
	return minChangePercent
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
		if err := r.Get(ctx, types.NamespacedName{Name: target.Name, Namespace: tailoring.TargetNamespace()}, deployment); err != nil {
			return nil, err
		}
		containers = deployment.Spec.Template.Spec.Containers

	case "StatefulSet":
		statefulSet := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: target.Name, Namespace: tailoring.TargetNamespace()}, statefulSet); err != nil {
			return nil, err
		}
		containers = statefulSet.Spec.Template.Spec.Containers

	case "DaemonSet":
		daemonSet := &appsv1.DaemonSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: target.Name, Namespace: tailoring.TargetNamespace()}, daemonSet); err != nil {
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

// buildPrometheusConfig builds the Prometheus client configuration.
func (r *TailoringReconciler) buildPrometheusConfig(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) (prometheus.Config, error) {
	config := prometheus.Config{
		URL:                atelier.Spec.Prometheus.URL,
		InsecureSkipVerify: atelier.Spec.Prometheus.InsecureSkipVerify,
		ClusterLabel:       atelier.Spec.Prometheus.ClusterLabel,
		// Without the Kubernetes API, ownership always comes from kube-state-metrics.
		OwnerJoinOnly: r.metricsOnly(),
	}

	if atelier.Spec.Prometheus.SecretRef != nil {
		secret, err := getCredentialSecret(ctx, r.Client, r.ControllerNamespace, *atelier.Spec.Prometheus.SecretRef)
		if err != nil {
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

// resolvedFitProfile is the strategy configuration an analysis uses.
type resolvedFitProfile struct {
	// ref is the FitProfile as namespace/name.
	ref                 string
	strategy            string
	params              map[string]interface{}
	enablePeakDetection bool
	enableNormalization bool
	safetyRails         *autoscalingv1alpha1.SafetyRailsConfig
	// fallback explains why the FitProfile could not be used as configured;
	// empty when it was.
	fallback string
}

// resolveFitProfile resolves the FitProfile reference and extracts its
// configuration. A missing FitProfile falls back to the percentile strategy
// with default parameters, invalid parameters to default parameters, and an
// unknown strategy to the percentile strategy; fallback records why.
func (r *TailoringReconciler) resolveFitProfile(
	ctx context.Context,
	tailoring *autoscalingv1alpha1.Tailoring,
	registry *strategy.StrategyRegistry,
	logger logr.Logger,
) resolvedFitProfile {
	key := types.NamespacedName{Name: tailoring.Spec.FitProfileRef.Name, Namespace: tailoring.Spec.FitProfileRef.Namespace}
	if key.Namespace == "" {
		key.Namespace = tailoring.Namespace
	}
	res := resolvedFitProfile{
		ref:      key.String(),
		strategy: strategy.StrategyNamePercentile,
		params:   map[string]interface{}{},
	}

	fitProfile := &autoscalingv1alpha1.FitProfile{}
	if err := r.Get(ctx, key, fitProfile); err != nil {
		logger.Error(err, "Failed to get FitProfile, falling back to the percentile strategy", "fitProfile", res.ref)
		if apierrors.IsNotFound(err) {
			res.fallback = fmt.Sprintf("FitProfile %s not found; using the percentile strategy with default parameters", res.ref)
		} else {
			res.fallback = fmt.Sprintf("FitProfile %s could not be read (%v); using the percentile strategy with default parameters", res.ref, err)
		}
		return res
	}
	res.safetyRails = fitProfile.Spec.SafetyRailsOverride

	var fallbacks []string
	if fitProfile.Spec.Parameters != nil && fitProfile.Spec.Parameters.Raw != nil {
		var params map[string]interface{}
		if err := json.Unmarshal(fitProfile.Spec.Parameters.Raw, &params); err != nil {
			logger.Error(err, "Failed to parse FitProfile parameters, using default parameters", "fitProfile", res.ref)
			fallbacks = append(fallbacks, fmt.Sprintf("FitProfile %s has invalid parameters (%v); using default parameters", res.ref, err))
		} else if params != nil {
			res.params = params
		}
	}

	if _, ok := registry.Get(fitProfile.Spec.Strategy); ok {
		res.strategy = fitProfile.Spec.Strategy
	} else {
		logger.Info("FitProfile references an unknown strategy, falling back to the percentile strategy",
			"fitProfile", res.ref, "strategy", fitProfile.Spec.Strategy)
		fallbacks = append(fallbacks, fmt.Sprintf("FitProfile %s references unknown strategy %q; using the percentile strategy", res.ref, fitProfile.Spec.Strategy))
	}
	res.fallback = strings.Join(fallbacks, "; ")

	// Extract enable flags from parameters (for DSP strategy)
	if enablePeak, ok := res.params["enablePeakDetection"].(bool); ok {
		res.enablePeakDetection = enablePeak
	}
	if enableNorm, ok := res.params["enableNormalization"].(bool); ok {
		res.enableNormalization = enableNorm
	}
	return res
}

// setFitProfileCondition records whether the FitProfile was used as configured.
func (r *TailoringReconciler) setFitProfileCondition(tailoring *autoscalingv1alpha1.Tailoring, profile resolvedFitProfile) {
	if profile.fallback != "" {
		r.setCondition(tailoring, ConditionTypeFitProfileResolved, metav1.ConditionFalse, ReasonFitProfileFallback, profile.fallback)
		return
	}
	r.setCondition(tailoring, ConditionTypeFitProfileResolved, metav1.ConditionTrue, "FitProfileResolved",
		fmt.Sprintf("Using FitProfile %s with the %s strategy", profile.ref, profile.strategy))
}

// addUsageHistory fills the usage history of metrics for strategies that
// forecast from it. Without history those strategies fall back to the
// percentiles, so a failed query does not fail the analysis.
func addUsageHistory(
	ctx context.Context,
	promClient *prometheus.Client,
	tailoring *autoscalingv1alpha1.Tailoring,
	metrics []prometheus.ContainerMetrics,
	window, step time.Duration,
	logger logr.Logger,
) {
	history, err := promClient.GetContainerHistory(ctx, tailoring.TargetNamespace(), targetWorkload(tailoring), window, step)
	if err != nil {
		logger.Error(err, "Failed to get usage history, falling back to percentiles")
		return
	}
	for i := range metrics {
		h := history[metrics[i].ContainerName]
		metrics[i].CPUHistory = h.CPU
		metrics[i].MemoryHistory = h.Memory
	}
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
		current := currentResources[rec.ContainerName]

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

// tailoringPredicates filters Tailoring events: only creation, deletion and
// spec changes (metadata.generation bumps) trigger a reconcile, so the
// controller's own status writes do not re-run the analysis.
func tailoringPredicates() predicate.Predicate {
	return predicate.GenerationChangedPredicate{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *TailoringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.startedAt.IsZero() {
		r.startedAt = r.clock()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.Tailoring{}, builder.WithPredicates(tailoringPredicates())).
		Named("tailoring").
		Complete(r)
}
