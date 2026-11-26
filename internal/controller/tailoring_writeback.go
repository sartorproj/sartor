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
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/provider"
	"github.com/sartorproj/sartor/internal/gitops/writeback"
	"github.com/sartorproj/sartor/internal/gitops/writer"
)

const (
	defaultBaseBranch   = "main"
	defaultBranchPrefix = "sartor/"
)

// reconcileGitOps decides whether and how to write the latest analysis back to
// Git, and records the outcome in the Tailoring status and conditions. It never
// returns an error: Git failures are reported as conditions and retried on the
// next analysis.
//
// Git is only contacted when all of these hold: --enable-git-writeback is set,
// the Tailoring is not paused, it was not ignored via the sartor-ignore label,
// and its writeBack spec is valid.
func (r *TailoringReconciler) reconcileGitOps(
	ctx context.Context,
	tailoring *autoscalingv1alpha1.Tailoring,
	atelier *autoscalingv1alpha1.Atelier,
	live map[string]autoscalingv1alpha1.ResourceRequirements,
	logger logr.Logger,
) {
	if tailoring.Spec.Paused {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Paused",
			"Tailoring is paused, recommendations calculated but nothing written to Git")
		return
	}
	if !r.EnableGitWriteBack {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Ready",
			"Recommendations calculated (git write-back disabled)")
		r.setCondition(tailoring, ConditionTypePRCreated, metav1.ConditionFalse, "WriteBackDisabled",
			"Git write-back is disabled (--enable-git-writeback=false)")
		return
	}
	if tailoring.Status.Ignored {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Ignored",
			fmt.Sprintf("A Sartor PR was closed with the %s label; set spec.paused or delete status to resume", SartorIgnoreLabel))
		return
	}
	if tailoring.Status.LastAnalysis == nil {
		return
	}

	req, err := r.buildWriteBackRequest(tailoring, atelier)
	if err != nil {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionFalse, "InvalidWriteBack", err.Error())
		r.setCondition(tailoring, ConditionTypePRCreated, metav1.ConditionFalse, "InvalidWriteBack", err.Error())
		return
	}

	plan := writeback.BuildPlan(tailoring.Status.LastAnalysis.Containers, live, r.writeBackGuard(ctx, tailoring, atelier, logger))

	ghCfg, err := loadGitHubConfig(ctx, r.secretReader(), atelier, r.ControllerNamespace)
	if err != nil {
		logger.Error(err, "Failed to load Git credentials")
		r.setCondition(tailoring, ConditionTypePRCreated, metav1.ConditionFalse, "GitCredentialsError", err.Error())
		return
	}
	newProvider := r.NewGitProvider
	if newProvider == nil {
		newProvider = DefaultGitProviderFactory
	}
	gitProvider, err := newProvider(ghCfg)
	if err != nil {
		r.setCondition(tailoring, ConditionTypePRCreated, metav1.ConditionFalse, "GitProviderError", err.Error())
		return
	}

	if stop := r.syncPRStatus(ctx, gitProvider, tailoring, req.Repository, logger); stop {
		return
	}

	if tailoring.Status.PRState != autoscalingv1alpha1.PRStateOpen && !r.checkCooldownPeriod(tailoring, atelier) {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "CooldownActive", "Cooldown period has not elapsed")
		return
	}

	minChangePercent := getMinChangePercent(atelier)
	if len(plan.Updates) == 0 || plan.MaxRequestChangePercent()+1e-9 < float64(minChangePercent) {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "BelowThreshold", "Resource changes below threshold")
		return
	}

	req.Updates = plan.Updates
	req.Body = buildPRBody(tailoring, plan)

	result, err := writeback.Apply(ctx, gitProvider, req)
	if err != nil {
		logger.Error(err, "Git write-back failed")
		tailoring.Status.PRMessage = err.Error()
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionFalse, "WriteBackFailed", err.Error())
		r.setCondition(tailoring, ConditionTypePRCreated, metav1.ConditionFalse, "WriteBackFailed", err.Error())
		return
	}

	tailoring.Status.BranchName = req.Branch
	tailoring.Status.PRMessage = ""
	if result.CommitSHA != "" {
		tailoring.Status.CommitSHA = result.CommitSHA
		tailoring.Status.LastPRUpdateTime = &metav1.Time{Time: time.Now()}
	}
	if result.PR == nil {
		r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "UpToDate",
			"Git already contains the recommended resources")
		return
	}
	tailoring.Status.PRNumber = result.PR.Number
	tailoring.Status.PRUrl = result.PR.URL
	tailoring.Status.PRState = autoscalingv1alpha1.PRStateOpen
	if result.PR.LabelError != nil {
		tailoring.Status.PRMessage = result.PR.LabelError.Error()
	}
	logger.Info("Git write-back done", "outcome", result.Outcome, "pr", result.PR.URL)
	r.setCondition(tailoring, ConditionTypePRCreated, metav1.ConditionTrue, string(result.Outcome),
		fmt.Sprintf("Pull request #%d %s", result.PR.Number, strings.ToLower(string(result.Outcome))))
	r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "PROpen", "Recommendations proposed in "+result.PR.URL)
}

