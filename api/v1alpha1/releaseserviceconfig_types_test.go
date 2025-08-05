/*
Copyright 2022.

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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("ReleaseServiceConfig type", func() {
	When("IsPipelineOverridden method is called", func() {
		var releaseServiceConfig *ReleaseServiceConfig

		BeforeEach(func() {
			releaseServiceConfig = &ReleaseServiceConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "config",
					Namespace: "default",
				},
			}
		})

		It("should return false if the resource is not overridden", func() {
			Expect(releaseServiceConfig.IsPipelineOverridden("foo", "bar", "baz")).To(BeFalse())
		})

		It("should return true if the resource is overridden", func() {
			releaseServiceConfig.Spec.EmptyDirOverrides = []EmptyDirOverrides{
				{"foo", "bar", "baz"},
			}
			Expect(releaseServiceConfig.IsPipelineOverridden("foo", "bar", "baz")).To(BeTrue())
		})

		It("should return true if the resource is overridden using a regex expression in the url field", func() {
			releaseServiceConfig.Spec.EmptyDirOverrides = []EmptyDirOverrides{
				{".*", "bar", "baz"},
			}
			Expect(releaseServiceConfig.IsPipelineOverridden("foo", "bar", "baz")).To(BeTrue())
		})

		It("should return true if the resource is overridden using a regex expression in the revision field", func() {
			releaseServiceConfig.Spec.EmptyDirOverrides = []EmptyDirOverrides{
				{"foo", ".*", "baz"},
			}
			Expect(releaseServiceConfig.IsPipelineOverridden("foo", "bar", "baz")).To(BeTrue())
		})

		It("should return false if the resource is overridden using a regex expression in the pathInRepo field", func() {
			releaseServiceConfig.Spec.EmptyDirOverrides = []EmptyDirOverrides{
				{"foo", "bar", ".*"},
			}
			Expect(releaseServiceConfig.IsPipelineOverridden("foo", "bar", "baz")).To(BeFalse())
		})
	})

	When("OciStorage configuration is used", func() {
		var releaseServiceConfig *ReleaseServiceConfig

		BeforeEach(func() {
			releaseServiceConfig = &ReleaseServiceConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "config",
					Namespace: "default",
				},
			}
		})

		It("should have default OCI storage location", func() {
			releaseServiceConfig.Spec.OciStorage = OciStorage{
				Default: "registry.example.com/default-storage",
			}
			Expect(releaseServiceConfig.Spec.OciStorage.Default).To(Equal("registry.example.com/default-storage"))
		})

		It("should have OCI storage overrides for specific pipelines", func() {
			releaseServiceConfig.Spec.OciStorage = OciStorage{
				Default: "registry.example.com/default-storage",
				OciStorageOverrides: []OciStorageOverrides{
					{
						PipelineName: "my-pipeline",
						OciStorage:   "registry.example.com/my-pipeline-storage",
					},
					{
						PipelineName: "another-pipeline",
						OciStorage:   "registry.example.com/another-pipeline-storage",
					},
				},
			}

			Expect(releaseServiceConfig.Spec.OciStorage.Default).To(Equal("registry.example.com/default-storage"))
			Expect(len(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides)).To(Equal(2))
			Expect(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides[0].PipelineName).To(Equal("my-pipeline"))
			Expect(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides[0].OciStorage).To(Equal("registry.example.com/my-pipeline-storage"))
			Expect(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides[1].PipelineName).To(Equal("another-pipeline"))
			Expect(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides[1].OciStorage).To(Equal("registry.example.com/another-pipeline-storage"))
		})

		It("should handle empty OCI storage overrides", func() {
			releaseServiceConfig.Spec.OciStorage = OciStorage{
				Default: "registry.example.com/default-storage",
			}

			Expect(releaseServiceConfig.Spec.OciStorage.Default).To(Equal("registry.example.com/default-storage"))
			Expect(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides).To(BeNil())
		})

		It("should handle OCI storage with only overrides and no default", func() {
			releaseServiceConfig.Spec.OciStorage = OciStorage{
				OciStorageOverrides: []OciStorageOverrides{
					{
						PipelineName: "my-pipeline",
						OciStorage:   "registry.example.com/my-pipeline-storage",
					},
				},
			}

			Expect(releaseServiceConfig.Spec.OciStorage.Default).To(Equal(""))
			Expect(len(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides)).To(Equal(1))
		})

		It("should handle empty OCI storage configuration", func() {
			Expect(releaseServiceConfig.Spec.OciStorage.Default).To(Equal(""))
			Expect(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides).To(BeNil())
		})

		It("should validate OciStorageOverrides structure", func() {
			override := OciStorageOverrides{
				PipelineName: "test-pipeline",
				OciStorage:   "registry.example.com/test-storage",
			}

			Expect(override.PipelineName).To(Equal("test-pipeline"))
			Expect(override.OciStorage).To(Equal("registry.example.com/test-storage"))
		})

		It("should handle multiple OCI storage overrides with different pipeline names", func() {
			releaseServiceConfig.Spec.OciStorage = OciStorage{
				Default: "registry.example.com/default-storage",
				OciStorageOverrides: []OciStorageOverrides{
					{
						PipelineName: "pipeline-1",
						OciStorage:   "registry.example.com/pipeline-1-storage",
					},
					{
						PipelineName: "pipeline-2",
						OciStorage:   "registry.example.com/pipeline-2-storage",
					},
					{
						PipelineName: "pipeline-3",
						OciStorage:   "registry.example.com/pipeline-3-storage",
					},
				},
			}

			Expect(len(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides)).To(Equal(3))

			// Verify all pipeline names are unique
			pipelineNames := make(map[string]bool)
			for _, override := range releaseServiceConfig.Spec.OciStorage.OciStorageOverrides {
				Expect(pipelineNames[override.PipelineName]).To(BeFalse())
				pipelineNames[override.PipelineName] = true
			}
		})

		It("should handle OCI storage with special characters in registry URLs", func() {
			releaseServiceConfig.Spec.OciStorage = OciStorage{
				Default: "my-registry.com:5000/default-storage",
				OciStorageOverrides: []OciStorageOverrides{
					{
						PipelineName: "my-pipeline",
						OciStorage:   "my-registry.com:5000/my-pipeline-storage",
					},
				},
			}

			Expect(releaseServiceConfig.Spec.OciStorage.Default).To(Equal("my-registry.com:5000/default-storage"))
			Expect(releaseServiceConfig.Spec.OciStorage.OciStorageOverrides[0].OciStorage).To(Equal("my-registry.com:5000/my-pipeline-storage"))
		})
	})
})
