package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"github.com/argoproj/argo-cd/common"
	statecache "github.com/argoproj/argo-cd/controller/cache"
	"github.com/argoproj/argo-cd/controller/metrics"
	"github.com/argoproj/argo-cd/engine/pkg/utils/diff"
	"github.com/argoproj/argo-cd/engine/pkg/utils/health"
	"github.com/argoproj/argo-cd/engine/pkg/utils/io"
	kubeutil "github.com/argoproj/argo-cd/engine/pkg/utils/kube"
	"github.com/argoproj/argo-cd/engine/pkg/utils/kube/sync"
	hookutil "github.com/argoproj/argo-cd/engine/pkg/utils/kube/sync/hook"
	"github.com/argoproj/argo-cd/engine/pkg/utils/kube/sync/ignore"
	resourceutil "github.com/argoproj/argo-cd/engine/pkg/utils/kube/sync/resource"
	"github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	appv1 "github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/reposerver/apiclient"
	"github.com/argoproj/argo-cd/util/argo"
	"github.com/argoproj/argo-cd/util/db"
	argohealth "github.com/argoproj/argo-cd/util/health"
	"github.com/argoproj/argo-cd/util/settings"
)

type resourceInfoProviderStub struct {
}

func (r *resourceInfoProviderStub) IsNamespaced(gk schema.GroupKind) (bool, error) {
	return false, nil
}

type managedResource struct {
	Target    *unstructured.Unstructured
	Live      *unstructured.Unstructured
	Diff      diff.DiffResult
	Group     string
	Version   string
	Kind      string
	Namespace string
	Name      string
	Hook      bool
}

func GetLiveObjs(res []managedResource) []*unstructured.Unstructured {
	objs := make([]*unstructured.Unstructured, len(res))
	for i := range res {
		objs[i] = res[i].Live
	}
	return objs
}

// AppStateManager defines methods which allow to compare application spec and actual application state.
type AppStateManager interface {
	CompareAppState(app *v1alpha1.Application, project *appv1.AppProject, revision string, source v1alpha1.ApplicationSource, noCache bool, localObjects []string) *comparisonResult
	SyncAppState(app *v1alpha1.Application, state *v1alpha1.OperationState)
}

type comparisonResult struct {
	syncStatus           *v1alpha1.SyncStatus
	healthStatus         *v1alpha1.HealthStatus
	resources            []v1alpha1.ResourceStatus
	managedResources     []managedResource
	reconciliationResult sync.ReconciliationResult
	diffNormalizer       diff.Normalizer
	appSourceType        v1alpha1.ApplicationSourceType
}

// appStateManager allows to compare applications to git
type appStateManager struct {
	metricsServer  *metrics.MetricsServer
	db             db.ArgoDB
	settingsMgr    *settings.SettingsManager
	appclientset   appclientset.Interface
	projInformer   cache.SharedIndexInformer
	kubectl        kubeutil.Kubectl
	repoClientset  apiclient.Clientset
	liveStateCache statecache.LiveStateCache
	namespace      string
}

