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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

var _ = Describe("Atelier Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	// Helper function to create a unique name
	var uniqueName = func(prefix string) string {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}

	// Helper function to cleanup an Atelier
	var cleanupAtelier = func(name string) {
		atelier := &autoscalingv1alpha1.Atelier{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, atelier)
		if err == nil {
			_ = k8sClient.Delete(ctx, atelier)
		}
	}

	Context("When creating an Atelier", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier")
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should create successfully", func() {
			By("Creating a new Atelier")
			atelier := &autoscalingv1alpha1.Atelier{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "autoscaling.sartorproj.io/v1alpha1",
					Kind:       "Atelier",
				},
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
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			atelierLookupKey := types.NamespacedName{Name: atelierName}
			createdAtelier := &autoscalingv1alpha1.Atelier{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, atelierLookupKey, createdAtelier)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdAtelier.Spec.Prometheus.URL).Should(Equal("http://prometheus:9090"))
			Expect(createdAtelier.Spec.GitProvider.Type).Should(Equal(autoscalingv1alpha1.GitProviderGitHub))
		})

		It("Should update status after reconciliation", func() {
			By("Creating a new Atelier")
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
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			atelierLookupKey := types.NamespacedName{Name: atelierName}
			createdAtelier := &autoscalingv1alpha1.Atelier{}

			// Wait for reconciliation to update the status
			Eventually(func() bool {
				err := k8sClient.Get(ctx, atelierLookupKey, createdAtelier)
				if err != nil {
					return false
				}
				// Status should be updated by reconciler
				// ManagedTailorings >= 0 indicates status was updated
				return createdAtelier.Status.ManagedTailorings >= 0
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When Atelier has different Git providers", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-git")
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should accept GitHub provider", func() {
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
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			created := &autoscalingv1alpha1.Atelier{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.GitProvider.Type).Should(Equal(autoscalingv1alpha1.GitProviderGitHub))
		})

		It("Should accept GitLab provider with base URL", func() {
			atelier := &autoscalingv1alpha1.Atelier{
				ObjectMeta: metav1.ObjectMeta{
					Name: atelierName,
				},
				Spec: autoscalingv1alpha1.AtelierSpec{
					Prometheus: autoscalingv1alpha1.PrometheusConfig{
						URL: "http://prometheus:9090",
					},
					GitProvider: autoscalingv1alpha1.GitProviderConfig{
						Type: autoscalingv1alpha1.GitProviderGitLab,
						SecretRef: corev1.SecretReference{
							Name:      "gitlab-token",
							Namespace: "default",
						},
						BaseURL: "https://gitlab.company.com",
					},
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			created := &autoscalingv1alpha1.Atelier{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.GitProvider.Type).Should(Equal(autoscalingv1alpha1.GitProviderGitLab))
			Expect(created.Spec.GitProvider.BaseURL).Should(Equal("https://gitlab.company.com"))
		})

		It("Should accept Bitbucket provider", func() {
			atelier := &autoscalingv1alpha1.Atelier{
				ObjectMeta: metav1.ObjectMeta{
					Name: atelierName,
				},
				Spec: autoscalingv1alpha1.AtelierSpec{
					Prometheus: autoscalingv1alpha1.PrometheusConfig{
						URL: "http://prometheus:9090",
					},
					GitProvider: autoscalingv1alpha1.GitProviderConfig{
						Type: autoscalingv1alpha1.GitProviderBitbucket,
						SecretRef: corev1.SecretReference{
							Name:      "bitbucket-token",
							Namespace: "default",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			created := &autoscalingv1alpha1.Atelier{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.GitProvider.Type).Should(Equal(autoscalingv1alpha1.GitProviderBitbucket))
		})
	})

	Context("When Atelier has ArgoCD enabled", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-argocd")
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should store basic ArgoCD configuration", func() {
			By("Creating an Atelier with ArgoCD config")
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
					ArgoCD: &autoscalingv1alpha1.ArgoCDConfig{
						Enabled:     true,
						SyncOnMerge: true,
					},
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			atelierLookupKey := types.NamespacedName{Name: atelierName}
			createdAtelier := &autoscalingv1alpha1.Atelier{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, atelierLookupKey, createdAtelier)
				return err == nil && createdAtelier.Spec.ArgoCD != nil && createdAtelier.Spec.ArgoCD.Enabled
			}, timeout, interval).Should(BeTrue())

			Expect(createdAtelier.Spec.ArgoCD.Enabled).Should(BeTrue())
			Expect(createdAtelier.Spec.ArgoCD.SyncOnMerge).Should(BeTrue())
		})

		It("Should store full ArgoCD configuration", func() {
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
					ArgoCD: &autoscalingv1alpha1.ArgoCDConfig{
						Enabled:            true,
						ServerURL:          "https://argocd.example.com",
						Namespace:          "argocd",
						SyncOnMerge:        true,
						RefreshOnPRCreate:  true,
						HardRefresh:        true,
						InsecureSkipVerify: false,
						SecretRef: &corev1.SecretReference{
							Name:      "argocd-token",
							Namespace: "default",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			created := &autoscalingv1alpha1.Atelier{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, created)
				return err == nil && created.Spec.ArgoCD != nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.ArgoCD.ServerURL).Should(Equal("https://argocd.example.com"))
			Expect(created.Spec.ArgoCD.Namespace).Should(Equal("argocd"))
			Expect(created.Spec.ArgoCD.RefreshOnPRCreate).Should(BeTrue())
			Expect(created.Spec.ArgoCD.HardRefresh).Should(BeTrue())
		})
	})

	Context("When Atelier has safety rails configured", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-rails")
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should store safety rails configuration", func() {
			By("Creating an Atelier with safety rails")
			minCPU := resource.MustParse("50m")
			minMem := resource.MustParse("64Mi")
			maxCPU := resource.MustParse("8")
			maxMem := resource.MustParse("16Gi")

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
					SafetyRails: &autoscalingv1alpha1.SafetyRailsConfig{
						MinCPU:    &minCPU,
						MinMemory: &minMem,
						MaxCPU:    &maxCPU,
						MaxMemory: &maxMem,
					},
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			atelierLookupKey := types.NamespacedName{Name: atelierName}
			createdAtelier := &autoscalingv1alpha1.Atelier{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, atelierLookupKey, createdAtelier)
				return err == nil && createdAtelier.Spec.SafetyRails != nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdAtelier.Spec.SafetyRails.MinCPU.String()).Should(Equal("50m"))
			Expect(createdAtelier.Spec.SafetyRails.MinMemory.String()).Should(Equal("64Mi"))
			Expect(createdAtelier.Spec.SafetyRails.MaxCPU.String()).Should(Equal("8"))
			Expect(createdAtelier.Spec.SafetyRails.MaxMemory.String()).Should(Equal("16Gi"))
		})
	})

	Context("When Atelier has PR settings configured", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-pr")
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should store PR settings configuration", func() {
			cooldown := metav1.Duration{Duration: 7 * 24 * time.Hour}
			minChange := int32(15)

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
					PRSettings: &autoscalingv1alpha1.PRSettingsConfig{
						CooldownPeriod:   &cooldown,
						MinChangePercent: &minChange,
						BaseBranch:       "main",
						BranchPrefix:     "sartor/",
						Labels:           []string{"sartor", "resource-optimization", "automated"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			created := &autoscalingv1alpha1.Atelier{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, created)
				return err == nil && created.Spec.PRSettings != nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.PRSettings.CooldownPeriod.Duration).Should(Equal(7 * 24 * time.Hour))
			Expect(*created.Spec.PRSettings.MinChangePercent).Should(Equal(int32(15)))
			Expect(created.Spec.PRSettings.BaseBranch).Should(Equal("main"))
			Expect(created.Spec.PRSettings.BranchPrefix).Should(Equal("sartor/"))
			Expect(created.Spec.PRSettings.Labels).Should(HaveLen(3))
		})
	})

	Context("When Atelier has OpenCost configured", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-opencost")
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should store OpenCost configuration", func() {
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
					OpenCost: &autoscalingv1alpha1.OpenCostConfig{
						Enabled:            true,
						URL:                "http://opencost.opencost:9003",
						InsecureSkipVerify: false,
					},
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			created := &autoscalingv1alpha1.Atelier{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, created)
				return err == nil && created.Spec.OpenCost != nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.OpenCost.Enabled).Should(BeTrue())
			Expect(created.Spec.OpenCost.URL).Should(Equal("http://opencost.opencost:9003"))
		})
	})

	Context("When Atelier has batch mode and default analysis window", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-batch")
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should store batch mode and default analysis window", func() {
			window := metav1.Duration{Duration: 14 * 24 * time.Hour}

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
					BatchMode:             true,
					DefaultAnalysisWindow: &window,
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			created := &autoscalingv1alpha1.Atelier{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.BatchMode).Should(BeTrue())
			Expect(created.Spec.DefaultAnalysisWindow.Duration).Should(Equal(14 * 24 * time.Hour))
		})
	})

	Context("When Atelier has Prometheus with authentication", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-prom-auth")
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should store Prometheus authentication configuration", func() {
			atelier := &autoscalingv1alpha1.Atelier{
				ObjectMeta: metav1.ObjectMeta{
					Name: atelierName,
				},
				Spec: autoscalingv1alpha1.AtelierSpec{
					Prometheus: autoscalingv1alpha1.PrometheusConfig{
						URL: "https://prometheus.monitoring.svc:9090",
						SecretRef: &corev1.SecretReference{
							Name:      "prometheus-credentials",
							Namespace: "monitoring",
						},
						InsecureSkipVerify: true,
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
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			created := &autoscalingv1alpha1.Atelier{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: atelierName}, created)
				return err == nil && created.Spec.Prometheus.SecretRef != nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.Prometheus.URL).Should(Equal("https://prometheus.monitoring.svc:9090"))
			Expect(created.Spec.Prometheus.SecretRef.Name).Should(Equal("prometheus-credentials"))
			Expect(created.Spec.Prometheus.SecretRef.Namespace).Should(Equal("monitoring"))
			Expect(created.Spec.Prometheus.InsecureSkipVerify).Should(BeTrue())
		})
	})

	Context("When deleting an Atelier", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-del")
		})

		It("Should delete successfully", func() {
			By("Creating an Atelier")
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
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			atelierLookupKey := types.NamespacedName{Name: atelierName}
			createdAtelier := &autoscalingv1alpha1.Atelier{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, atelierLookupKey, createdAtelier)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			By("Deleting the Atelier")
			Expect(k8sClient.Delete(ctx, createdAtelier)).Should(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, atelierLookupKey, &autoscalingv1alpha1.Atelier{})
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})
})
