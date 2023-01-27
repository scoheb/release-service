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

package release

import (
	"context"
	"fmt"
	"strings"
	"time"

	ecapiv1alpha1 "github.com/hacbs-contract/enterprise-contract-controller/api/v1alpha1"
	applicationapiv1alpha1 "github.com/redhat-appstudio/application-api/api/v1alpha1"

	"github.com/redhat-appstudio/release-service/gitops"
	"github.com/redhat-appstudio/release-service/syncer"
	ctrl "sigs.k8s.io/controller-runtime"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/go-logr/logr"
	libhandler "github.com/operator-framework/operator-lib/handler"
	"github.com/redhat-appstudio/operator-goodies/reconciler"
	"github.com/redhat-appstudio/release-service/api/v1alpha1"
	"github.com/redhat-appstudio/release-service/tekton"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Adapter holds the objects needed to reconcile a Release.
type Adapter struct {
	release *v1alpha1.Release
	logger  logr.Logger
	client  client.Client
	context context.Context
	syncer  *syncer.Syncer
}

// finalizerName is the finalizer name to be added to the Releases
const finalizerName string = "appstudio.redhat.com/release-finalizer"

// NewAdapter creates and returns an Adapter instance.
func NewAdapter(release *v1alpha1.Release, logger logr.Logger, client client.Client, context context.Context) *Adapter {
	return &Adapter{
		release: release,
		logger:  logger,
		client:  client,
		context: context,
		syncer:  syncer.NewSyncerWithContext(client, logger, context),
	}
}

// EnsureFinalizersAreCalled is an operation that will ensure that finalizers are called whenever the Release being
// processed is marked for deletion. Once finalizers get called, the finalizer will be removed and the Release will go
// back to the queue, so it gets deleted. If a finalizer function fails its execution or a finalizer fails to be removed,
// the Release will be requeued with the error attached.
func (a *Adapter) EnsureFinalizersAreCalled() (reconciler.OperationResult, error) {
	// Check if the Release is marked for deletion and continue processing other operations otherwise
	if a.release.GetDeletionTimestamp() == nil {
		return reconciler.ContinueProcessing()
	}

	if controllerutil.ContainsFinalizer(a.release, finalizerName) {
		if err := a.finalizeRelease(); err != nil {
			return reconciler.RequeueWithError(err)
		}

		patch := client.MergeFrom(a.release.DeepCopy())
		controllerutil.RemoveFinalizer(a.release, finalizerName)
		err := a.client.Patch(a.context, a.release, patch)
		if err != nil {
			return reconciler.RequeueWithError(err)
		}
	}

	// Requeue the release again so it gets deleted and other operations are not executed
	return reconciler.Requeue()
}

// EnsureFinalizerIsAdded is an operation that will ensure that the Release being processed contains a finalizer.
func (a *Adapter) EnsureFinalizerIsAdded() (reconciler.OperationResult, error) {
	var finalizerFound bool
	for _, finalizer := range a.release.GetFinalizers() {
		if finalizer == finalizerName {
			finalizerFound = true
		}
	}

	if !finalizerFound {
		a.logger.Info("Adding Finalizer to the Release")
		patch := client.MergeFrom(a.release.DeepCopy())
		controllerutil.AddFinalizer(a.release, finalizerName)
		err := a.client.Patch(a.context, a.release, patch)

		return reconciler.RequeueOnErrorOrContinue(err)
	}

	return reconciler.ContinueProcessing()
}

// EnsureReleasePlanAdmissionEnabled is an operation that will ensure that the ReleasePlanAdmission is enabled.
// If it is not, no further operations will occur for this Release.
func (a *Adapter) EnsureReleasePlanAdmissionEnabled() (reconciler.OperationResult, error) {
	_, err := a.getActiveReleasePlanAdmission()
	if err != nil && strings.Contains(err.Error(), "multiple ReleasePlanAdmissions found") {
		patch := client.MergeFrom(a.release.DeepCopy())
		a.release.MarkInvalid(v1alpha1.ReleaseReasonValidationError, err.Error())
		return reconciler.RequeueOnErrorOrStop(a.client.Status().Patch(a.context, a.release, patch))
	}
	if err != nil && strings.Contains(err.Error(), "auto-release label set to false") {
		patch := client.MergeFrom(a.release.DeepCopy())
		a.release.MarkInvalid(v1alpha1.ReleaseReasonTargetDisabledError, err.Error())
		return reconciler.RequeueOnErrorOrStop(a.client.Status().Patch(a.context, a.release, patch))
	}
	return reconciler.ContinueProcessing()
}

// EnsureReleasePipelineRunExists is an operation that will ensure that a release PipelineRun associated to the Release
// being processed exists. Otherwise, it will create a new release PipelineRun.
func (a *Adapter) EnsureReleasePipelineRunExists() (reconciler.OperationResult, error) {
	pipelineRun, err := a.getReleasePipelineRun()
	if err != nil && !errors.IsNotFound(err) {
		return reconciler.RequeueWithError(err)
	}

	var (
		releasePlanAdmission *v1alpha1.ReleasePlanAdmission
		releaseStrategy      *v1alpha1.ReleaseStrategy
	)

	if pipelineRun == nil {
		releasePlanAdmission, err = a.getActiveReleasePlanAdmission()
		if err != nil {
			patch := client.MergeFrom(a.release.DeepCopy())
			a.release.MarkInvalid(v1alpha1.ReleaseReasonReleasePlanValidationError, err.Error())
			return reconciler.RequeueOnErrorOrStop(a.client.Status().Patch(a.context, a.release, patch))
		}
		releaseStrategy, err = a.getReleaseStrategy(releasePlanAdmission)
		if err != nil {
			patch := client.MergeFrom(a.release.DeepCopy())
			a.release.MarkInvalid(v1alpha1.ReleaseReasonValidationError, err.Error())
			return reconciler.RequeueOnErrorOrStop(a.client.Status().Patch(a.context, a.release, patch))
		}
		enterpriseContractPolicy, err := a.getEnterpriseContractPolicy(releaseStrategy)
		if err != nil {
			patch := client.MergeFrom(a.release.DeepCopy())
			a.release.MarkInvalid(v1alpha1.ReleaseReasonValidationError, err.Error())
			return reconciler.RequeueOnErrorOrStop(a.client.Status().Patch(a.context, a.release, patch))
		}

		snapshot, err := a.getSnapshot()
		if err != nil {
			patch := client.MergeFrom(a.release.DeepCopy())
			a.release.MarkInvalid(v1alpha1.ReleaseReasonValidationError, err.Error())
			return reconciler.RequeueOnErrorOrStop(a.client.Status().Patch(a.context, a.release, patch))
		}

		pipelineRun, err = a.createReleasePipelineRun(releaseStrategy, enterpriseContractPolicy, snapshot)
		if err != nil {
			return reconciler.RequeueWithError(err)
		}

		a.logger.Info("Created release PipelineRun",
			"PipelineRun.Name", pipelineRun.Name, "PipelineRun.Namespace", pipelineRun.Namespace)
	}

	return reconciler.RequeueOnErrorOrContinue(a.registerReleaseStatusData(pipelineRun, releaseStrategy))
}

// EnsureReleasePipelineStatusIsTracked is an operation that will ensure that the release PipelineRun status is tracked
// in the Release being processed.
func (a *Adapter) EnsureReleasePipelineStatusIsTracked() (reconciler.OperationResult, error) {
	if !a.release.HasStarted() || a.release.IsDone() {
		return reconciler.ContinueProcessing()
	}

	pipelineRun, err := a.getReleasePipelineRun()
	if err != nil {
		return reconciler.RequeueWithError(err)
	}
	if pipelineRun != nil {
		return reconciler.RequeueOnErrorOrContinue(a.registerReleasePipelineRunStatus(pipelineRun))
	}

	return reconciler.ContinueProcessing()
}

// EnsureSnapshotEnvironmentBindingExists is an operation that will ensure that a SnapshotEnvironmentBinding
// associated to the Release being processed exists. Otherwise, it will create a new one.
func (a *Adapter) EnsureSnapshotEnvironmentBindingExists() (reconciler.OperationResult, error) {
	if !a.release.HasSucceeded() || a.release.HasBeenDeployed() {
		return reconciler.ContinueProcessing()
	}

	releasePlanAdmission, err := a.getActiveReleasePlanAdmission()
	if err != nil {
		return reconciler.RequeueWithError(err)
	}

	// If no environment is set in the ReleasePlanAdmission, skip the Binding creation
	if releasePlanAdmission.Spec.Environment == "" {
		return reconciler.ContinueProcessing()
	}

	environment, err := a.getEnvironment(releasePlanAdmission)
	if err != nil {
		return reconciler.RequeueWithError(err)
	}

	// Search for an existing binding
	binding, err := a.getSnapshotEnvironmentBinding(environment, releasePlanAdmission)
	if err != nil && !errors.IsNotFound(err) {
		return reconciler.RequeueWithError(err)
	}

	if binding == nil {
		err = a.syncResources()
		if err != nil {
			return reconciler.RequeueWithError(err)
		}

		patch := client.MergeFrom(a.release.DeepCopy())

		binding, err := a.createSnapshotEnvironmentBinding(environment, releasePlanAdmission)
		if err != nil {
			return reconciler.RequeueWithError(err)
		}

		a.logger.Info("Created SnapshotEnvironmentBinding",
			"SnapshotEnvironmentBinding.Name", binding.Name, "SnapshotEnvironmentBinding.Namespace", binding.Namespace)

		a.release.Status.SnapshotEnvironmentBinding = fmt.Sprintf("%s%c%s", binding.Namespace, types.Separator, binding.Name)

		return reconciler.RequeueOnErrorOrContinue(a.client.Status().Patch(a.context, a.release, patch))
	}

	return reconciler.ContinueProcessing()
}

// EnsureSnapshotEnvironmentBindingIsTracked is an operation that will ensure that the SnapshotEnvironmentBinding
// Deployment status is tracked in the Release being processed.
func (a *Adapter) EnsureSnapshotEnvironmentBindingIsTracked() (reconciler.OperationResult, error) {
	if !a.release.HasSucceeded() || a.release.Status.SnapshotEnvironmentBinding == "" || a.release.HasBeenDeployed() {
		return reconciler.ContinueProcessing()
	}

	// Search for an existing binding
	binding, err := a.getSnapshotEnvironmentBindingFromReleaseStatus()
	if err != nil {
		return reconciler.RequeueWithError(err)
	}

	return reconciler.RequeueOnErrorOrContinue(a.registerGitOpsDeploymentStatus(binding))
}

// createReleasePipelineRun creates and returns a new release PipelineRun. The new PipelineRun will include owner
// annotations, so it triggers Release reconciles whenever it changes. The Pipeline information and the parameters to it
// will be extracted from the given ReleaseStrategy. The Release's Snapshot will also be passed to the release
// PipelineRun.
func (a *Adapter) createReleasePipelineRun(releaseStrategy *v1alpha1.ReleaseStrategy,
	enterpriseContractPolicy *ecapiv1alpha1.EnterpriseContractPolicy,
	snapshot *applicationapiv1alpha1.Snapshot) (*v1beta1.PipelineRun, error) {
	pipelineRun := tekton.NewReleasePipelineRun("release-pipelinerun", releaseStrategy.Namespace).
		WithOwner(a.release).
		WithReleaseAndApplicationMetadata(a.release, snapshot.Spec.Application).
		WithReleaseStrategy(releaseStrategy).
		WithEnterpriseContractPolicy(enterpriseContractPolicy).
		WithSnapshot(snapshot).
		AsPipelineRun()

	err := a.client.Create(a.context, pipelineRun)
	if err != nil {
		return nil, err
	}

	return pipelineRun, nil
}

// createSnapshotEnvironmentBinding creates a SnapshotEnvironmentBinding for the Release being processed.
func (a *Adapter) createSnapshotEnvironmentBinding(environment *applicationapiv1alpha1.Environment,
	releasePlanAdmission *v1alpha1.ReleasePlanAdmission) (*applicationapiv1alpha1.SnapshotEnvironmentBinding, error) {
	application, components, snapshot, err := a.getSnapshotEnvironmentResources(releasePlanAdmission)
	if err != nil {
		return nil, err
	}

	binding := gitops.NewSnapshotEnvironmentBinding(components, snapshot, environment)

	// Set owner references so the binding is deleted if the application is deleted
	err = ctrl.SetControllerReference(application, binding, a.client.Scheme())
	if err != nil {
		return nil, err
	}

	// Add owner annotations so the controller can watch for status updates to the binding and track them
	// in the release
	err = libhandler.SetOwnerAnnotations(a.release, binding)
	if err != nil {
		return nil, err
	}

	return binding, a.client.Create(a.context, binding)
}

// finalizeRelease will finalize the Release being processed, removing the associated resources.
func (a *Adapter) finalizeRelease() error {
	pipelineRun, err := a.getReleasePipelineRun()
	if err != nil {
		return err
	}

	if pipelineRun != nil {
		err = a.client.Delete(a.context, pipelineRun)
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	a.logger.Info("Successfully finalized Release")

	return nil
}

// getActiveReleasePlanAdmission returns the ReleasePlanAdmission targeted by the ReleasePlan in the Release being
// processed. Only ReleasePlanAdmissions with the 'auto-release' label set to true (or missing the label, which is
// treated the same as having the label and it being set to true) will be searched for. If a matching
// ReleasePlanAdmission is not found or the List operation fails, an error will be returned.
func (a *Adapter) getActiveReleasePlanAdmission() (*v1alpha1.ReleasePlanAdmission, error) {
	releasePlan, err := a.getReleasePlan()
	if err != nil {
		return nil, err
	}

	releasePlanAdmissions := &v1alpha1.ReleasePlanAdmissionList{}
	opts := []client.ListOption{
		client.InNamespace(releasePlan.Spec.Target),
		client.MatchingFields{"spec.origin": releasePlan.Namespace},
	}

	err = a.client.List(a.context, releasePlanAdmissions, opts...)
	if err != nil {
		return nil, err
	}

	var activeReleasePlanAdmission *v1alpha1.ReleasePlanAdmission

	for i, releasePlanAdmission := range releasePlanAdmissions.Items {
		if releasePlanAdmission.Spec.Application != releasePlan.Spec.Application {
			continue
		}

		if activeReleasePlanAdmission != nil {
			return nil, fmt.Errorf("multiple ReleasePlanAdmissions found with the target (%+v) for application '%s'",
				releasePlan.Spec.Target, releasePlan.Spec.Application)
		}

		labelValue, found := releasePlanAdmission.GetLabels()[v1alpha1.AutoReleaseLabel]
		if found && labelValue == "false" {
			return nil, fmt.Errorf("found ReleasePlanAdmission '%s' with auto-release label set to false",
				releasePlanAdmission.Name)
		}

		activeReleasePlanAdmission = &releasePlanAdmissions.Items[i]

	}

	if activeReleasePlanAdmission == nil {
		return nil, fmt.Errorf("no ReleasePlanAdmission found in the target (%+v) for application '%s'",
			releasePlan.Spec.Target, releasePlan.Spec.Application)
	}

	return activeReleasePlanAdmission, nil
}

// getApplication returns the Application referenced by the ReleasePlanAdmission. If the Application is not found or
// the Get operation failed, an error will be returned.
func (a *Adapter) getApplication(releasePlanAdmission *v1alpha1.ReleasePlanAdmission) (*applicationapiv1alpha1.Application, error) {
	application := &applicationapiv1alpha1.Application{}
	err := a.client.Get(a.context, types.NamespacedName{
		Name:      releasePlanAdmission.Spec.Application,
		Namespace: releasePlanAdmission.Namespace,
	}, application)

	if err != nil {
		return nil, err
	}

	return application, nil
}

// getApplicationComponents returns a list of all the Components associated with the given Application.
func (a *Adapter) getApplicationComponents(application *applicationapiv1alpha1.Application) ([]applicationapiv1alpha1.Component, error) {
	applicationComponents := &applicationapiv1alpha1.ComponentList{}
	opts := []client.ListOption{
		client.InNamespace(application.Namespace),
		client.MatchingFields{"spec.application": application.Name},
	}

	err := a.client.List(a.context, applicationComponents, opts...)
	if err != nil {
		return nil, err
	}

	return applicationComponents.Items, nil
}

// getSnapshot returns the Snapshot referenced by the Release being processed. If the Snapshot
// is not found or the Get operation failed, an error is returned.
func (a *Adapter) getSnapshot() (*applicationapiv1alpha1.Snapshot, error) {
	snapshot := &applicationapiv1alpha1.Snapshot{}
	err := a.client.Get(a.context, types.NamespacedName{
		Name:      a.release.Spec.Snapshot,
		Namespace: a.release.Namespace,
	}, snapshot)

	if err != nil {
		return nil, err
	}

	return snapshot, nil
}

// getEnvironment returns the Environment referenced by the ReleasePlanAdmission used during this release. If the
// Environment is not found or the Get operation fails, an error will be returned.
func (a *Adapter) getEnvironment(releasePlanAdmission *v1alpha1.ReleasePlanAdmission) (*applicationapiv1alpha1.Environment, error) {
	environment := &applicationapiv1alpha1.Environment{}
	err := a.client.Get(a.context, types.NamespacedName{
		Name:      releasePlanAdmission.Spec.Environment,
		Namespace: releasePlanAdmission.Namespace,
	}, environment)

	if err != nil {
		return nil, err
	}

	return environment, nil
}

// getReleasePlan returns the ReleasePlan referenced by the Release being processed. If the ReleasePlan is not found or
// the Get operation fails, an error will be returned.
func (a *Adapter) getReleasePlan() (*v1alpha1.ReleasePlan, error) {
	releasePlan := &v1alpha1.ReleasePlan{}
	err := a.client.Get(a.context, types.NamespacedName{
		Namespace: a.release.Namespace,
		Name:      a.release.Spec.ReleasePlan,
	}, releasePlan)
	if err != nil {
		return nil, err
	}

	return releasePlan, nil
}

// getReleasePipelineRun returns the PipelineRun referenced by the Release being processed or nil if it's not found.
// In the case the List operation fails, an error will be returned.
func (a *Adapter) getReleasePipelineRun() (*v1beta1.PipelineRun, error) {
	pipelineRuns := &v1beta1.PipelineRunList{}
	opts := []client.ListOption{
		client.Limit(1),
		client.MatchingLabels{
			tekton.ReleaseNameLabel:      a.release.Name,
			tekton.ReleaseNamespaceLabel: a.release.Namespace,
		},
	}

	err := a.client.List(a.context, pipelineRuns, opts...)
	if err == nil && len(pipelineRuns.Items) > 0 {
		return &pipelineRuns.Items[0], nil
	}

	return nil, err
}

// getReleaseStrategy returns the ReleaseStrategy referenced by the given ReleasePlanAdmission. If the ReleaseStrategy
// is not found or the Get operation fails, an error will be returned.
func (a *Adapter) getReleaseStrategy(releasePlanAdmission *v1alpha1.ReleasePlanAdmission) (*v1alpha1.ReleaseStrategy, error) {
	releaseStrategy := &v1alpha1.ReleaseStrategy{}
	err := a.client.Get(a.context, types.NamespacedName{
		Name:      releasePlanAdmission.Spec.ReleaseStrategy,
		Namespace: releasePlanAdmission.Namespace,
	}, releaseStrategy)

	if err != nil {
		return nil, err
	}

	return releaseStrategy, nil
}

// getEnterpriseContractPolicy return the EnterpriseContractPolicy referenced by the given ReleaseStrategy.
func (a *Adapter) getEnterpriseContractPolicy(releaseStrategy *v1alpha1.ReleaseStrategy) (*ecapiv1alpha1.EnterpriseContractPolicy, error) {
	enterpriseContractPolicy := &ecapiv1alpha1.EnterpriseContractPolicy{}
	err := a.client.Get(a.context, types.NamespacedName{
		Name:      releaseStrategy.Spec.Policy,
		Namespace: releaseStrategy.Namespace,
	}, enterpriseContractPolicy)

	if err != nil {
		return nil, err
	}

	return enterpriseContractPolicy, nil
}

// getSnapshotEnvironmentBinding returns the SnapshotEnvironmentBinding associated with the Release being processed.
// That association is defined by both the Environment and Application matching between the ReleasePlanAdmission and
// the SnapshotEnvironmentBinding. If the Get operation fails, an error will be returned.
func (a *Adapter) getSnapshotEnvironmentBinding(environment *applicationapiv1alpha1.Environment,
	releasePlanAdmission *v1alpha1.ReleasePlanAdmission) (*applicationapiv1alpha1.SnapshotEnvironmentBinding, error) {
	bindingList := &applicationapiv1alpha1.SnapshotEnvironmentBindingList{}
	opts := []client.ListOption{
		client.InNamespace(environment.Namespace),
		client.MatchingFields{"spec.environment": environment.Name},
	}

	err := a.client.List(a.context, bindingList, opts...)
	if err != nil {
		return nil, err
	}

	for _, binding := range bindingList.Items {
		if binding.Spec.Application == releasePlanAdmission.Spec.Application {
			return &binding, nil
		}
	}

	return nil, nil
}

// getSnapshotEnvironmentBindingFromReleaseStatus returns the SnapshotEnvironmentBinding associated with the Release being processed.
// That association is defined by namespaced name stored in the Release's status
func (a *Adapter) getSnapshotEnvironmentBindingFromReleaseStatus() (*applicationapiv1alpha1.SnapshotEnvironmentBinding, error) {
	binding := &applicationapiv1alpha1.SnapshotEnvironmentBinding{}
	bindingNamespacedName := strings.Split(a.release.Status.SnapshotEnvironmentBinding, string(types.Separator))
	if len(bindingNamespacedName) != 2 {
		return nil, fmt.Errorf("found invalid namespaced name of SnapshotEnvironmentBinding in"+
			" release status: '%s'", a.release.Status.SnapshotEnvironmentBinding)
	}

	err := a.client.Get(a.context, types.NamespacedName{
		Namespace: bindingNamespacedName[0],
		Name:      bindingNamespacedName[1],
	}, binding)

	if err != nil {
		return nil, err
	}

	return binding, nil
}

// getSnapshotEnvironmentResources returns all the resources required to create a SnapshotEnvironmentBinding. If any of
// those resources cannot be retrieved from the cluster, an error will be returned.
func (a *Adapter) getSnapshotEnvironmentResources(releasePlanAdmission *v1alpha1.ReleasePlanAdmission) (
	*applicationapiv1alpha1.Application, []applicationapiv1alpha1.Component, *applicationapiv1alpha1.Snapshot, error) {
	application, err := a.getApplication(releasePlanAdmission)
	if err != nil {
		return application, nil, nil, err
	}

	components, err := a.getApplicationComponents(application)
	if err != nil {
		return application, nil, nil, err
	}

	snapshot, err := a.getSnapshot()
	if err != nil {
		return application, components, nil, err
	}

	return application, components, snapshot, err
}

// registerGitOpsDeploymentStatus updates the status of the Release being processed by monitoring the status of the
// associated SnapshotEnvironmentBinding and setting the appropriate state in the Release.
func (a *Adapter) registerGitOpsDeploymentStatus(binding *applicationapiv1alpha1.SnapshotEnvironmentBinding) error {
	if binding == nil {
		return nil
	}

	condition := meta.FindStatusCondition(binding.Status.ComponentDeploymentConditions,
		applicationapiv1alpha1.ComponentDeploymentConditionAllComponentsDeployed)
	if condition == nil {
		return nil
	}

	patch := client.MergeFrom(a.release.DeepCopy())

	if condition.Status == metav1.ConditionUnknown {
		a.release.MarkDeploying(condition.Reason, condition.Message)
	} else {
		a.release.MarkDeployed(condition.Status, condition.Reason, condition.Message)
	}

	return a.client.Status().Patch(a.context, a.release, patch)
}

// registerReleasePipelineRunStatus updates the status of the Release being processed by monitoring the status of the
// associated release PipelineRun and setting the appropriate state in the Release. If the PipelineRun hasn't
// started/succeeded, no action will be taken.
func (a *Adapter) registerReleasePipelineRunStatus(pipelineRun *v1beta1.PipelineRun) error {
	if pipelineRun != nil && pipelineRun.IsDone() {
		patch := client.MergeFrom(a.release.DeepCopy())

		a.release.Status.CompletionTime = &metav1.Time{Time: time.Now()}

		condition := pipelineRun.Status.GetCondition(apis.ConditionSucceeded)
		if condition.IsTrue() {
			a.release.MarkSucceeded()
		} else {
			a.release.MarkFailed(v1alpha1.ReleaseReasonPipelineFailed, condition.Message)
		}

		return a.client.Status().Patch(a.context, a.release, patch)
	}

	return nil
}

// registerReleaseStatusData adds all the Release information to its Status.
func (a *Adapter) registerReleaseStatusData(releasePipelineRun *v1beta1.PipelineRun, releaseStrategy *v1alpha1.ReleaseStrategy) error {
	if releasePipelineRun == nil || releaseStrategy == nil {
		return nil
	}

	patch := client.MergeFrom(a.release.DeepCopy())

	a.release.Status.ReleasePipelineRun = fmt.Sprintf("%s%c%s",
		releasePipelineRun.Namespace, types.Separator, releasePipelineRun.Name)
	a.release.Status.ReleaseStrategy = fmt.Sprintf("%s%c%s",
		releaseStrategy.Namespace, types.Separator, releaseStrategy.Name)
	a.release.Status.Target = releasePipelineRun.Namespace

	a.release.MarkRunning()

	return a.client.Status().Patch(a.context, a.release, patch)
}

// syncResources sync all the resources needed to trigger the deployment of the Release being processed.
func (a *Adapter) syncResources() error {
	releasePlanAdmission, err := a.getActiveReleasePlanAdmission()
	if err != nil {
		return err
	}

	snapshot, err := a.getSnapshot()
	if err != nil {
		return err
	}

	return a.syncer.SyncSnapshot(snapshot, releasePlanAdmission.Namespace)
}
