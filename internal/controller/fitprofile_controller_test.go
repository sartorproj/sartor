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
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

var _ = Describe("FitProfile Controller", func() {
	const (
		FitProfileNamespace = "default"

		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	// Helper function to create a unique name
	var uniqueName = func(prefix string) string {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}

	// Helper function to cleanup a FitProfile
	var cleanupFitProfile = func(name, namespace string) {
		fp := &autoscalingv1alpha1.FitProfile{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, fp)
		if err == nil {
			_ = k8sClient.Delete(ctx, fp)
		}
	}

	Context("When creating a FitProfile", func() {
		var fitProfileName string

		BeforeEach(func() {
			fitProfileName = uniqueName("test-fitprofile")
		})

		AfterEach(func() {
			cleanupFitProfile(fitProfileName, FitProfileNamespace)
		})

		It("Should create successfully with percentile strategy", func() {
			By("Creating a FitProfile with percentile strategy")
			params := map[string]interface{}{
				"requestsBufferPercent": 20,
				"limitsBufferPercent":   50,
			}
			paramsJSON, _ := json.Marshal(params)

			fitProfile := &autoscalingv1alpha1.FitProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fitProfileName,
					Namespace: FitProfileNamespace,
				},
				Spec: autoscalingv1alpha1.FitProfileSpec{
					Strategy:    "percentile",
					DisplayName: "Balanced Profile",
					Description: "A balanced approach for general workloads",
					Parameters: &runtime.RawExtension{
						Raw: paramsJSON,
					},
				},
			}
			Expect(k8sClient.Create(ctx, fitProfile)).Should(Succeed())

			lookupKey := types.NamespacedName{Name: fitProfileName, Namespace: FitProfileNamespace}
			createdFP := &autoscalingv1alpha1.FitProfile{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, lookupKey, createdFP)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdFP.Spec.Strategy).Should(Equal("percentile"))
			Expect(createdFP.Spec.DisplayName).Should(Equal("Balanced Profile"))
		})

		It("Should create successfully with DSP strategy", func() {
			By("Creating a FitProfile with DSP strategy")
			params := map[string]interface{}{
				"method":                "fourier",
				"marginFraction":        0.15,
				"lowAmplitudeThreshold": 0.05,
				"enablePeakDetection":   true,
				"enableNormalization":   true,
			}
			paramsJSON, _ := json.Marshal(params)

			fitProfile := &autoscalingv1alpha1.FitProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fitProfileName,
					Namespace: FitProfileNamespace,
				},
				Spec: autoscalingv1alpha1.FitProfileSpec{
					Strategy:    "dsp",
					DisplayName: "DSP-Based Profile",
					Description: "Uses digital signal processing for analysis",
					Parameters: &runtime.RawExtension{
						Raw: paramsJSON,
					},
				},
			}
			Expect(k8sClient.Create(ctx, fitProfile)).Should(Succeed())

			lookupKey := types.NamespacedName{Name: fitProfileName, Namespace: FitProfileNamespace}
			createdFP := &autoscalingv1alpha1.FitProfile{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, lookupKey, createdFP)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdFP.Spec.Strategy).Should(Equal("dsp"))
		})

		It("Should create successfully with no parameters", func() {
			By("Creating a FitProfile without parameters")
			fitProfile := &autoscalingv1alpha1.FitProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fitProfileName,
					Namespace: FitProfileNamespace,
				},
				Spec: autoscalingv1alpha1.FitProfileSpec{
					Strategy:    "percentile",
					DisplayName: "Default Profile",
				},
			}
			Expect(k8sClient.Create(ctx, fitProfile)).Should(Succeed())

			lookupKey := types.NamespacedName{Name: fitProfileName, Namespace: FitProfileNamespace}
			createdFP := &autoscalingv1alpha1.FitProfile{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, lookupKey, createdFP)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdFP.Spec.Parameters).Should(BeNil())
		})
	})

	Context("When FitProfile has safety rails override", func() {
		var fitProfileName string

		BeforeEach(func() {
			fitProfileName = uniqueName("test-fp-rails")
		})

		AfterEach(func() {
			cleanupFitProfile(fitProfileName, FitProfileNamespace)
		})

		It("Should store safety rails override configuration", func() {
			By("Creating a FitProfile with safety rails override")
			minCPU := resource.MustParse("100m")
			minMem := resource.MustParse("128Mi")
			maxCPU := resource.MustParse("4")
			maxMem := resource.MustParse("8Gi")

			fitProfile := &autoscalingv1alpha1.FitProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fitProfileName,
					Namespace: FitProfileNamespace,
				},
				Spec: autoscalingv1alpha1.FitProfileSpec{
					Strategy:    "percentile",
					DisplayName: "Eco Profile with Rails",
					SafetyRailsOverride: &autoscalingv1alpha1.SafetyRailsConfig{
						MinCPU:    &minCPU,
						MinMemory: &minMem,
						MaxCPU:    &maxCPU,
						MaxMemory: &maxMem,
					},
				},
			}
			Expect(k8sClient.Create(ctx, fitProfile)).Should(Succeed())

			lookupKey := types.NamespacedName{Name: fitProfileName, Namespace: FitProfileNamespace}
			createdFP := &autoscalingv1alpha1.FitProfile{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, lookupKey, createdFP)
				return err == nil && createdFP.Spec.SafetyRailsOverride != nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdFP.Spec.SafetyRailsOverride.MinCPU.String()).Should(Equal("100m"))
			Expect(createdFP.Spec.SafetyRailsOverride.MinMemory.String()).Should(Equal("128Mi"))
			Expect(createdFP.Spec.SafetyRailsOverride.MaxCPU.String()).Should(Equal("4"))
			Expect(createdFP.Spec.SafetyRailsOverride.MaxMemory.String()).Should(Equal("8Gi"))
		})
	})

	Context("When FitProfile has priority and labels", func() {
		var fitProfileName string

		BeforeEach(func() {
			fitProfileName = uniqueName("test-fp-priority")
		})

		AfterEach(func() {
			cleanupFitProfile(fitProfileName, FitProfileNamespace)
		})

		It("Should store priority and labels", func() {
			By("Creating a FitProfile with priority and labels")
			fitProfile := &autoscalingv1alpha1.FitProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fitProfileName,
					Namespace: FitProfileNamespace,
				},
				Spec: autoscalingv1alpha1.FitProfileSpec{
					Strategy:    "percentile",
					DisplayName: "High Priority Profile",
					Priority:    100,
					Labels: map[string]string{
						"environment": "production",
						"tier":        "critical",
					},
				},
			}
			Expect(k8sClient.Create(ctx, fitProfile)).Should(Succeed())

			lookupKey := types.NamespacedName{Name: fitProfileName, Namespace: FitProfileNamespace}
			createdFP := &autoscalingv1alpha1.FitProfile{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, lookupKey, createdFP)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdFP.Spec.Priority).Should(Equal(int32(100)))
			Expect(createdFP.Spec.Labels["environment"]).Should(Equal("production"))
			Expect(createdFP.Spec.Labels["tier"]).Should(Equal("critical"))
		})
	})

	Context("When deleting a FitProfile", func() {
		var fitProfileName string

		BeforeEach(func() {
			fitProfileName = uniqueName("test-fp-del")
		})

		It("Should delete successfully", func() {
			By("Creating a FitProfile")
			fitProfile := &autoscalingv1alpha1.FitProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fitProfileName,
					Namespace: FitProfileNamespace,
				},
				Spec: autoscalingv1alpha1.FitProfileSpec{
					Strategy: "percentile",
				},
			}
			Expect(k8sClient.Create(ctx, fitProfile)).Should(Succeed())

			lookupKey := types.NamespacedName{Name: fitProfileName, Namespace: FitProfileNamespace}

			// Wait for creation
			createdFP := &autoscalingv1alpha1.FitProfile{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, lookupKey, createdFP)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			By("Deleting the FitProfile")
			Expect(k8sClient.Delete(ctx, createdFP)).Should(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, lookupKey, &autoscalingv1alpha1.FitProfile{})
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When FitProfile is referenced by Tailoring", func() {
		var fitProfileName, tailoringName, atelierName string

		BeforeEach(func() {
			fitProfileName = uniqueName("test-fp-ref")
			tailoringName = uniqueName("test-tailoring-ref")
			atelierName = uniqueName("test-atelier-ref")

			// Create Atelier
			atelier := &autoscalingv1alpha1.Atelier{
				ObjectMeta: metav1.ObjectMeta{
					Name: atelierName,
				},
				Spec: autoscalingv1alpha1.AtelierSpec{
					Prometheus: autoscalingv1alpha1.PrometheusConfig{
						URL: "http://prometheus:9090",
					},
					GitProvider: autoscalingv1alpha1.GitProviderConfig{
						Type: autoscalingv1alpha1.GitProviderGitHub,
						SecretRef: corev1.SecretReference{
							Name:      "github-token",
							Namespace: "default",
						},
					},
				},
			}
			_ = k8sClient.Create(ctx, atelier)
		})

		AfterEach(func() {
			// Cleanup Tailoring first
			tailoring := &autoscalingv1alpha1.Tailoring{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: tailoringName, Namespace: FitProfileNamespace}, tailoring)
			if err == nil {
				_ = k8sClient.Delete(ctx, tailoring)
			}

			cleanupFitProfile(fitProfileName, FitProfileNamespace)

			// Cleanup Atelier
			atelier := &autoscalingv1alpha1.Atelier{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, atelier)
			if err == nil {
				_ = k8sClient.Delete(ctx, atelier)
			}
		})

		It("Should track usage count", func() {
			By("Creating a FitProfile")
			fitProfile := &autoscalingv1alpha1.FitProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fitProfileName,
					Namespace: FitProfileNamespace,
				},
				Spec: autoscalingv1alpha1.FitProfileSpec{
					Strategy:    "percentile",
					DisplayName: "Referenced Profile",
				},
			}
			Expect(k8sClient.Create(ctx, fitProfile)).Should(Succeed())

			By("Creating a Tailoring that references this FitProfile")
			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: FitProfileNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "test-deployment",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name:      fitProfileName,
						Namespace: FitProfileNamespace,
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:       autoscalingv1alpha1.WriteBackTypeRaw,
						Repository: "org/repo",
						Path:       "apps/test/deployment.yaml",
						Branch:     "main",
					},
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			// Verify Tailoring is created
			tailoringLookup := types.NamespacedName{Name: tailoringName, Namespace: FitProfileNamespace}
			createdTailoring := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, tailoringLookup, createdTailoring)
				return err == nil
			}, timeout, interval).Should(BeTrue())
		})
	})
})
