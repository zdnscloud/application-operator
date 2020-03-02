package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"github.com/zdnscloud/cement/log"
	"github.com/zdnscloud/cement/slice"
	"github.com/zdnscloud/gok8s/client"
	"github.com/zdnscloud/gok8s/helper"

	appv1beta1 "github.com/zdnscloud/application-operator/pkg/apis/app/v1beta1"
)

var (
	supportWorkloadTypes = []string{string(appv1beta1.ResourceTypeDeployment), string(appv1beta1.ResourceTypeDaemonSet), string(appv1beta1.ResourceTypeStatefulSet)}
	supportResourceTypes = append(supportWorkloadTypes, string(appv1beta1.ResourceTypeCronJob), string(appv1beta1.ResourceTypeJob), string(appv1beta1.ResourceTypeConfigMap), string(appv1beta1.ResourceTypeSecret), string(appv1beta1.ResourceTypeService), string(appv1beta1.ResourceTypeIngress))
)

const (
	AnnKeyForInjectServiceMesh = "linkerd.io/inject"

	zcloudAppFinalizer = "app.zcloud.cn.v1beta1/finalizer"
	crdCheckTimes      = 30
	crdCheckInterval   = 2 * time.Second

	eventReasonEmpty              = ""
	eventReasonCreateResources    = "CreateAppResourcesFailed"
	eventReasonCreateCRD          = "CreateCRDFailed"
	eventReasonUpdateAppStatus    = "UpdateAppStatusFailed"
	eventReasonAddAppFinalizer    = "AddAppFinalizerFailed"
	eventReasonDeleteAppFinalizer = "DeleteAppFinalizerFailed"
	eventReasonDeleteResources    = "DeleteAppResourcesFailed"
	eventReasonNoFoundApp         = "NoFoundApp"
	eventReasonNoFoundAppResource = "NoFoundAppResource"
)

type ApplicationInfo struct {
	Namespace string
	Name      string
}

type ApplicationCache struct {
	eventRecorder     record.EventRecorder
	nsAndApplications map[string]map[string]appv1beta1.AppResources
	nsAndAppResources map[string]map[string]*ApplicationInfo
}

func newApplicationCache(eventRecorder record.EventRecorder) *ApplicationCache {
	return &ApplicationCache{
		eventRecorder:     eventRecorder,
		nsAndApplications: make(map[string]map[string]appv1beta1.AppResources),
		nsAndAppResources: make(map[string]map[string]*ApplicationInfo),
	}
}

func (ac *ApplicationCache) Add(app *appv1beta1.Application) {
	appAndResources, ok := ac.nsAndApplications[app.Namespace]
	if ok == false {
		appAndResources = make(map[string]appv1beta1.AppResources)
		ac.nsAndApplications[app.Namespace] = appAndResources
	}
	appAndResources[app.Name] = app.Status.AppResources

	for _, resource := range app.Status.AppResources {
		resourcesAndApps, ok := ac.nsAndAppResources[resource.Namespace]
		if ok == false {
			resourcesAndApps = make(map[string]*ApplicationInfo)
			ac.nsAndAppResources[resource.Namespace] = resourcesAndApps
		}
		resourcesAndApps[genResourceID(resource.Type, resource.Name)] = &ApplicationInfo{
			Namespace: app.Namespace,
			Name:      app.Name,
		}
	}
}

func genResourceID(resourceType appv1beta1.ResourceType, resourceName string) string {
	return string(resourceType) + "/" + resourceName
}

func (ac *ApplicationCache) OnCreateApplication(cli client.Client, app *appv1beta1.Application) {
	if _, ok := ac.getAppResources(app.Namespace, app.Name); ok {
		return
	}

	if reason, err := ac.onCreateApplication(cli, app); err != nil {
		log.Warnf("on create app %s with namespace %s failed: reason: %s and error: %s", app.Name, app.Namespace, reason, err.Error())
		ac.eventRecorder.Event(app, corev1.EventTypeWarning, reason, err.Error())
	}
}