func (m *appStateManager) getRepoObjs(app *v1alpha1.Application, source v1alpha1.ApplicationSource, appLabelKey, revision string, noCache bool) ([]*unstructured.Unstructured, *apiclient.ManifestResponse, error) {
	helmRepos, err := m.db.ListHelmRepositories(context.Background())
	if err != nil {
		return nil, nil, err
	}
	repo, err := m.db.GetRepository(context.Background(), source.RepoURL)
	if err != nil {
		return nil, nil, err
	}
	conn, repoClient, err := m.repoClientset.NewRepoServerClient()
	if err != nil {
		return nil, nil, err
	}
	defer io.Close(conn)

	if revision == "" {
		revision = source.TargetRevision
	}

	plugins, err := m.settingsMgr.GetConfigManagementPlugins()
	if err != nil {
		return nil, nil, err
	}

	tools := make([]*appv1.ConfigManagementPlugin, len(plugins))
	for i := range plugins {
		tools[i] = &plugins[i]
	}

	buildOptions, err := m.settingsMgr.GetKustomizeBuildOptions()
	if err != nil {
		return nil, nil, err
	}
	serverVersion, err := m.liveStateCache.GetServerVersion(app.Spec.Destination.Server)
	if err != nil {
		return nil, nil, err
	}
	manifestInfo, err := repoClient.GenerateManifest(context.Background(), &apiclient.ManifestRequest{
		Repo:              repo,
		Repos:             helmRepos,
		Revision:          revision,
		NoCache:           noCache,
		AppLabelKey:       appLabelKey,
		AppLabelValue:     app.Name,
		Namespace:         app.Spec.Destination.Namespace,
		ApplicationSource: &source,
		Plugins:           tools,
		KustomizeOptions: &appv1.KustomizeOptions{
			BuildOptions: buildOptions,
		},
		KubeVersion: serverVersion,
	})
	if err != nil {
		return nil, nil, err
	}
	targetObjs, err := unmarshalManifests(manifestInfo.Manifests)
	if err != nil {
		return nil, nil, err
	}
	return targetObjs, manifestInfo, nil
}

func unmarshalManifests(manifests []string) ([]*unstructured.Unstructured, error) {
	targetObjs := make([]*unstructured.Unstructured, 0)
	for _, manifest := range manifests {
		obj, err := v1alpha1.UnmarshalToUnstructured(manifest)
		if err != nil {
			return nil, err
		}
		targetObjs = append(targetObjs, obj)
	}
	return targetObjs, nil
}

func DeduplicateTargetObjects(
	namespace string,
	objs []*unstructured.Unstructured,
	infoProvider kubeutil.ResourceInfoProvider,
) ([]*unstructured.Unstructured, []v1alpha1.ApplicationCondition, error) {

	targetByKey := make(map[kubeutil.ResourceKey][]*unstructured.Unstructured)
	for i := range objs {
		obj := objs[i]
		isNamespaced := infoProvider.IsNamespaced(obj.GroupVersionKind().GroupKind())
		if !isNamespaced {
			obj.SetNamespace("")
		} else if obj.GetNamespace() == "" {
			obj.SetNamespace(namespace)
		}
		key := kubeutil.GetResourceKey(obj)
		targetByKey[key] = append(targetByKey[key], obj)
	}
	conditions := make([]v1alpha1.ApplicationCondition, 0)
	result := make([]*unstructured.Unstructured, 0)
	for key, targets := range targetByKey {
		if len(targets) > 1 {
			now := metav1.Now()
			conditions = append(conditions, appv1.ApplicationCondition{
				Type:               appv1.ApplicationConditionRepeatedResourceWarning,
				Message:            fmt.Sprintf("Resource %s appeared %d times among application resources.", key.String(), len(targets)),
				LastTransitionTime: &now,
			})
		}
		result = append(result, targets[len(targets)-1])
	}

	return result, conditions, nil
}

func (m *appStateManager) getComparisonSettings(app *appv1.Application) (string, map[string]v1alpha1.ResourceOverride, diff.Normalizer, error) {
	resourceOverrides, err := m.settingsMgr.GetResourceOverrides()
	if err != nil {
		return "", nil, nil, err
	}
	appLabelKey, err := m.settingsMgr.GetAppInstanceLabelKey()
	if err != nil {
		return "", nil, nil, err
	}
	diffNormalizer, err := argo.NewDiffNormalizer(app.Spec.IgnoreDifferences, resourceOverrides)
	if err != nil {
		return "", nil, nil, err
	}
	return appLabelKey, resourceOverrides, diffNormalizer, nil
}

