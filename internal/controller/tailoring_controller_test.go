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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

var _ = Describe("Tailoring Controller", func() {
	const (
		TailoringNamespace = "default"

		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	// Helper function to create a unique name
	var uniqueName = func(prefix string) string {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}

	// Helper function to create an Atelier for tests
	var createTestAtelier = func(name string) *autoscalingv1alpha1.Atelier {
		atelier := &autoscalingv1alpha1.Atelier{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
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
			// Atelier might already exist
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: name}, atelier)
		}
		return atelier
	}

	// Helper function to create a Tailoring
	var createTestTailoring = func(name, atelierName string) *autoscalingv1alpha1.Tailoring {
		tailoring := &autoscalingv1alpha1.Tailoring{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
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
		return tailoring
	}

	// Helper function to cleanup a Tailoring
	var cleanupTailoring = func(name string) {
		tailoring := &autoscalingv1alpha1.Tailoring{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: TailoringNamespace,
		}, tailoring)
		if err == nil {
			_ = k8sClient.Delete(ctx, tailoring)
		}
	}

	// Helper function to cleanup an Atelier
	var cleanupAtelier = func(name string) {
		atelier := &autoscalingv1alpha1.Atelier{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, atelier)
		if err == nil {
			_ = k8sClient.Delete(ctx, atelier)
		}
	}

	Context("When creating a Tailoring", func() {
		var atelierName, tailoringName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier")
			tailoringName = uniqueName("test-tailoring")
			createTestAtelier(atelierName)
		})

		AfterEach(func() {
			cleanupTailoring(tailoringName)
			cleanupAtelier(atelierName)
		})

		It("Should create successfully", func() {
			By("Creating a new Tailoring")
			createTestTailoring(tailoringName, atelierName)

			tailoringLookupKey := types.NamespacedName{
				Name:      tailoringName,
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

		It("Should set TargetFound to false when workload doesn't exist", func() {
			By("Creating a Tailoring targeting non-existent workload")
			createTestTailoring(tailoringName, atelierName)

			tailoringLookupKey := types.NamespacedName{
				Name:      tailoringName,
				Namespace: TailoringNamespace,
			}
			tailoring := &autoscalingv1alpha1.Tailoring{}

			// Wait for the controller to reconcile and set TargetFound status
			Eventually(func() bool {
				err := k8sClient.Get(ctx, tailoringLookupKey, tailoring)
				if err != nil {
					return false
				}
				// The controller should set TargetFound to false since the deployment doesn't exist
				return !tailoring.Status.TargetFound
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When creating Tailoring with different target kinds", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-kinds")
			createTestAtelier(atelierName)
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should accept Deployment target", func() {
			tailoringName := uniqueName("test-tailoring-deployment")
			defer cleanupTailoring(tailoringName)

			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "my-deployment",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name: "balanced-profile",
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:       autoscalingv1alpha1.WriteBackTypeRaw,
						Repository: "org/repo",
						Path:       "apps/deployment.yaml",
						Branch:     "main",
					},
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.Target.Kind).Should(Equal("Deployment"))
		})

		It("Should accept StatefulSet target", func() {
			tailoringName := uniqueName("test-tailoring-sts")
			defer cleanupTailoring(tailoringName)

			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "StatefulSet",
						Name: "my-statefulset",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name: "balanced-profile",
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:       autoscalingv1alpha1.WriteBackTypeRaw,
						Repository: "org/repo",
						Path:       "apps/statefulset.yaml",
						Branch:     "main",
					},
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.Target.Kind).Should(Equal("StatefulSet"))
		})

		It("Should accept DaemonSet target", func() {
			tailoringName := uniqueName("test-tailoring-ds")
			defer cleanupTailoring(tailoringName)

			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "DaemonSet",
						Name: "my-daemonset",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name: "balanced-profile",
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:       autoscalingv1alpha1.WriteBackTypeRaw,
						Repository: "org/repo",
						Path:       "apps/daemonset.yaml",
						Branch:     "main",
					},
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.Target.Kind).Should(Equal("DaemonSet"))
		})
	})

	Context("When creating Tailoring with different WriteBack types", func() {
		var atelierName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-wb")
			createTestAtelier(atelierName)
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should accept raw WriteBack type", func() {
			tailoringName := uniqueName("test-tailoring-raw")
			defer cleanupTailoring(tailoringName)

			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "my-app",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name: "balanced-profile",
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:       autoscalingv1alpha1.WriteBackTypeRaw,
						Repository: "org/repo",
						Path:       "manifests/deployment.yaml",
						Branch:     "main",
					},
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.WriteBack.Type).Should(Equal(autoscalingv1alpha1.WriteBackTypeRaw))
		})

		It("Should accept Helm WriteBack type with value path", func() {
			tailoringName := uniqueName("test-tailoring-helm")
			defer cleanupTailoring(tailoringName)

			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "my-helm-app",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name: "balanced-profile",
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:          autoscalingv1alpha1.WriteBackTypeHelm,
						Repository:    "org/helm-charts",
						Path:          "charts/my-app/values.yaml",
						Branch:        "main",
						HelmValuePath: "resources",
					},
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.WriteBack.Type).Should(Equal(autoscalingv1alpha1.WriteBackTypeHelm))
			Expect(created.Spec.WriteBack.HelmValuePath).Should(Equal("resources"))
		})

		It("Should accept Kustomize WriteBack type", func() {
			tailoringName := uniqueName("test-tailoring-kustomize")
			defer cleanupTailoring(tailoringName)

			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "my-kustomize-app",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name: "balanced-profile",
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:       autoscalingv1alpha1.WriteBackTypeKustomize,
						Repository: "org/kustomize-repo",
						Path:       "overlays/prod/kustomization.yaml",
						Branch:     "main",
					},
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.WriteBack.Type).Should(Equal(autoscalingv1alpha1.WriteBackTypeKustomize))
		})
	})

	Context("When Tailoring has exclude containers", func() {
		var atelierName, tailoringName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-exclude")
			tailoringName = uniqueName("test-tailoring-exclude")
			createTestAtelier(atelierName)
		})

		AfterEach(func() {
			cleanupTailoring(tailoringName)
			cleanupAtelier(atelierName)
		})

		It("Should store excluded containers list", func() {
			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "multi-container-app",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name: "balanced-profile",
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:       autoscalingv1alpha1.WriteBackTypeRaw,
						Repository: "org/repo",
						Path:       "apps/deployment.yaml",
						Branch:     "main",
					},
					ExcludeContainers: []string{"sidecar", "init-container", "log-collector"},
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.ExcludeContainers).Should(HaveLen(3))
			Expect(created.Spec.ExcludeContainers).Should(ContainElements("sidecar", "init-container", "log-collector"))
		})
	})

	Context("When Tailoring has custom analysis window", func() {
		var atelierName, tailoringName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-window")
			tailoringName = uniqueName("test-tailoring-window")
			createTestAtelier(atelierName)
		})

		AfterEach(func() {
			cleanupTailoring(tailoringName)
			cleanupAtelier(atelierName)
		})

		It("Should store custom analysis window", func() {
			window := metav1.Duration{Duration: 14 * 24 * time.Hour} // 14 days

			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "long-running-app",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name: "balanced-profile",
					},
					WriteBack: autoscalingv1alpha1.WriteBackConfig{
						Type:       autoscalingv1alpha1.WriteBackTypeRaw,
						Repository: "org/repo",
						Path:       "apps/deployment.yaml",
						Branch:     "main",
					},
					AnalysisWindow: &window,
				},
			}
			Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())

			created := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.AnalysisWindow).ShouldNot(BeNil())
			Expect(created.Spec.AnalysisWindow.Duration).Should(Equal(14 * 24 * time.Hour))
		})
	})

	Context("When Tailoring is paused", func() {
		var atelierName, tailoringName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-paused")
			tailoringName = uniqueName("test-tailoring-paused")
			createTestAtelier(atelierName)
		})

		AfterEach(func() {
			cleanupTailoring(tailoringName)
			cleanupAtelier(atelierName)
		})

		It("Should respect paused flag", func() {
			By("Creating a new paused Tailoring")
			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Paused: true,
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
				Name:      tailoringName,
				Namespace: TailoringNamespace,
			}
			createdTailoring := &autoscalingv1alpha1.Tailoring{}

			// Verify the Tailoring is created with paused flag
			Eventually(func() bool {
				err := k8sClient.Get(ctx, tailoringLookupKey, createdTailoring)
				return err == nil && createdTailoring.Spec.Paused
			}, timeout, interval).Should(BeTrue())

			Expect(createdTailoring.Spec.Paused).Should(BeTrue())
		})
	})

	Context("With FitProfile references", func() {
		var atelierName, tailoringName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-fp")
			tailoringName = uniqueName("fitprofile-tailoring")
			createTestAtelier(atelierName)
		})

		AfterEach(func() {
			cleanupTailoring(tailoringName)
			cleanupAtelier(atelierName)
		})

		It("Should accept FitProfile reference", func() {
			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "test-app",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name:      "custom-profile",
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
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.FitProfileRef.Name).Should(Equal("custom-profile"))
		})

		It("Should accept cross-namespace FitProfile reference", func() {
			tailoring := &autoscalingv1alpha1.Tailoring{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				},
				Spec: autoscalingv1alpha1.TailoringSpec{
					Target: autoscalingv1alpha1.TargetRef{
						Kind: "Deployment",
						Name: "test-app",
					},
					FitProfileRef: autoscalingv1alpha1.FitProfileRef{
						Name:      "shared-profile",
						Namespace: "sartor-system",
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
					Name:      tailoringName,
					Namespace: TailoringNamespace,
				}, created)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(created.Spec.FitProfileRef.Name).Should(Equal("shared-profile"))
			Expect(created.Spec.FitProfileRef.Namespace).Should(Equal("sartor-system"))
		})
	})

	Context("When deleting a Tailoring", func() {
		var atelierName, tailoringName string

		BeforeEach(func() {
			atelierName = uniqueName("test-atelier-del")
			tailoringName = uniqueName("test-tailoring-del")
			createTestAtelier(atelierName)
		})

		AfterEach(func() {
			cleanupAtelier(atelierName)
		})

		It("Should delete successfully", func() {
			By("Creating a Tailoring")
			createTestTailoring(tailoringName, atelierName)

			tailoringLookupKey := types.NamespacedName{
				Name:      tailoringName,
				Namespace: TailoringNamespace,
			}

			// Wait for creation
			tailoring := &autoscalingv1alpha1.Tailoring{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, tailoringLookupKey, tailoring)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			By("Deleting the Tailoring")
			Expect(k8sClient.Delete(ctx, tailoring)).Should(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, tailoringLookupKey, tailoring)
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})
})
