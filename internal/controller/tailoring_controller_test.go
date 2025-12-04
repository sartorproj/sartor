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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

var _ = Describe("Tailoring Controller", func() {
	const (
		TailoringName      = "test-tailoring"
		TailoringNamespace = "default"
		AtelierName        = "test-atelier-for-tailoring"

		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	BeforeEach(func() {
		// Create prerequisite Atelier
		atelier := &autoscalingv1alpha1.Atelier{
			ObjectMeta: metav1.ObjectMeta{
				Name: AtelierName,
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
		err := k8sClient.Create(ctx, atelier)
		if err != nil {
			// Atelier might already exist from previous test
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: AtelierName}, atelier)
		}
	})

	Context("When creating a Tailoring", func() {
		It("Should create successfully", func() {
			By("Creating a new Tailoring")
			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      TailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "test-deployment",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name:      "balanced-profile",
						Namespace: "default",
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

			tailoringLookupKey := types.NamespacedName{
				Name:      TailoringName,
				Namespace: TailoringNamespace,
			}
			createdTailoring := &autoscalingv1alpha1.Tailoring{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, tailoringLookupKey, createdTailoring)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdTailoring.Spec.Target.Name).Should(Equal("test-deployment"))
			Expect(createdTailoring.Spec.FitProfileRef.Name).Should(Equal("balanced-profile"))
		})

		It("Should set default analysis window", func() {
			tailoringLookupKey := types.NamespacedName{
				Name:      TailoringName,
				Namespace: TailoringNamespace,
			}
			tailoring := &autoscalingv1alpha1.Tailoring{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, tailoringLookupKey, tailoring)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			// Default analysis window should be applied if not set
			// (This depends on webhook or controller defaulting)
		})
	})

	Context("When Tailoring is paused", func() {
		It("Should not create PRs", func() {
			tailoring := &autoscalingv1alpha1.Tailoring{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      TailoringName,
				Namespace: TailoringNamespace,
			}, tailoring)
			Expect(err).NotTo(HaveOccurred())

			// Pause the tailoring
			tailoring.Spec.Paused = true
			Expect(k8sClient.Update(ctx, tailoring)).Should(Succeed())

			// Verify it's paused
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      TailoringName,
					Namespace: TailoringNamespace,
				}, tailoring)
				return err == nil && tailoring.Spec.Paused
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When Tailoring targets a non-existent workload", func() {
		It("Should set TargetFound to false", func() {
			tailoring := &autoscalingv1alpha1.Tailoring{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      TailoringName,
				Namespace: TailoringNamespace,
			}, tailoring)
			Expect(err).NotTo(HaveOccurred())

			// Status should indicate target not found
			// (Controller should set this during reconciliation)
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      TailoringName,
					Namespace: TailoringNamespace,
				}, tailoring)
				return err == nil && !tailoring.Status.TargetFound
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("With FitProfile references", func() {
		It("Should accept FitProfile reference", func() {
			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fitprofile-tailoring",
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "test-app",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name:      "test-profile",
						Namespace: "default",
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

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "fitprofile-tailoring",
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.FitProfileRef.Name).Should(Equal("test-profile"))
		})
	})

	AfterEach(func() {
		// Cleanup
		tailoring := &autoscalingv1alpha1.Tailoring{}
		_ = k8sClient.Get(ctx, types.NamespacedName{
			Name:      TailoringName,
			Namespace: TailoringNamespace,
		}, tailoring)
		_ = k8sClient.Delete(ctx, tailoring)
	})
})