// CompareAppState compares application git state to the live app state, using the specified
// revision and supplied source. If revision or overrides are empty, then compares against
// revision and overrides in the app spec.
func (m *appStateManager) CompareAppState(app *v1alpha1.Application, project *appv1.AppProject, revision string, source v1alpha1.ApplicationSource, noCache bool, localManifests []string) *comparisonResult {
	appLabelKey, resourceOverrides, diffNormalizer, err := m.getComparisonSettings(app)

	// return unknown comparison result if basic comparison settings cannot be loaded
	if err != nil {
		return &comparisonResult{
			syncStatus: &v1alpha1.SyncStatus{
				ComparedTo: appv1.ComparedTo{Source: source, Destination: app.Spec.Destination},
				Status:     appv1.SyncStatusCodeUnknown,
			},
			healthStatus: &appv1.HealthStatus{Status: health.HealthStatusUnknown},
		}
	}

	// do best effort loading live and target state to present as much information about app state as possible
	failedToLoadObjs := false
	conditions := make([]v1alpha1.ApplicationCondition, 0)

	logCtx := log.WithField("application", app.Name)
	logCtx.Infof("Comparing app state (cluster: %s, namespace: %s)", app.Spec.Destination.Server, app.Spec.Destination.Namespace)

	var targetObjs []*unstructured.Unstructured
	var manifestInfo *apiclient.ManifestResponse
	now := metav1.Now()

	if len(localManifests) == 0 {
		targetObjs, manifestInfo, err = m.getRepoObjs(app, source, appLabelKey, revision, noCache)
		if err != nil {
			targetObjs = make([]*unstructured.Unstructured, 0)
			conditions = append(conditions, v1alpha1.ApplicationCondition{Type: v1alpha1.ApplicationConditionComparisonError, Message: err.Error(), LastTransitionTime: &now})
			failedToLoadObjs = true
		}
	} else {
		targetObjs, err = unmarshalManifests(localManifests)
		if err != nil {
			targetObjs = make([]*unstructured.Unstructured, 0)
			conditions = append(conditions, v1alpha1.ApplicationCondition{Type: v1alpha1.ApplicationConditionComparisonError, Message: err.Error(), LastTransitionTime: &now})
			failedToLoadObjs = true
		}
		manifestInfo = nil
	}

	var infoProvider kubeutil.ResourceInfoProvider
	infoProvider, err = m.liveStateCache.GetClusterCache(app.Spec.Destination.Server)
	if err != nil {
		infoProvider = &resourceInfoProviderStub{}
	}
	targetObjs, dedupConditions, err := DeduplicateTargetObjects(app.Spec.Destination.Namespace, targetObjs, infoProvider)
	if err != nil {
		conditions = append(conditions, v1alpha1.ApplicationCondition{Type: v1alpha1.ApplicationConditionComparisonError, Message: err.Error(), LastTransitionTime: &now})
	}
	conditions = append(conditions, dedupConditions...)

	resFilter, err := m.settingsMgr.GetResourcesFilter()
	if err != nil {
		conditions = append(conditions, v1alpha1.ApplicationCondition{Type: v1alpha1.ApplicationConditionComparisonError, Message: err.Error(), LastTransitionTime: &now})
	} else {
		for i := len(targetObjs) - 1; i >= 0; i-- {
			targetObj := targetObjs[i]
			gvk := targetObj.GroupVersionKind()
			if resFilter.IsExcludedResource(gvk.Group, gvk.Kind, app.Spec.Destination.Server) {
				targetObjs = append(targetObjs[:i], targetObjs[i+1:]...)
				conditions = append(conditions, v1alpha1.ApplicationCondition{
					Type:               v1alpha1.ApplicationConditionExcludedResourceWarning,
					Message:            fmt.Sprintf("Resource %s/%s %s is excluded in the settings", gvk.Group, gvk.Kind, targetObj.GetName()),
					LastTransitionTime: &now,
				})
			}
		}
	}

	logCtx.Debugf("Generated config manifests")
	liveObjByKey, err := m.liveStateCache.GetManagedLiveObjs(app, targetObjs)
	if err != nil {
		liveObjByKey = make(map[kubeutil.ResourceKey]*unstructured.Unstructured)
		conditions = append(conditions, v1alpha1.ApplicationCondition{Type: v1alpha1.ApplicationConditionComparisonError, Message: err.Error(), LastTransitionTime: &now})
		failedToLoadObjs = true
	}
	logCtx.Debugf("Retrieved lived manifests")

	// filter out all resources which are not permitted in the application project
	for k, v := range liveObjByKey {
		if !project.IsLiveResourcePermitted(v, app.Spec.Destination.Server) {
			delete(liveObjByKey, k)
		}
	}

	for _, liveObj := range liveObjByKey {
		if liveObj != nil {
			appInstanceName := kubeutil.GetAppInstanceLabel(liveObj, appLabelKey)
			if appInstanceName != "" && appInstanceName != app.Name {
				conditions = append(conditions, v1alpha1.ApplicationCondition{
					Type:               v1alpha1.ApplicationConditionSharedResourceWarning,
					Message:            fmt.Sprintf("%s/%s is part of a different application: %s", liveObj.GetKind(), liveObj.GetName(), appInstanceName),
					LastTransitionTime: &now,
				})
			}
		}
	}

	reconciliation := sync.Reconcile(targetObjs, liveObjByKey, app.Spec.Destination.Namespace, infoProvider)

	logCtx.Debugf("built managed objects list")
	// Do the actual comparison
	diffResults, err := diff.DiffArray(reconciliation.Target, reconciliation.Live, diffNormalizer)
	if err != nil {
		diffResults = &diff.DiffResultList{}
		failedToLoadObjs = true
		conditions = append(conditions, v1alpha1.ApplicationCondition{Type: v1alpha1.ApplicationConditionComparisonError, Message: err.Error(), LastTransitionTime: &now})
	}

	syncCode := v1alpha1.SyncStatusCodeSynced
	managedResources := make([]managedResource, len(reconciliation.Target))
	resourceSummaries := make([]v1alpha1.ResourceStatus, len(reconciliation.Target))
	for i, targetObj := range reconciliation.Target {
		liveObj := reconciliation.Live[i]
		obj := liveObj
		if obj == nil {
			obj = targetObj
		}
		if obj == nil {
			continue
		}
		gvk := obj.GroupVersionKind()

		resState := v1alpha1.ResourceStatus{
			Namespace:       obj.GetNamespace(),
			Name:            obj.GetName(),
			Kind:            gvk.Kind,
			Version:         gvk.Version,
			Group:           gvk.Group,
			Hook:            hookutil.IsHook(obj),
			RequiresPruning: targetObj == nil && liveObj != nil,
		}

		diffResult := diffResults.Diffs[i]
		if resState.Hook || ignore.Ignore(obj) {
			// For resource hooks, don't store sync status, and do not affect overall sync status
		} else if diffResult.Modified || targetObj == nil || liveObj == nil {
			// Set resource state to OutOfSync since one of the following is true:
			// * target and live resource are different
			// * target resource not defined and live resource is extra
			// * target resource present but live resource is missing
			resState.Status = v1alpha1.SyncStatusCodeOutOfSync
			// we ignore the status if the obj needs pruning AND we have the annotation
			needsPruning := targetObj == nil && liveObj != nil
			if !(needsPruning && resourceutil.HasAnnotationOption(obj, common.AnnotationCompareOptions, "IgnoreExtraneous")) {
				syncCode = v1alpha1.SyncStatusCodeOutOfSync
			}
		} else {
			resState.Status = v1alpha1.SyncStatusCodeSynced
		}
		// set unknown status to all resource that are not permitted in the app project
		isNamespaced, err := m.liveStateCache.IsNamespaced(app.Spec.Destination.Server, gvk.GroupKind())
		if !project.IsGroupKindPermitted(gvk.GroupKind(), isNamespaced && err == nil) {
			resState.Status = v1alpha1.SyncStatusCodeUnknown
		}

		// we can't say anything about the status if we were unable to get the target objects
		if failedToLoadObjs {
			resState.Status = v1alpha1.SyncStatusCodeUnknown
		}
		managedResources[i] = managedResource{
			Name:      resState.Name,
			Namespace: resState.Namespace,
			Group:     resState.Group,
			Kind:      resState.Kind,
			Version:   resState.Version,
			Live:      liveObj,
			Target:    targetObj,
			Diff:      diffResult,
			Hook:      resState.Hook,
		}
		resourceSummaries[i] = resState
	}

	if failedToLoadObjs {
		syncCode = v1alpha1.SyncStatusCodeUnknown
	}
	syncStatus := v1alpha1.SyncStatus{
		ComparedTo: appv1.ComparedTo{
			Source:      source,
			Destination: app.Spec.Destination,
		},
		Status: syncCode,
	}
	if manifestInfo != nil {
		syncStatus.Revision = manifestInfo.Revision
	}

	healthStatus, err := argohealth.SetApplicationHealth(resourceSummaries, GetLiveObjs(managedResources), resourceOverrides, func(obj *unstructured.Unstructured) bool {
		return !isSelfReferencedApp(app, kubeutil.GetObjectRef(obj))
	})

	if err != nil {
		conditions = append(conditions, appv1.ApplicationCondition{Type: v1alpha1.ApplicationConditionComparisonError, Message: err.Error(), LastTransitionTime: &now})
	}

	compRes := comparisonResult{
		syncStatus:           &syncStatus,
		healthStatus:         healthStatus,
		resources:            resourceSummaries,
		managedResources:     managedResources,
		reconciliationResult: reconciliation,
		diffNormalizer:       diffNormalizer,
	}
	if manifestInfo != nil {
		compRes.appSourceType = v1alpha1.ApplicationSourceType(manifestInfo.SourceType)
	}
	app.Status.SetConditions(conditions, map[appv1.ApplicationConditionType]bool{
		appv1.ApplicationConditionComparisonError:         true,
		appv1.ApplicationConditionSharedResourceWarning:   true,
		appv1.ApplicationConditionRepeatedResourceWarning: true,
		appv1.ApplicationConditionExcludedResourceWarning: true,
	})
	return &compRes
}

