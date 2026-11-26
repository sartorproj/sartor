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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/writeback"
	"github.com/sartorproj/sartor/internal/gitops/writer"
)

const reasonWriteBackFailed = "WriteBackFailed"

// reconcileGitOps writes the latest analysis back to Git and records the
// outcome in the Tailoring status and conditions. It never returns an error:
// Git failures are reported as conditions and retried on the next analysis.
//
// Git is only contacted when --enable-git-writeback is set and the Tailoring
// is not paused. It returns what was done to autoscalers in the Git file.
func (r *TailoringReconciler) reconcileGitOps(
	ctx context.Context,
	tailoring *autoscalingv1alpha1.Tailoring,
	atelier *autoscalingv1alpha1.Atelier,
	live map[string]autoscalingv1alpha1.ResourceRequirements,
	logger logr.Logger,
) []writer.AutoscalerNote {
	if tailoring.Spec.Paused {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Paused",
			"Tailoring is paused, recommendations calculated but nothing written to Git")
		return nil
	}
	if !r.EnableGitWriteBack || tailoring.Status.LastAnalysis == nil {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Ready", "Recommendations calculated")
		return nil
	}

	fail := func(err error) {
		logger.Error(err, "Git write-back failed")
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionFalse, reasonWriteBackFailed, err.Error())
		r.setCondition(tailoring, ConditionTypePRCreated, metav1.ConditionFalse, reasonWriteBackFailed, err.Error())
	}

	req, err := buildWriteBackRequest(tailoring, atelier)
	if err != nil {
		fail(err)
		return nil
	}

	ghCfg, err := loadGitHubConfig(ctx, r.Client, atelier, r.ControllerNamespace)
	if err != nil {
		fail(err)
		return nil
	}
	newProvider := r.NewGitProvider
	if newProvider == nil {
		newProvider = DefaultGitProviderFactory
	}
	gitProvider, err := newProvider(ghCfg)
	if err != nil {
		fail(err)
		return nil
	}

	plan := writeback.BuildPlan(tailoring.Status.LastAnalysis.Containers, live, r.writeBackGuard(ctx, tailoring, atelier, logger))
	req.Updates = plan.Updates
	req.Current = plan.Current
	req.Autoscalers = tailoring.Status.Autoscalers
	req.LiveFromPods = r.metricsOnly()
	req.Body = buildPRBody(tailoring, plan)

	result, err := writeback.Apply(ctx, gitProvider, req)
	if err != nil {
		fail(err)
		return nil
	}

	tailoring.Status.BranchName = req.Branch
	if result.CommitSHA != "" {
		tailoring.Status.LastPRUpdateTime = &metav1.Time{Time: time.Now()}
	}
	if result.PR != nil {
		tailoring.Status.PRNumber = result.PR.Number
		tailoring.Status.PRUrl = result.PR.URL
		tailoring.Status.PRState = autoscalingv1alpha1.PRStateOpen
		logger.Info("Git write-back done", "outcome", result.Outcome, "pr", result.PR.URL)
		r.setCondition(tailoring, ConditionTypePRCreated, metav1.ConditionTrue, string(result.Outcome),
			fmt.Sprintf("Pull request #%d: %s", result.PR.Number, result.PR.URL))
	}
	r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Ready", "Recommendations calculated")
	return result.Autoscalers
}

// buildWriteBackRequest validates the Tailoring's writeBack spec without contacting Git.
func buildWriteBackRequest(
	tailoring *autoscalingv1alpha1.Tailoring,
	atelier *autoscalingv1alpha1.Atelier,
) (writeback.Request, error) {
	wb := tailoring.Spec.WriteBack
	repo, err := writeback.NormalizeRepository(wb.Repository)
	if err != nil {
		return writeback.Request{}, err
	}

	base := wb.Branch
	var prefix string
	if ps := atelier.Spec.PRSettings; ps != nil {
		if base == "" {
			base = ps.BaseBranch
		}
		prefix = ps.BranchPrefix
	}

	target := tailoring.Spec.Target
	ns := tailoring.TargetNamespace()
	req := writeback.Request{
		Repository:    repo,
		BaseBranch:    base,
		Branch:        prefix + tailoring.Namespace + "/" + tailoring.Name,
		Path:          wb.Path,
		Type:          wb.Type,
		HelmValuePath: wb.HelmValuePath,
		Target:        writeback.Target{Kind: target.Kind, Name: target.Name, Namespace: ns},
		Title:         fmt.Sprintf("chore(sartor): right-size %s %s/%s", target.Kind, ns, target.Name),
		CommitMessage: fmt.Sprintf("chore(sartor): right-size %s %s/%s", target.Kind, ns, target.Name),
	}
	return req, req.Validate()
}

// writeBackGuard collects the limits applied to every value written to Git.
func (r *TailoringReconciler) writeBackGuard(
	ctx context.Context,
	tailoring *autoscalingv1alpha1.Tailoring,
	atelier *autoscalingv1alpha1.Atelier,
	logger logr.Logger,
) writeback.Guard {
	rails := r.getSafetyRails(atelier)
	_, params, _, _, override := r.resolveFitProfile(ctx, tailoring, atelier, logger)
	applySafetyRailsOverride(&rails, override)

	guard := writeback.Guard{
		MinCPU:    rails.MinCPU,
		MaxCPU:    rails.MaxCPU,
		MinMemory: rails.MinMemory,
		MaxMemory: rails.MaxMemory,
	}
	if v, ok := params["maxChangePercent"].(float64); ok && v >= 0 {
		guard.MaxChangePercent = v
	}
	return guard
}

// buildPRBody renders the PR description.
func buildPRBody(tailoring *autoscalingv1alpha1.Tailoring, plan writeback.Plan) string {
	analysis := tailoring.Status.LastAnalysis
	metrics := make(map[string]*writer.MetricsInfo)
	for _, c := range analysis.Containers {
		metrics[c.Name] = &writer.MetricsInfo{
			P95CPU:    quantityString(c.P95CPU),
			P99CPU:    quantityString(c.P99CPU),
			P95Memory: quantityString(c.P95Memory),
			P99Memory: quantityString(c.P99Memory),
		}
	}

	return writer.GeneratePRDescriptionDetailed(plan.Updates, writer.PRDescriptionOptions{
		AnalysisWindow:   analysis.AnalysisWindow.Duration.String(),
		TargetName:       tailoring.Spec.Target.Name,
		TargetKind:       tailoring.Spec.Target.Kind,
		Namespace:        tailoring.TargetNamespace(),
		CurrentResources: plan.Current,
		Metrics:          metrics,
	})
}

func quantityString(q *resource.Quantity) string {
	if q == nil {
		return ""
	}
	return q.String()
}