func (ac *ApplicationCache) onCreateApplication(cli client.Client, app *appv1beta1.Application) (string, error) {
	app.Status.State = appv1beta1.ApplicationStatusStateCreate
	if err := cli.Status().Update(context.TODO(), app); err != nil {
		return eventReasonUpdateAppStatus, fmt.Errorf("update app.status.state to create failed: %s", err.Error())
	}

	if err := preInstallChartCRDs(cli, app.Spec.CRDManifests); err != nil {
		return eventReasonCreateCRD, err
	}

	helper.AddFinalizer(app, zcloudAppFinalizer)
	if err := cli.Update(context.TODO(), app); err != nil {
		return eventReasonAddAppFinalizer, err
	}

	defer ac.Add(app)
	if err := createAppResources(cli, app); err != nil {
		app.Status.State = appv1beta1.ApplicationStatusStateFailed
		if err := cli.Status().Update(context.TODO(), app); err != nil {
			log.Warnf("after create app %s with namespace %s resources failed, update app status failed: %s",
				app.Name, app.Namespace, err.Error())
		}
		return eventReasonCreateResources, err
	}

	app.Status.State = appv1beta1.ApplicationStatusStateSucceed
	if err := cli.Status().Update(context.TODO(), app); err != nil {
		return eventReasonUpdateAppStatus, fmt.Errorf("update app.status failed: %s", err.Error())
	}

	return eventReasonEmpty, nil
}

func (ac *ApplicationCache) getAppResources(namespace, name string) (appv1beta1.AppResources, bool) {
	if appAndResources, ok := ac.nsAndApplications[namespace]; ok {
		if rs, ok := appAndResources[name]; ok {
			return rs, true
		}
	}

	return nil, false
}