func (m *appStateManager) persistRevisionHistory(app *v1alpha1.Application, revision string, source v1alpha1.ApplicationSource) error {
	var nextID int64
	if len(app.Status.History) > 0 {
		nextID = app.Status.History[len(app.Status.History)-1].ID + 1
	}
	app.Status.History = append(app.Status.History, v1alpha1.RevisionHistory{
		Revision:   revision,
		DeployedAt: metav1.NewTime(time.Now().UTC()),
		ID:         nextID,
		Source:     source,
	})

	app.Status.History = app.Status.History.Trunc(app.Spec.GetRevisionHistoryLimit())

	patch, err := json.Marshal(map[string]map[string][]v1alpha1.RevisionHistory{
		"status": {
			"history": app.Status.History,
		},
	})
	if err != nil {
		return err
	}
	_, err = m.appclientset.ArgoprojV1alpha1().Applications(m.namespace).Patch(app.Name, types.MergePatchType, patch)
	return err
}

// NewAppStateManager creates new instance of AppStateManager
func NewAppStateManager(
	db db.ArgoDB,
	appclientset appclientset.Interface,
	repoClientset apiclient.Clientset,
	namespace string,
	kubectl kubeutil.Kubectl,
	settingsMgr *settings.SettingsManager,
	liveStateCache statecache.LiveStateCache,
	projInformer cache.SharedIndexInformer,
	metricsServer *metrics.MetricsServer,
) AppStateManager {
	return &appStateManager{
		liveStateCache: liveStateCache,
		db:             db,
		appclientset:   appclientset,
		kubectl:        kubectl,
		repoClientset:  repoClientset,
		namespace:      namespace,
		settingsMgr:    settingsMgr,
		projInformer:   projInformer,
		metricsServer:  metricsServer,
	}
}