// syncPRStatus refreshes the state of the last known PR. It returns true when
// write-back must stop for this reconcile (the PR was closed with sartor-ignore).
func (r *TailoringReconciler) syncPRStatus(
	ctx context.Context,
	p provider.Provider,
	tailoring *autoscalingv1alpha1.Tailoring,
	repository string,
	logger logr.Logger,
) bool {
	st := &tailoring.Status
	if st.PRNumber == 0 || (st.PRState != autoscalingv1alpha1.PRStateOpen && st.PRState != "") {
		return false
	}
	pr, err := p.GetPullRequest(ctx, repository, st.PRNumber)
	if err != nil {
		logger.Error(err, "Failed to refresh pull request status", "pr", st.PRNumber)
		return false
	}
	switch {
	case pr.IsMerged:
		st.PRState = autoscalingv1alpha1.PRStateMerged
		if pr.MergedAt != nil {
			st.MergedAt = &metav1.Time{Time: *pr.MergedAt}
		}
		r.setCondition(tailoring, ConditionTypePRMerged, metav1.ConditionTrue, "Merged", fmt.Sprintf("Pull request #%d merged", pr.Number))
	case pr.State == "closed":
		st.PRState = autoscalingv1alpha1.PRStateClosed
		for _, l := range pr.Labels {
			if l == SartorIgnoreLabel {
				st.Ignored = true
				r.setCondition(tailoring, ConditionTypeReady, metav1.ConditionTrue, "Ignored",
					fmt.Sprintf("Pull request #%d was closed with the %s label", pr.Number, SartorIgnoreLabel))
				return true
			}
		}
	}
	return false
}

// buildWriteBackRequest validates the Tailoring's writeBack spec without contacting Git.
func (r *TailoringReconciler) buildWriteBackRequest(
	tailoring *autoscalingv1alpha1.Tailoring,
	atelier *autoscalingv1alpha1.Atelier,
) (writeback.Request, error) {
	wb := tailoring.Spec.WriteBack
	repo, err := writeback.NormalizeRepository(wb.Repository)
	if err != nil {
		return writeback.Request{}, err
	}

	base := wb.Branch
	prefix := defaultBranchPrefix
	var labels []string
	if ps := atelier.Spec.PRSettings; ps != nil {
		if base == "" {
			base = ps.BaseBranch
		}
		if ps.BranchPrefix != "" {
			prefix = ps.BranchPrefix
		}
		labels = ps.Labels
	}
	if base == "" {
		base = defaultBaseBranch
	}

	target := tailoring.Spec.Target
	req := writeback.Request{
		Repository:    repo,
		BaseBranch:    base,
		Branch:        prefix + tailoring.Namespace + "/" + tailoring.Name,
		Path:          wb.Path,
		Type:          wb.Type,
		HelmValuePath: wb.HelmValuePath,
		Target:        writeback.Target{Kind: target.Kind, Name: target.Name, Namespace: tailoring.Namespace},
		Title:         fmt.Sprintf("chore(sartor): right-size %s %s/%s", target.Kind, tailoring.Namespace, target.Name),
		CommitMessage: fmt.Sprintf("chore(sartor): right-size %s %s/%s\n\nGenerated by Sartor from Tailoring %s/%s.",
			target.Kind, tailoring.Namespace, target.Name, tailoring.Namespace, tailoring.Name),
		Labels: labels,
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
		MaxChangePercent: writeback.DefaultMaxChangePercent,
		MinCPU:           rails.MinCPU,
		MaxCPU:           rails.MaxCPU,
		MinMemory:        rails.MinMemory,
		MaxMemory:        rails.MaxMemory,
	}
	if v, ok := params["maxChangePercent"].(float64); ok && v >= 0 {
		guard.MaxChangePercent = v
	}
	return guard
}

// buildPRBody renders the PR description, including any guard notes.
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

	body := writer.GeneratePRDescriptionDetailed(plan.Updates, writer.PRDescriptionOptions{
		AnalysisWindow:   analysis.AnalysisWindow.Duration.String(),
		TargetName:       tailoring.Spec.Target.Name,
		TargetKind:       tailoring.Spec.Target.Kind,
		Namespace:        tailoring.Namespace,
		CurrentResources: plan.Current,
		Metrics:          metrics,
	})

	var b strings.Builder
	b.WriteString(body)
	if len(plan.Notes) > 0 {
		b.WriteString("\n### Safety adjustments\n\n")
		for _, n := range plan.Notes {
			b.WriteString("- " + n + "\n")
		}
	}
	fmt.Fprintf(&b, "\n<!-- sartor:tailoring=%s/%s -->\n", tailoring.Namespace, tailoring.Name)
	return b.String()
}