func preInstallChartCRDs(cli client.Client, crdManifests []appv1beta1.Manifest) error {
	if len(crdManifests) == 0 {
		return nil
	}

	var crds []*apiextv1beta1.CustomResourceDefinition
	for _, manifest := range crdManifests {
		if err := helper.MapOnRuntimeObject(manifest.Content, func(ctx context.Context, obj runtime.Object) error {
			crd, ok := obj.(*apiextv1beta1.CustomResourceDefinition)
			if !ok {
				return fmt.Errorf("runtime object isn't k8s crd object with file: %s", manifest.File)
			}
			crds = append(crds, crd)

			if err := cli.Create(ctx, obj); err != nil {
				if apierrors.IsAlreadyExists(err) == false {
					return fmt.Errorf("create crd with file %s failed: %s", manifest.File, err.Error())
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}

	if !waitCRDsReady(cli, crds) {
		return fmt.Errorf("wait chart crds to ready timeout")
	}

	return nil
}

func waitCRDsReady(cli client.Client, requiredCRDs []*apiextv1beta1.CustomResourceDefinition) bool {
	for i := 0; i < crdCheckTimes; i++ {
		if isCRDsReady(cli, requiredCRDs) {
			return true
		}
		time.Sleep(crdCheckInterval)
	}
	return false
}

func isCRDsReady(cli client.Client, requiredCRDs []*apiextv1beta1.CustomResourceDefinition) bool {
	var crds apiextv1beta1.CustomResourceDefinitionList
	if err := cli.List(context.TODO(), nil, &crds); err != nil {
		return false
	}

	for _, required := range requiredCRDs {
		ready := false
		for _, crd := range crds.Items {
			if crd.Name == required.Name {
				if isCRDReady(crd) {
					ready = true
				}
				break
			}
		}
		if !ready {
			return false
		}
	}

	return true
}

func isCRDReady(crd apiextv1beta1.CustomResourceDefinition) bool {
	for _, cond := range crd.Status.Conditions {
		switch cond.Type {
		case apiextv1beta1.Established:
			if cond.Status == apiextv1beta1.ConditionTrue {
				return true
			}
		case apiextv1beta1.NamesAccepted:
			if cond.Status == apiextv1beta1.ConditionFalse {
				return true
			}
		}
	}
	return false
}

func createAppResources(cli client.Client, app *appv1beta1.Application) error {
	for i, manifest := range app.Spec.Manifests {
		if err := helper.MapOnRuntimeObject(manifest.Content, func(ctx context.Context, obj runtime.Object) error {
			if obj == nil {
				return fmt.Errorf("cann`t unmarshal file %s to k8s runtime object\n", manifest.File)
			}

			gvk := obj.GetObjectKind().GroupVersionKind()
			metaObj, err := runtimeObjectToMetaObject(obj, app.Namespace, app.Spec.CreatedByAdmin)
			if err != nil {
				return fmt.Errorf("runtime object to meta object with chart file %s failed: %s", manifest.File, err.Error())
			}

			typ := strings.ToLower(gvk.Kind)
			injectServiceMeshToWorkload(typ, app, obj)
			if err := cli.Create(ctx, obj); err != nil {
				if apierrors.IsAlreadyExists(err) {
					app.Spec.Manifests[i].Duplicate = true
				}
				return fmt.Errorf("create resource with file %s failed: %s", manifest.File, err.Error())
			}

			if slice.SliceIndex(supportResourceTypes, typ) != -1 {
				if slice.SliceIndex(supportWorkloadTypes, typ) != -1 {
					app.Status.WorkloadCount += 1
				}
				app.Status.AppResources = append(app.Status.AppResources, appv1beta1.AppResource{
					Namespace: metaObj.GetNamespace(),
					Name:      metaObj.GetName(),
					Type:      appv1beta1.ResourceType(typ),
					Exists:    true,
				})
			}
			return nil
		}); err != nil {
			return err
		}
	}

	sort.Sort(app.Status.AppResources)
	return nil
}

func runtimeObjectToMetaObject(obj runtime.Object, namespace string, createdByAdmin bool) (metav1.Object, error) {
	metaObj, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}

	if metaObj.GetNamespace() != "" {
		if createdByAdmin == false {
			return nil, fmt.Errorf("chart file should not has namespace with current user")
		}
	} else {
		metaObj.SetNamespace(namespace)
	}

	return metaObj, nil
}

func injectServiceMeshToWorkload(typ string, app *appv1beta1.Application, obj runtime.Object) {
	if slice.SliceIndex(supportWorkloadTypes, typ) == -1 || app.Spec.InjectServiceMesh == false {
		return
	}

	switch obj.(type) {
	case *appsv1.Deployment:
		deploy := obj.(*appsv1.Deployment)
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = make(map[string]string)
		}
		deploy.Spec.Template.Annotations[AnnKeyForInjectServiceMesh] = "enabled"
	case *appsv1.DaemonSet:
		ds := obj.(*appsv1.DaemonSet)
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = make(map[string]string)
		}
		ds.Spec.Template.Annotations[AnnKeyForInjectServiceMesh] = "enabled"
	case *appsv1.StatefulSet:
		sts := obj.(*appsv1.StatefulSet)
		if sts.Spec.Template.Annotations == nil {
			sts.Spec.Template.Annotations = make(map[string]string)
		}
		sts.Spec.Template.Annotations[AnnKeyForInjectServiceMesh] = "enabled"
	}
}

func (ac *ApplicationCache) OnCreateAppResource(cli client.Client, obj runtime.Object, resource appv1beta1.AppResource) {
	if reason, err := ac.onCreateAppResource(cli, obj, resource); err != nil {
		log.Warnf("update app status failed when recv create resource %s/%s/%s event: reason: %s and error: %s",
			resource.Namespace, string(resource.Type), resource.Name, reason, err.Error())
		ac.eventRecorder.Event(obj, corev1.EventTypeWarning, reason, err.Error())
	}
}

func (ac *ApplicationCache) onCreateAppResource(cli client.Client, obj runtime.Object, resource appv1beta1.AppResource) (string, error) {
	appInfo, found := ac.getAppInfo(resource.Type, resource.Namespace, resource.Name)
	if found == false {
		return eventReasonEmpty, nil
	}

	return ac.updateAppStatus(cli, resource, appInfo)
}

func (ac *ApplicationCache) updateAppStatus(cli client.Client, resource appv1beta1.AppResource, appInfo *ApplicationInfo) (string, error) {
	app := &appv1beta1.Application{}
	if err := cli.Get(context.TODO(), k8stypes.NamespacedName{appInfo.Namespace, appInfo.Name}, app); err != nil {
		return eventReasonNoFoundApp, fmt.Errorf("get app %s/%s failed: %s", appInfo.Namespace, appInfo.Name, err.Error())
	}

	oldIsReady, ok := updateAppResources(resource, app.Status.AppResources)
	if ok == false {
		return eventReasonNoFoundAppResource, fmt.Errorf("no found resource for app %s/%s", appInfo.Namespace, appInfo.Name)
	}

	if slice.SliceIndex(supportWorkloadTypes, string(resource.Type)) != -1 {
		if resource.Replicas > resource.ReadyReplicas {
			if oldIsReady {
				app.Status.ReadyWorkloadCount -= 1
			}
		} else if oldIsReady == false {
			app.Status.ReadyWorkloadCount += 1
		}
	}

	if err := cli.Status().Update(context.TODO(), app); err != nil {
		return eventReasonUpdateAppStatus, fmt.Errorf("update app %s/%s status failed: %s", appInfo.Namespace, appInfo.Name, err.Error())
	}

	return eventReasonEmpty, nil
}

func (ac *ApplicationCache) getAppInfo(typ appv1beta1.ResourceType, resourceNamespace, resourceName string) (*ApplicationInfo, bool) {
	if resourcesAndApps, ok := ac.nsAndAppResources[resourceNamespace]; ok {
		if appInfo, ok := resourcesAndApps[genResourceID(typ, resourceName)]; ok {
			return appInfo, true
		}
	}
	return nil, false
}

func updateAppResources(resource appv1beta1.AppResource, resources appv1beta1.AppResources) (bool, bool) {
	for i, r := range resources {
		if r.Namespace == resource.Namespace && r.Type == resource.Type && r.Name == resource.Name {
			resources[i].Replicas = resource.Replicas
			resources[i].ReadyReplicas = resource.ReadyReplicas
			resources[i].CreationTimestamp = resource.CreationTimestamp
			return r.ReadyReplicas != 0 && r.Replicas <= r.ReadyReplicas, true
		}
	}

	return false, false
}

func (ac *ApplicationCache) OnUpdateAppResource(cli client.Client, obj runtime.Object, resource appv1beta1.AppResource) {
	if reason, err := ac.onCreateAppResource(cli, obj, resource); err != nil {
		log.Warnf("update app status failed when recv update resource %s/%s/%s event: reason: %s and error: %s",
			resource.Namespace, string(resource.Type), resource.Name, reason, err.Error())
		ac.eventRecorder.Event(obj, corev1.EventTypeWarning, reason, err.Error())
	}
}

func (ac *ApplicationCache) OnDeleteApplication(cli client.Client, app *appv1beta1.Application) {
	if _, ok := ac.getAppResources(app.Namespace, app.Name); ok == false {
		return
	}

	if reason, err := ac.onDeleteApplication(cli, app); err != nil {
		log.Warnf("on delete app %s with namespace %s failed: reason: %s and error: %s", app.Name, app.Namespace, reason, err.Error())
		ac.eventRecorder.Event(app, corev1.EventTypeWarning, reason, err.Error())
	}
}

func (ac *ApplicationCache) onDeleteApplication(cli client.Client, app *appv1beta1.Application) (string, error) {
	if err := ac.deleteAppResources(cli, app); err != nil {
		app.Status.State = appv1beta1.ApplicationStatusStateFailed
		if err := cli.Status().Update(context.TODO(), app); err != nil {
			log.Warnf("update app %s state to failed with namespace %s failed: %s", app.Name, app.Namespace, err.Error())
		}
		return eventReasonDeleteResources, err
	}

	return ac.deleteApplication(cli, app)
}

func (ac *ApplicationCache) deleteApplication(cli client.Client, app *appv1beta1.Application) (string, error) {
	if len(ac.nsAndApplications[app.Namespace][app.Name]) == 0 {
		if helper.HasFinalizer(app, zcloudAppFinalizer) {
			helper.RemoveFinalizer(app, zcloudAppFinalizer)
			if err := cli.Update(context.TODO(), app); err != nil {
				return eventReasonDeleteAppFinalizer, err
			}
		}

		delete(ac.nsAndApplications[app.Namespace], app.Name)
	}

	return eventReasonEmpty, nil
}

func (ac *ApplicationCache) deleteAppResources(cli client.Client, app *appv1beta1.Application) error {
	for _, manifest := range app.Spec.Manifests {
		if manifest.Duplicate {
			continue
		}

		if err := helper.MapOnRuntimeObject(manifest.Content, func(ctx context.Context, obj runtime.Object) error {
			gvk := obj.GetObjectKind().GroupVersionKind()
			metaObj, err := runtimeObjectToMetaObject(obj, app.Namespace, true)
			if err != nil {
				return fmt.Errorf("runtime object to meta object with file %s failed: %s", manifest.File, err.Error())
			}

			typ := strings.ToLower(gvk.Kind)
			if slice.SliceIndex(supportResourceTypes, typ) != -1 {
				if _, ok := ac.getAppInfo(appv1beta1.ResourceType(typ), metaObj.GetNamespace(), metaObj.GetName()); ok == false {
					return nil
				}
			}

			if err := cli.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
				if apierrors.IsNotFound(err) == false {
					return fmt.Errorf("delete resource with file %s failed: %s", manifest.File, err.Error())
				}
			}

			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

func (ac *ApplicationCache) OnDeleteAppResource(cli client.Client, obj runtime.Object, resource appv1beta1.AppResource) {
	appInfo, rs, found := ac.getAppInfoAndResources(resource)
	if found == false {
		return
	}

	for i, r := range rs {
		if r.Namespace == resource.Namespace && r.Type == resource.Type && r.Name == resource.Name {
			rs = append(rs[:i], rs[i+1:]...)
			break
		}
	}

	if reason, err := ac.onDeleteAppResource(cli, resource, appInfo, rs); err != nil {
		log.Warnf("update app status failed when recv delete resource %s/%s/%s event: reason: %s and error: %s",
			resource.Namespace, string(resource.Type), resource.Name, reason, err.Error())
		ac.eventRecorder.Event(obj, corev1.EventTypeWarning, reason, err.Error())
	}
}

func (ac *ApplicationCache) onDeleteAppResource(cli client.Client, resource appv1beta1.AppResource, appInfo *ApplicationInfo, rs appv1beta1.AppResources) (string, error) {
	app := &appv1beta1.Application{}
	if err := cli.Get(context.TODO(), k8stypes.NamespacedName{appInfo.Namespace, appInfo.Name}, app); err != nil {
		return eventReasonNoFoundApp, fmt.Errorf("get app %s/%s failed: %s", appInfo.Namespace, appInfo.Name, err.Error())
	}

	oldIsReady, ok := updateAppResources(resource, app.Status.AppResources)
	if ok == false {
		log.Warnf("no found resource %s/%s with namespace %s in application %s/%s status.AppResources",
			string(resource.Type), resource.Name, resource.Namespace, appInfo.Namespace, appInfo.Name)
	} else {
		if slice.SliceIndex(supportWorkloadTypes, string(resource.Type)) != -1 {
			if oldIsReady {
				app.Status.ReadyWorkloadCount -= 1
			}
		}

		if err := cli.Status().Update(context.TODO(), app); err != nil {
			return eventReasonUpdateAppStatus, fmt.Errorf("update app %s/%s status failed: %s",
				appInfo.Namespace, appInfo.Name, err.Error())
		}
	}

	ac.nsAndApplications[appInfo.Namespace][appInfo.Name] = rs
	delete(ac.nsAndAppResources[resource.Namespace], genResourceID(resource.Type, resource.Name))
	return ac.deleteApplication(cli, app)
}

func (ac *ApplicationCache) getAppInfoAndResources(resource appv1beta1.AppResource) (*ApplicationInfo, appv1beta1.AppResources, bool) {
	appInfo, ok := ac.getAppInfo(resource.Type, resource.Namespace, resource.Name)
	if ok == false {
		return nil, nil, false
	}

	resources, ok := ac.getAppResources(appInfo.Namespace, appInfo.Name)
	if ok == false {
		log.Warnf("no found app %s with namespace %s", appInfo.Name, appInfo.Namespace)
		return nil, nil, false
	}

	rs := make(appv1beta1.AppResources, len(resources))
	copy(rs, resources)
	return appInfo, rs, true
}
