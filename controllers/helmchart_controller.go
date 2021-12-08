/*
Copyright 2020 The Flux authors

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/untar"
	"github.com/fluxcd/source-controller/internal/utils"
	helmgetter "helm.sh/helm/v3/pkg/getter"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/predicates"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	serror "github.com/fluxcd/source-controller/internal/error"
	"github.com/fluxcd/source-controller/internal/helm/chart"
	"github.com/fluxcd/source-controller/internal/helm/getter"
	"github.com/fluxcd/source-controller/internal/helm/repository"
	sreconcile "github.com/fluxcd/source-controller/internal/reconcile"
)

// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// HelmChartReconciler reconciles a HelmChart object
type HelmChartReconciler struct {
	client.Client
	helper.Events
	helper.Metrics

	Storage *Storage
	Getters helmgetter.Providers
}

func (r *HelmChartReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, HelmChartReconcilerOptions{})
}

type HelmChartReconcilerOptions struct {
	MaxConcurrentReconciles int
}

type helmChartReconcilerFunc func(ctx context.Context, obj *sourcev1.HelmChart, build *chart.Build) (sreconcile.Result, error)

func (r *HelmChartReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts HelmChartReconcilerOptions) error {
	if err := mgr.GetCache().IndexField(context.TODO(), &sourcev1.HelmRepository{}, sourcev1.HelmRepositoryURLIndexKey,
		r.indexHelmRepositoryByURL); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}
	if err := mgr.GetCache().IndexField(context.TODO(), &sourcev1.HelmChart{}, sourcev1.SourceIndexKey,
		r.indexHelmChartBySource); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.HelmChart{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		Watches(
			&source.Kind{Type: &sourcev1.HelmRepository{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForHelmRepositoryChange),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		Watches(
			&source.Kind{Type: &sourcev1.GitRepository{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForGitRepositoryChange),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		Watches(
			&source.Kind{Type: &sourcev1.Bucket{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForBucketChange),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: opts.MaxConcurrentReconciles}).
		Complete(r)
}

func (r *HelmChartReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	// Fetch the HelmChart
	obj := &sourcev1.HelmChart{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Record suspended status metric
	r.RecordSuspend(ctx, obj, obj.Spec.Suspend)

	// Return early if the object is suspended
	if obj.Spec.Suspend {
		log.Info("Reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(obj, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Result of the sub-reconciliation
	var recResult sreconcile.Result

	// Always attempt to patch the object after each reconciliation.
	// NOTE: This deferred block only modifies the named return error. The
	// result from the reconciliation remains the same. Any requeue attributes
	// set in the result will continue to be effective.
	defer func() {
		retErr = r.summarizeAndPatch(ctx, obj, patchHelper, recResult, retErr)

		// Always record readiness and duration metrics
		r.Metrics.RecordReadiness(ctx, obj)
		r.Metrics.RecordDuration(ctx, obj, start)
	}()

	// Add finalizer first if not exist to avoid the race condition
	// between init and delete
	if !controllerutil.ContainsFinalizer(obj, sourcev1.SourceFinalizer) {
		controllerutil.AddFinalizer(obj, sourcev1.SourceFinalizer)
		recResult = sreconcile.ResultRequeue
		return ctrl.Result{Requeue: true}, nil
	}

	// Examine if the object is under deletion
	if !obj.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err := r.reconcileDelete(ctx, obj)
		return sreconcile.BuildReconcileResult(obj, res, err)
	}

	// Reconcile actual object
	reconcilers := []helmChartReconcilerFunc{
		r.reconcileStorage,
		r.reconcileSource,
		r.reconcileArtifact,
	}
	recResult, err = r.reconcile(ctx, obj, reconcilers)
	return sreconcile.BuildReconcileResult(obj, recResult, err)
}

// summarizeAndPatch analyzes the object conditions to create a summary of the
// status conditions and patches the object with the calculated summary. The
// reconciler error type is also used to determine the conditions and the
// returned error.
func (r *HelmChartReconciler) summarizeAndPatch(ctx context.Context, obj *sourcev1.HelmChart, patchHelper *patch.Helper, res sreconcile.Result, recErr error) error {
	// Remove reconciling condition on successful reconciliation
	if recErr == nil && res == sreconcile.ResultSuccess {
		conditions.Delete(obj, meta.ReconcilingCondition)
	}

	// Record the value of the reconciliation request, if any
	if v, ok := meta.ReconcileAnnotationValue(obj.GetAnnotations()); ok {
		obj.Status.SetLastHandledReconcileRequest(v)
	}

	// Summarize Ready condition
	conditions.SetSummary(obj,
		meta.ReadyCondition,
		conditions.WithConditions(
			sourcev1.BuildFailedCondition,
			sourcev1.FetchFailedCondition,
			sourcev1.ArtifactOutdatedCondition,
			meta.ReconcilingCondition,
		),
		conditions.WithNegativePolarityConditions(
			sourcev1.BuildFailedCondition,
			sourcev1.FetchFailedCondition,
			sourcev1.ArtifactOutdatedCondition,
			meta.ReconcilingCondition,
		),
	)

	// Patch the object, ignoring conflicts on the conditions owned by this controller
	patchOpts := []patch.Option{
		patch.WithOwnedConditions{
			Conditions: []string{
				sourcev1.BuildFailedCondition,
				sourcev1.FetchFailedCondition,
				sourcev1.ArtifactOutdatedCondition,
				meta.ReadyCondition,
				meta.ReconcilingCondition,
				meta.StalledCondition,
			},
		},
	}

	// Analyze the reconcile error.
	switch t := recErr.(type) {
	case *serror.StallingError:
		// The current generation has been reconciled successfully and it has
		// resulted in a stalled state. Return no error to stop further
		// requeuing.
		patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		conditions.MarkFalse(obj, meta.ReadyCondition, t.Reason, t.Error())
		conditions.MarkStalled(obj, t.Reason, t.Error())
		recErr = nil
	case nil:
		// The reconcile didn't result in any error, we are not in stalled
		// state. If a requeue is requested, the current generation has not been
		// reconciled successfully.
		if res != sreconcile.ResultRequeue {
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		}
		conditions.Delete(obj, meta.StalledCondition)
	default:
		// The reconcile resulted in some error, but we are not in stalled
		// state.
		conditions.Delete(obj, meta.StalledCondition)
	}

	// Finally, patch the resource
	if err := patchHelper.Patch(ctx, obj, patchOpts...); err != nil {
		// Ignore patch error "not found" when the object is being deleted.
		if !obj.ObjectMeta.DeletionTimestamp.IsZero() {
			err = kerrors.FilterOut(err, func(e error) bool { return apierrs.IsNotFound(err) })
		}
		recErr = kerrors.NewAggregate([]error{recErr, err})
	}
	return recErr
}

// reconcile steps through the actual reconciliation tasks for the object, it returns early on the first step that
// produces an error.
func (r *HelmChartReconciler) reconcile(ctx context.Context, obj *sourcev1.HelmChart, reconcilers []helmChartReconcilerFunc) (sreconcile.Result, error) {
	if obj.Generation != obj.Status.ObservedGeneration {
		conditions.MarkReconciling(obj, "NewGeneration", "Reconciling new generation %d", obj.Generation)
	}

	// Run the sub-reconcilers and build the result of reconciliation.
	var (
		build  chart.Build
		res    sreconcile.Result
		resErr error
	)
	for _, rec := range reconcilers {
		recResult, err := rec(ctx, obj, &build)
		// Prioritize requeue request in the result.
		res = sreconcile.LowestRequeuingResult(res, recResult)
		if err != nil {
			resErr = err
			break
		}
	}
	return res, resErr
}

// reconcileStorage ensures the current state of the storage matches the desired and previously observed state.
//
// All artifacts for the resource except for the current one are garbage collected from the storage.
// If the artifact in the Status object of the resource disappeared from storage, it is removed from the object.
// If the object does not have an artifact in its Status object, a v1beta1.ArtifactUnavailableCondition is set.
// If the hostname of the URLs on the object do not match the current storage server hostname, they are updated.
//
// The caller should assume a failure if an error is returned, or the BuildResult is zero.
func (r *HelmChartReconciler) reconcileStorage(ctx context.Context, obj *sourcev1.HelmChart, build *chart.Build) (sreconcile.Result, error) {
	// Garbage collect previous advertised artifact(s) from storage
	_ = r.garbageCollect(ctx, obj)

	// Determine if the advertised artifact is still in storage
	if artifact := obj.GetArtifact(); artifact != nil && !r.Storage.ArtifactExist(*artifact) {
		obj.Status.Artifact = nil
		obj.Status.URL = ""
	}

	// Record that we do not have an artifact
	if obj.GetArtifact() == nil {
		conditions.MarkReconciling(obj, "NoArtifact", "No artifact for resource in storage")
		return sreconcile.ResultSuccess, nil
	}

	// Always update URLs to ensure hostname is up-to-date
	// TODO(hidde): we may want to send out an event only if we notice the URL has changed
	r.Storage.SetArtifactURL(obj.GetArtifact())
	obj.Status.URL = r.Storage.SetHostname(obj.Status.URL)

	return sreconcile.ResultSuccess, nil
}

// reconcileSource reconciles the upstream bucket with the client for the given object's Provider, and returns the
// result.
// If a SecretRef is defined, it attempts to fetch the Secret before calling the provider. If the fetch of the Secret
// fails, it records v1beta1.FetchFailedCondition=True and returns early.
//
// The caller should assume a failure if an error is returned, or the BuildResult is zero.
func (r *HelmChartReconciler) reconcileSource(ctx context.Context, obj *sourcev1.HelmChart, build *chart.Build) (sreconcile.Result, error) {
	// Retrieve the source
	s, err := r.getSource(ctx, obj)
	if err != nil {
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, "SourceUnavailable", "Failed to get source: %s", err)
		r.Eventf(ctx, obj, events.EventSeverityError, "SourceUnavailable", "Failed to get source: %s", err)

		// Return Kubernetes client errors, but ignore others which can only be
		// solved by a change in generation
		if apierrs.ReasonForError(err) != metav1.StatusReasonUnknown {
			ctrl.LoggerFrom(ctx).Error(err, "failed to get source")
			err = &serror.StallingError{Err: err, Reason: "UnsupportedSourceKind"}
		}
		return sreconcile.ResultEmpty, err
	}

	// Assert source has an artifact
	if s.GetArtifact() == nil || !r.Storage.ArtifactExist(*s.GetArtifact()) {
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, "NoSourceArtifact",
			"No artifact available for %s source '%s'", obj.Spec.SourceRef.Kind, obj.Spec.SourceRef.Name)
		r.Eventf(ctx, obj, events.EventSeverityError, "NoSourceArtifact",
			"No artifact available for %s source '%s'", obj.Spec.SourceRef.Kind, obj.Spec.SourceRef.Name)
		return sreconcile.ResultRequeue, nil
	}

	// Record current artifact revision as last observed
	obj.Status.ObservedSourceArtifactRevision = s.GetArtifact().Revision

	// Perform the reconciliation for the chart source type
	switch typedSource := s.(type) {
	case *sourcev1.HelmRepository:
		return r.reconcileFromHelmRepository(ctx, obj, typedSource, build)
	case *sourcev1.GitRepository, *sourcev1.Bucket:
		return r.reconcileFromTarballArtifact(ctx, obj, *typedSource.GetArtifact(), build)
	default:
		// Ending up here should generally not be possible
		// as getSource already validates
		return sreconcile.ResultEmpty, nil
	}
}

func (r *HelmChartReconciler) reconcileFromHelmRepository(ctx context.Context, obj *sourcev1.HelmChart,
	repo *sourcev1.HelmRepository, b *chart.Build) (sreconcile.Result, error) {

	// Construct the Getter options from the HelmRepository data
	clientOpts := []helmgetter.Option{
		helmgetter.WithURL(repo.Spec.URL),
		helmgetter.WithTimeout(repo.Spec.Timeout.Duration),
		helmgetter.WithPassCredentialsAll(repo.Spec.PassCredentials),
	}
	if secret, err := r.getHelmRepositorySecret(ctx, repo); secret != nil || err != nil {
		if err != nil {
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.AuthenticationFailedReason,
				"Failed to get secret '%s': %s", repo.Spec.SecretRef.Name, err.Error())
			r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.AuthenticationFailedReason,
				"Failed to get secret '%s': %s", repo.Spec.SecretRef.Name, err.Error())
			// Return error as the world as observed may change
			return sreconcile.ResultEmpty, err
		}

		// Create temporary working directory for credentials
		authDir, err := utils.TmpDirForObj("", obj)
		if err != nil {
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.StorageOperationFailedReason,
				"Failed to create temporary working directory: %s", err)
			r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
				"Failed to create temporary working directory: %s", err)
			return sreconcile.ResultEmpty, err
		}
		defer os.RemoveAll(authDir)

		// Build client options from secret
		opts, err := getter.ClientOptionsFromSecret(authDir, *secret)
		if err != nil {
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.AuthenticationFailedReason,
				"Failed to configure Helm client with secret data: %s", err)
			r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.AuthenticationFailedReason,
				"Failed to configure Helm client with secret data: %s", err)
			// Requeue as content of secret might change
			return sreconcile.ResultEmpty, err
		}
		clientOpts = append(clientOpts, opts...)
	}

	// Initialize the chart repository
	chartRepo, err := repository.NewChartRepository(repo.Spec.URL, r.Storage.LocalPath(*repo.GetArtifact()), r.Getters, clientOpts)
	if err != nil {
		// Any error requires a change in generation,
		// which we should be informed about by the watcher
		switch err.(type) {
		case *url.Error:
			ctrl.LoggerFrom(ctx).Error(err, "invalid Helm repository URL")
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.URLInvalidReason,
				"Invalid Helm repository URL: %s", err.Error())
			return sreconcile.ResultEmpty, &serror.StallingError{Err: err, Reason: sourcev1.URLInvalidReason}
		default:
			ctrl.LoggerFrom(ctx).Error(err, "failed to construct Helm client")
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, meta.FailedReason,
				"Failed to construct Helm client: %s", err.Error())
			return sreconcile.ResultEmpty, &serror.StallingError{Err: err, Reason: meta.FailedReason}
		}
	}

	// Construct the chart builder with scoped configuration
	cb := chart.NewRemoteBuilder(chartRepo)
	opts := chart.BuildOptions{
		ValuesFiles: obj.GetValuesFiles(),
		Force:       obj.Generation != obj.Status.ObservedGeneration,
	}
	if artifact := obj.GetArtifact(); artifact != nil {
		opts.CachedChart = r.Storage.LocalPath(*artifact)
	}

	// Set the VersionMetadata to the object's Generation if ValuesFiles is defined
	// This ensures changes can be noticed by the Artifact consumer
	if len(opts.GetValuesFiles()) > 0 {
		opts.VersionMetadata = strconv.FormatInt(obj.Generation, 10)
	}

	// Build the chart
	ref := chart.RemoteReference{Name: obj.Spec.Chart, Version: obj.Spec.Version}
	build, err := cb.Build(ctx, ref, utils.TmpPathForObj("", ".tgz", obj), opts)

	// Record both success _and_ error observations on the object
	processChartBuild(obj, build, err)

	// Handle any build error
	if err != nil {
		if buildErr := new(chart.BuildError); errors.As(err, &buildErr) {
			r.Eventf(ctx, obj, events.EventSeverityError, buildErr.Reason.Reason, buildErr.Error())
			if chart.IsPersistentBuildErrorReason(buildErr.Reason) {
				ctrl.LoggerFrom(ctx).Error(err, "failed to build chart from remote source")
				err = &serror.StallingError{Err: err, Reason: buildErr.Reason.Reason}
			}
		}
		return sreconcile.ResultEmpty, err
	}

	*b = *build
	return sreconcile.ResultSuccess, nil
}

func (r *HelmChartReconciler) reconcileFromTarballArtifact(ctx context.Context, obj *sourcev1.HelmChart, source sourcev1.Artifact, b *chart.Build) (sreconcile.Result, error) {
	// Create temporary working directory
	tmpDir, err := utils.TmpDirForObj("", obj)
	if err != nil {
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.StorageOperationFailedReason,
			"Failed to create temporary working directory: %s", err)
		r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
			"Failed to create temporary working directory: %s", err)
		return sreconcile.ResultEmpty, err
	}
	defer os.RemoveAll(tmpDir)

	// Create directory to untar source into
	sourceDir := filepath.Join(tmpDir, "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.StorageOperationFailedReason,
			"Failed to create directory to untar source into: %s", err)
		r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
			"Failed to create directory to untar source into: %s", err)
		return sreconcile.ResultEmpty, err
	}

	// Open the tarball artifact file and untar files into working directory
	f, err := os.Open(r.Storage.LocalPath(source))
	if err != nil {
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.StorageOperationFailedReason,
			"Failed to open source artifact: %s", err)
		r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
			"Failed to open source artifact: %s", err)
		return sreconcile.ResultEmpty, err
	}
	if _, err = untar.Untar(f, sourceDir); err != nil {
		_ = f.Close()
		err = fmt.Errorf("artifact untar error: %w", err)
		return sreconcile.ResultEmpty, err
	}
	if err = f.Close(); err != nil {
		err = fmt.Errorf("artifact close error: %w", err)
		return sreconcile.ResultEmpty, err
	}

	// Calculate (secure) absolute chart path
	chartPath, err := securejoin.SecureJoin(sourceDir, obj.Spec.Chart)
	if err != nil {
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, "IllegalPath",
			"Path calculation for chart '%s' failed: %s", obj.Spec.Chart, err.Error())
		// We are unable to recover from this change without a change in generation
		return sreconcile.ResultEmpty, &serror.StallingError{Err: err, Reason: "IllegalPath"}
	}

	// Setup dependency manager
	authDir := filepath.Join(tmpDir, "creds")
	if err = os.Mkdir(authDir, 0700); err != nil {
		err = fmt.Errorf("failed to create temporary directory for dependency credentials: %w", err)
		return sreconcile.ResultEmpty, err
	}
	dm := chart.NewDependencyManager(
		chart.WithRepositoryCallback(r.namespacedChartRepositoryCallback(ctx, authDir, obj.GetNamespace())),
	)
	defer dm.Clear()

	// Configure builder options, including any previously cached chart
	opts := chart.BuildOptions{
		ValuesFiles: obj.GetValuesFiles(),
		Force:       obj.Generation != obj.Status.ObservedGeneration,
	}
	if artifact := obj.Status.Artifact; artifact != nil {
		opts.CachedChart = artifact.Path
	}

	// Add revision metadata to chart build
	if obj.Spec.ReconcileStrategy == sourcev1.ReconcileStrategyRevision {
		// Isolate the commit SHA from GitRepository type artifacts by removing the branch/ prefix.
		splitRev := strings.Split(source.Revision, "/")
		opts.VersionMetadata = splitRev[len(splitRev)-1]
	}
	// Configure revision metadata for chart build if we should react to revision changes
	if obj.Spec.ReconcileStrategy == sourcev1.ReconcileStrategyRevision {
		rev := source.Revision
		if obj.Spec.SourceRef.Kind == sourcev1.GitRepositoryKind {
			// Split the reference by the `/` delimiter which may be present,
			// and take the last entry which contains the SHA.
			split := strings.Split(source.Revision, "/")
			rev = split[len(split)-1]
		}
		if kind := obj.Spec.SourceRef.Kind; kind == sourcev1.GitRepositoryKind || kind == sourcev1.BucketKind {
			// The SemVer from the metadata is at times used in e.g. the label metadata for a resource
			// in a chart, which has a limited length of 63 characters.
			// To not fill most of this space with a full length SHA hex (40 characters for SHA-1, and
			// even more for SHA-2 for a chart from a Bucket), we shorten this to the first 12
			// characters taken from the hex.
			// For SHA-1, this has proven to be unique in the Linux kernel with over 875.000 commits
			// (http://git-scm.com/book/en/v2/Git-Tools-Revision-Selection#Short-SHA-1).
			// Note that for a collision to be problematic, it would need to happen right after the
			// previous SHA for the artifact, which is highly unlikely, if not virtually impossible.
			// Ref: https://en.wikipedia.org/wiki/Birthday_attack
			rev = rev[0:12]
		}
		opts.VersionMetadata = rev
	}

	// Build chart
	cb := chart.NewLocalBuilder(dm)
	build, err := cb.Build(ctx, chart.LocalReference{
		WorkDir: sourceDir,
		Path:    chartPath,
	}, utils.TmpPathForObj("", ".tgz", obj), opts)

	// Record both success _and_ error observations on the object
	processChartBuild(obj, build, err)

	// Handle any build error
	if err != nil {
		if buildErr := new(chart.BuildError); errors.As(err, &buildErr) {
			r.Eventf(ctx, obj, events.EventSeverityError, buildErr.Reason.Reason, buildErr.Error())
			if chart.IsPersistentBuildErrorReason(buildErr.Reason) {
				ctrl.LoggerFrom(ctx).Error(err, "failed to build chart from source artifact")
				err = &serror.StallingError{Err: err, Reason: buildErr.Reason.Reason}
			}
		}
		return sreconcile.ResultEmpty, err
	}

	// If we actually build a chart, take a historical note of any dependencies we resolved.
	// The reason this is a done conditionally, is because if we have a cached one in storage,
	// we can not recover this information (and put it in a condition). Which would result in
	// a sudden (partial) disappearance of observed state.
	// TODO(hidde): include specific name/version information?
	if depNum := build.ResolvedDependencies; depNum > 0 {
		r.Eventf(ctx, obj, events.EventSeverityInfo, "ResolvedDependencies", "Resolved %d chart dependencies", depNum)
	}

	*b = *build
	return sreconcile.ResultSuccess, nil
}

// reconcileArtifact reconciles the given chart.Build to an v1beta1.Artifact in the Storage, and records it
// on the object.
func (r *HelmChartReconciler) reconcileArtifact(ctx context.Context, obj *sourcev1.HelmChart, b *chart.Build) (sreconcile.Result, error) {
	// Without a complete chart build, there is little to reconcile
	if !b.Complete() {
		return sreconcile.ResultRequeue, nil
	}

	// Always restore the conditions in case they got overwritten by transient errors
	defer func() {
		if obj.Status.ObservedChartName == b.Name && obj.GetArtifact().HasRevision(b.Version) {
			conditions.Delete(obj, sourcev1.ArtifactOutdatedCondition)
			conditions.MarkTrue(obj, meta.ReadyCondition, reasonForBuild(b), b.Summary())
		}
	}()

	// Create artifact from build data
	artifact := r.Storage.NewArtifactFor(obj.Kind, obj.GetObjectMeta(), b.Version, fmt.Sprintf("%s-%s.tgz", b.Name, b.Version))

	// Return early if the build path equals the current artifact path
	if artifact := obj.GetArtifact(); artifact != nil && artifact.Path == b.Path {
		ctrl.LoggerFrom(ctx).Info(fmt.Sprintf("Already up to date, current revision '%s'", artifact.Revision))
		return sreconcile.ResultSuccess, nil
	}

	// Garbage collect chart build once persisted to storage
	defer os.Remove(b.Path)

	// Ensure artifact directory exists and acquire lock
	if err := r.Storage.MkdirAll(artifact); err != nil {
		err = fmt.Errorf("failed to create artifact directory: %w", err)
		return sreconcile.ResultEmpty, err
	}
	unlock, err := r.Storage.Lock(artifact)
	if err != nil {
		err = fmt.Errorf("failed to acquire lock for artifact: %w", err)
		return sreconcile.ResultEmpty, err
	}
	defer unlock()

	// Copy the packaged chart to the artifact path
	if err = r.Storage.CopyFromPath(&artifact, b.Path); err != nil {
		r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
			"Unable to copy Helm chart to storage: %s", err.Error())
		return sreconcile.ResultEmpty, err
	}

	// Record it on the object
	obj.Status.Artifact = artifact.DeepCopy()
	obj.Status.ObservedChartName = b.Name

	// Publish an event
	r.Events.EventWithMetaf(ctx, obj, map[string]string{
		"revision": artifact.Revision,
		"checksum": artifact.Checksum,
	}, events.EventSeverityInfo, reasonForBuild(b), b.Summary())

	// Update symlink on a "best effort" basis
	symURL, err := r.Storage.Symlink(artifact, "latest.tar.gz")
	if err != nil {
		r.Events.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
			"Failed to update status URL symlink: %s", err)
	}
	if symURL != "" {
		obj.Status.URL = symURL
	}
	return sreconcile.ResultSuccess, nil
}

// getSource returns the v1beta1.Source for the given object, or an error describing why the source could not be
// returned.
func (r *HelmChartReconciler) getSource(ctx context.Context, obj *sourcev1.HelmChart) (sourcev1.Source, error) {
	namespacedName := types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.Spec.SourceRef.Name,
	}
	var s sourcev1.Source
	switch obj.Spec.SourceRef.Kind {
	case sourcev1.HelmRepositoryKind:
		var repo sourcev1.HelmRepository
		if err := r.Client.Get(ctx, namespacedName, &repo); err != nil {
			return nil, err
		}
		s = &repo
	case sourcev1.GitRepositoryKind:
		var repo sourcev1.GitRepository
		if err := r.Client.Get(ctx, namespacedName, &repo); err != nil {
			return nil, err
		}
		s = &repo
	case sourcev1.BucketKind:
		var bucket sourcev1.Bucket
		if err := r.Client.Get(ctx, namespacedName, &bucket); err != nil {
			return nil, err
		}
		s = &bucket
	default:
		return nil, fmt.Errorf("unsupported source kind '%s', must be one of: %v", obj.Spec.SourceRef.Kind, []string{
			sourcev1.HelmRepositoryKind, sourcev1.GitRepositoryKind, sourcev1.BucketKind})
	}
	return s, nil
}

// reconcileDelete handles the delete of an object. It first garbage collects all artifacts for the object from the
// artifact storage, if successful, the finalizer is removed from the object.
func (r *HelmChartReconciler) reconcileDelete(ctx context.Context, obj *sourcev1.HelmChart) (sreconcile.Result, error) {
	// Garbage collect the resource's artifacts
	if err := r.garbageCollect(ctx, obj); err != nil {
		// Return the error so we retry the failed garbage collection
		return sreconcile.ResultEmpty, err
	}

	// Remove our finalizer from the list
	controllerutil.RemoveFinalizer(obj, sourcev1.SourceFinalizer)

	// Stop reconciliation as the object is being deleted
	return sreconcile.ResultEmpty, nil
}

// garbageCollect performs a garbage collection for the given v1beta1.HelmChart. It removes all but the current
// artifact except for when the deletion timestamp is set, which will result in the removal of all artifacts for the
// resource.
func (r *HelmChartReconciler) garbageCollect(ctx context.Context, obj *sourcev1.HelmChart) error {
	if !obj.DeletionTimestamp.IsZero() {
		if err := r.Storage.RemoveAll(r.Storage.NewArtifactFor(obj.Kind, obj.GetObjectMeta(), "", "*")); err != nil {
			r.Eventf(ctx, obj, events.EventSeverityError, "GarbageCollectionFailed",
				"Garbage collection for deleted resource failed: %s", err)
			return err
		}
		obj.Status.Artifact = nil
		// TODO(hidde): we should only push this event if we actually garbage collected something
		r.Eventf(ctx, obj, events.EventSeverityInfo, "GarbageCollectionSucceeded",
			"Garbage collected artifacts for deleted resource")
		return nil
	}
	if obj.GetArtifact() != nil {
		if err := r.Storage.RemoveAllButCurrent(*obj.GetArtifact()); err != nil {
			r.Eventf(ctx, obj, events.EventSeverityError, "GarbageCollectionFailed", "Garbage collection of old artifacts failed: %s", err)
			return err
		}
		// TODO(hidde): we should only push this event if we actually garbage collected something
		r.Eventf(ctx, obj, events.EventSeverityInfo, "GarbageCollectionSucceeded", "Garbage collected old artifacts")
	}
	return nil
}

// namespacedChartRepositoryCallback returns a chart.GetChartRepositoryCallback scoped to the given namespace.
// Credentials for retrieved v1beta1.HelmRepository objects are stored in the given directory.
// The returned callback returns a repository.ChartRepository configured with the retrieved v1beta1.HelmRepository,
// or a shim with defaults if no object could be found.
func (r *HelmChartReconciler) namespacedChartRepositoryCallback(ctx context.Context, dir, namespace string) chart.GetChartRepositoryCallback {
	return func(url string) (*repository.ChartRepository, error) {
		repo, err := r.resolveDependencyRepository(ctx, url, namespace)
		if err != nil {
			// Return Kubernetes client errors, but ignore others
			if apierrs.ReasonForError(err) != metav1.StatusReasonUnknown {
				return nil, err
			}
			repo = &sourcev1.HelmRepository{
				Spec: sourcev1.HelmRepositorySpec{
					URL:     url,
					Timeout: &metav1.Duration{Duration: 60 * time.Second},
				},
			}
		}
		clientOpts := []helmgetter.Option{
			helmgetter.WithURL(repo.Spec.URL),
			helmgetter.WithTimeout(repo.Spec.Timeout.Duration),
			helmgetter.WithPassCredentialsAll(repo.Spec.PassCredentials),
		}
		if secret, err := r.getHelmRepositorySecret(ctx, repo); secret != nil || err != nil {
			if err != nil {
				return nil, err
			}
			opts, err := getter.ClientOptionsFromSecret(dir, *secret)
			if err != nil {
				return nil, err
			}
			clientOpts = append(clientOpts, opts...)
		}
		chartRepo, err := repository.NewChartRepository(repo.Spec.URL, "", r.Getters, clientOpts)
		if err != nil {
			return nil, err
		}
		if repo.Status.Artifact != nil {
			chartRepo.CachePath = r.Storage.LocalPath(*repo.GetArtifact())
		}
		return chartRepo, nil
	}
}

func (r *HelmChartReconciler) resolveDependencyRepository(ctx context.Context, url string, namespace string) (*sourcev1.HelmRepository, error) {
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{sourcev1.HelmRepositoryURLIndexKey: url},
	}
	var list sourcev1.HelmRepositoryList
	err := r.Client.List(ctx, &list, listOpts...)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve HelmRepositoryList: %w", err)
	}
	if len(list.Items) > 0 {
		return &list.Items[0], nil
	}
	return nil, fmt.Errorf("no HelmRepository found for '%s' in '%s' namespace", url, namespace)
}

func (r *HelmChartReconciler) getHelmRepositorySecret(ctx context.Context, repository *sourcev1.HelmRepository) (*corev1.Secret, error) {
	if repository.Spec.SecretRef == nil {
		return nil, nil
	}
	name := types.NamespacedName{
		Namespace: repository.GetNamespace(),
		Name:      repository.Spec.SecretRef.Name,
	}
	var secret corev1.Secret
	err := r.Client.Get(ctx, name, &secret)
	if err != nil {
		return nil, err
	}
	return &secret, nil
}

func (r *HelmChartReconciler) indexHelmRepositoryByURL(o client.Object) []string {
	repo, ok := o.(*sourcev1.HelmRepository)
	if !ok {
		panic(fmt.Sprintf("Expected a HelmRepository, got %T", o))
	}
	u := repository.NormalizeURL(repo.Spec.URL)
	if u != "" {
		return []string{u}
	}
	return nil
}

func (r *HelmChartReconciler) indexHelmChartBySource(o client.Object) []string {
	hc, ok := o.(*sourcev1.HelmChart)
	if !ok {
		panic(fmt.Sprintf("Expected a HelmChart, got %T", o))
	}
	return []string{fmt.Sprintf("%s/%s", hc.Spec.SourceRef.Kind, hc.Spec.SourceRef.Name)}
}

func (r *HelmChartReconciler) requestsForHelmRepositoryChange(o client.Object) []reconcile.Request {
	repo, ok := o.(*sourcev1.HelmRepository)
	if !ok {
		panic(fmt.Sprintf("Expected a HelmRepository, got %T", o))
	}
	// If we do not have an artifact, we have no requests to make
	if repo.GetArtifact() == nil {
		return nil
	}

	ctx := context.Background()
	var list sourcev1.HelmChartList
	if err := r.List(ctx, &list, client.MatchingFields{
		sourcev1.SourceIndexKey: fmt.Sprintf("%s/%s", sourcev1.HelmRepositoryKind, repo.Name),
	}); err != nil {
		return nil
	}

	var reqs []reconcile.Request
	for _, i := range list.Items {
		if i.Status.ObservedSourceArtifactRevision != repo.GetArtifact().Revision {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&i)})
		}
	}
	return reqs
}

func (r *HelmChartReconciler) requestsForGitRepositoryChange(o client.Object) []reconcile.Request {
	repo, ok := o.(*sourcev1.GitRepository)
	if !ok {
		panic(fmt.Sprintf("Expected a GitRepository, got %T", o))
	}

	// If we do not have an artifact, we have no requests to make
	if repo.GetArtifact() == nil {
		return nil
	}

	var list sourcev1.HelmChartList
	if err := r.List(context.TODO(), &list, client.MatchingFields{
		sourcev1.SourceIndexKey: fmt.Sprintf("%s/%s", sourcev1.GitRepositoryKind, repo.Name),
	}); err != nil {
		return nil
	}

	var reqs []reconcile.Request
	for _, i := range list.Items {
		if i.Status.ObservedSourceArtifactRevision != repo.GetArtifact().Revision {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&i)})
		}
	}
	return reqs
}

func (r *HelmChartReconciler) requestsForBucketChange(o client.Object) []reconcile.Request {
	bucket, ok := o.(*sourcev1.Bucket)
	if !ok {
		panic(fmt.Sprintf("Expected a Bucket, got %T", o))
	}

	// If we do not have an artifact, we have no requests to make
	if bucket.GetArtifact() == nil {
		return nil
	}

	var list sourcev1.HelmChartList
	if err := r.List(context.TODO(), &list, client.MatchingFields{
		sourcev1.SourceIndexKey: fmt.Sprintf("%s/%s", sourcev1.BucketKind, bucket.Name),
	}); err != nil {
		return nil
	}

	var reqs []reconcile.Request
	for _, i := range list.Items {
		if i.Status.ObservedSourceArtifactRevision != bucket.GetArtifact().Revision {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&i)})
		}
	}
	return reqs
}

func processChartBuild(obj *sourcev1.HelmChart, build *chart.Build, err error) {
	if build.HasMetadata() {
		if build.Name != obj.Status.ObservedChartName || !obj.GetArtifact().HasRevision(build.Version) {
			conditions.MarkTrue(obj, sourcev1.ArtifactOutdatedCondition, "NewChart", build.Summary())
		}
	}

	if err == nil {
		conditions.Delete(obj, sourcev1.FetchFailedCondition)
		conditions.Delete(obj, sourcev1.BuildFailedCondition)
		return
	}

	var buildErr *chart.BuildError
	if ok := errors.As(err, &buildErr); !ok {
		buildErr = &chart.BuildError{
			Reason: chart.ErrUnknown,
			Err:    err,
		}
	}

	switch buildErr.Reason {
	case chart.ErrChartMetadataPatch, chart.ErrValuesFilesMerge, chart.ErrDependencyBuild, chart.ErrChartPackage:
		conditions.Delete(obj, sourcev1.FetchFailedCondition)
		conditions.MarkTrue(obj, sourcev1.BuildFailedCondition, buildErr.Reason.Reason, buildErr.Error())
	default:
		conditions.Delete(obj, sourcev1.BuildFailedCondition)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, buildErr.Reason.Reason, buildErr.Error())
	}
}

func reasonForBuild(build *chart.Build) string {
	if build.Packaged {
		return sourcev1.ChartPackageSucceededReason
	}
	return sourcev1.ChartPullSucceededReason
}
