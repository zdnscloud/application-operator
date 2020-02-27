package controller

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	extv1beta1 "k8s.io/api/extensions/v1beta1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/zdnscloud/cement/log"
	"github.com/zdnscloud/cement/slice"
	"github.com/zdnscloud/gok8s/client"
	"github.com/zdnscloud/gok8s/helper"
	restutil "github.com/zdnscloud/gorest/util"

	appv1beta1 "github.com/zdnscloud/application-operator/pkg/apis/app/v1beta1"
)

var (
	supportWorkloadTypes = []string{string(appv1beta1.ResourceTypeDeployment), string(appv1beta1.ResourceTypeDaemonSet), string(appv1beta1.ResourceTypeStatefulSet)}
	supportResourceTypes = append(supportWorkloadTypes, string(appv1beta1.ResourceTypeCronJob), string(appv1beta1.ResourceTypeJob), string(appv1beta1.ResourceTypeConfigMap), string(appv1beta1.ResourceTypeSecret), string(appv1beta1.ResourceTypeService), string(appv1beta1.ResourceTypeIngress))
)

const (
	AnnKeyForInjectServiceMesh = "linkerd.io/inject"

	zcloudAppFinalizer        = "app.zcloud.cn.v1beta1/finalizer"
	ZcloudAppRequestUrlPrefix = "app.zcloud.cn.v1beta1/url-prefix"

	crdCheckTimes    = 30
	crdCheckInterval = 2 * time.Second
)

type ApplicationInfo struct {
	Namespace string
	Name      string
}

type ApplicationCache struct {
	nsAndApplications map[string]map[string]appv1beta1.AppResources
	nsAndAppResources map[string]map[string]*ApplicationInfo
}

func newApplicationCache() *ApplicationCache {
	return &ApplicationCache{
		nsAndApplications: make(map[string]map[string]appv1beta1.AppResources),
		nsAndAppResources: make(map[string]map[string]*ApplicationInfo),
	}
}

func (ac *ApplicationCache) Add(app *appv1beta1.Application) {
	appAndResources, ok := ac.nsAndApplications[app.Namespace]
	if ok == false {
		appAndResources = make(map[string]appv1beta1.AppResources)
		ac.nsAndApplications[app.Namespace] = appAndResources
	} else if _, ok := appAndResources[app.Name]; ok {
		return
	}
	appAndResources[app.Name] = app.Status.AppResources

	for _, resource := range app.Status.AppResources {
		resourcesAndApps, ok := ac.nsAndAppResources[resource.Namespace]
		if ok == false {
			resourcesAndApps = make(map[string]*ApplicationInfo)
			ac.nsAndAppResources[resource.Namespace] = resourcesAndApps
		}
		resourcesAndApps[genResourceID(resource)] = &ApplicationInfo{
			Namespace: app.Namespace,
			Name:      app.Name,
		}
	}
}

func genResourceID(resource appv1beta1.AppResource) string {
	return string(resource.Type) + "/" + resource.Name
}

func (ac *ApplicationCache) OnCreateApplication(cli client.Client, app *appv1beta1.Application) {
	if _, ok := ac.getAppResources(app.Namespace, app.Name); ok {
		return
	}

	app.Status.State = appv1beta1.ApplicationStatusStateCreate
	if err := cli.Status().Update(context.TODO(), app); err != nil {
		log.Warnf("update app %s with namespace %s status to create failed: %s", app.Name, app.Namespace, err.Error())
		return
	}

	if err := createAppResources(cli, app); err != nil {
		log.Warnf("create app %s resources with namespace %s failed: %s", app.Name, app.Namespace, err.Error())
		app.Status.State = appv1beta1.ApplicationStatusStateFailed
		if err := cli.Status().Update(context.TODO(), app); err != nil {
			log.Warnf("update app %s state to failed with namespace %s failed: %s", app.Name, app.Namespace, err.Error())
		}
		return
	}

	helper.AddFinalizer(app, zcloudAppFinalizer)
	if err := cli.Update(context.TODO(), app); err != nil {
		log.Warnf("add finalizer to app %s with namespace %s failed: %s", app.Name, app.Namespace, err.Error())
		return
	}

	app.Status.State = appv1beta1.ApplicationStatusStateSucceed
	if err := cli.Status().Update(context.TODO(), app); err != nil {
		log.Warnf("update app %s status with namespace %s after create app resources failed: %s", app.Name, app.Namespace, err.Error())
		return
	}

	ac.Add(app)
}

func (ac *ApplicationCache) getAppResources(namespace, name string) (appv1beta1.AppResources, bool) {
	if appAndResources, ok := ac.nsAndApplications[namespace]; ok {
		if rs, ok := appAndResources[name]; ok {
			return rs, true
		}
	}

	return nil, false
}

func createAppResources(cli client.Client, app *appv1beta1.Application) error {
	if err := preInstallChartCRDs(cli, app.Spec.CRDManifests); err != nil {
		return fmt.Errorf("create crds for chart %s failed: %s", app.Spec.OwnerChart.Name, err.Error())
	}

	return createResources(cli, app)
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

func createResources(cli client.Client, app *appv1beta1.Application) error {
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
	case *appsv1beta1.Deployment:
		deploy := obj.(*appsv1beta1.Deployment)
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = make(map[string]string)
		}
		deploy.Spec.Template.Annotations[AnnKeyForInjectServiceMesh] = "enabled"
	case *appsv1beta2.Deployment:
		deploy := obj.(*appsv1beta2.Deployment)
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = make(map[string]string)
		}
		deploy.Spec.Template.Annotations[AnnKeyForInjectServiceMesh] = "enabled"
	case *extv1beta1.Deployment:
		deploy := obj.(*extv1beta1.Deployment)
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
	case *appsv1beta2.DaemonSet:
		ds := obj.(*appsv1beta2.DaemonSet)
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = make(map[string]string)
		}
		ds.Spec.Template.Annotations[AnnKeyForInjectServiceMesh] = "enabled"
	case *extv1beta1.DaemonSet:
		ds := obj.(*extv1beta1.DaemonSet)
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
	case *appsv1beta1.StatefulSet:
		sts := obj.(*appsv1beta1.StatefulSet)
		if sts.Spec.Template.Annotations == nil {
			sts.Spec.Template.Annotations = make(map[string]string)
		}
		sts.Spec.Template.Annotations[AnnKeyForInjectServiceMesh] = "enabled"
	case *appsv1beta2.StatefulSet:
		sts := obj.(*appsv1beta2.StatefulSet)
		if sts.Spec.Template.Annotations == nil {
			sts.Spec.Template.Annotations = make(map[string]string)
		}
		sts.Spec.Template.Annotations[AnnKeyForInjectServiceMesh] = "enabled"
	}
}

func (ac *ApplicationCache) OnCreateAppResource(cli client.Client, resource appv1beta1.AppResource) {
	appInfo, found := ac.getAppInfo(resource)
	if found == false {
		return
	}

	app := &appv1beta1.Application{}
	if err := cli.Get(context.TODO(), k8stypes.NamespacedName{appInfo.Namespace, appInfo.Name}, app); err != nil {
		log.Warnf("get app %s with namespace %s failed: %s", appInfo.Name, appInfo.Namespace, err.Error())
		return
	}

	if prefix, ok := app.Annotations[ZcloudAppRequestUrlPrefix]; ok {
		resource.Link = path.Join(prefix, resource.Namespace, restutil.GuessPluralName(string(resource.Type)), resource.Name)
	}

	resource.Exists = true
	updateAppResources(resource, app.Status.AppResources)
	if app.Status.WorkloadCount != app.Status.ReadyWorkloadCount {
		if slice.SliceIndex(supportWorkloadTypes, string(resource.Type)) != -1 && resource.Replicas == resource.ReadyReplicas {
			app.Status.ReadyWorkloadCount += 1
		}
	}

	if err := cli.Status().Update(context.TODO(), app); err != nil {
		log.Warnf("update app %s with namespace %s after create resource %s with namespace %s failed: %s",
			appInfo.Name, appInfo.Namespace, resource.Name, resource.Namespace, err.Error())
	}
}

func (ac *ApplicationCache) getAppInfo(resource appv1beta1.AppResource) (*ApplicationInfo, bool) {
	if resourcesAndApps, ok := ac.nsAndAppResources[resource.Namespace]; ok {
		if appInfo, ok := resourcesAndApps[genResourceID(resource)]; ok {
			return appInfo, true
		}
	}
	return nil, false
}

func updateAppResources(resource appv1beta1.AppResource, resources appv1beta1.AppResources) {
	for i, r := range resources {
		if r.Namespace == resource.Namespace && r.Type == resource.Type && r.Name == resource.Name {
			resources[i].Replicas = resource.Replicas
			resources[i].ReadyReplicas = resource.ReadyReplicas
			resources[i].CreationTimestamp = resource.CreationTimestamp
			resources[i].Exists = resource.Exists
			resources[i].Link = resource.Link
			break
		}
	}
}

func (ac *ApplicationCache) OnUpdateAppResource(cli client.Client, resource appv1beta1.AppResource) {
	ac.OnCreateAppResource(cli, resource)
}

func (ac *ApplicationCache) OnDeleteApplication(cli client.Client, app *appv1beta1.Application) {
	if _, ok := ac.getAppResources(app.Namespace, app.Name); ok == false {
		return
	}

	if len(app.Status.AppResources) != 0 {
		if err := deleteResources(cli, app); err != nil {
			log.Warnf("delete application %s resources with namespace %s failed: %s", app.Name, app.Namespace, err.Error())
			app.Status.State = appv1beta1.ApplicationStatusStateFailed
			if err := cli.Status().Update(context.TODO(), app); err != nil {
				log.Warnf("update app %s state to failed with namespace %s failed: %s", app.Name, app.Namespace, err.Error())
			}
			return
		}
	}

	if len(ac.nsAndApplications[app.Namespace][app.Name]) == 0 {
		delete(ac.nsAndApplications[app.Namespace], app.Name)
	}
}

func deleteResources(cli client.Client, app *appv1beta1.Application) error {
	for _, manifest := range app.Spec.Manifests {
		if manifest.Duplicate {
			continue
		}

		if err := helper.MapOnRuntimeObject(manifest.Content, func(ctx context.Context, obj runtime.Object) error {
			_, err := runtimeObjectToMetaObject(obj, app.Namespace, true)
			if err != nil {
				return fmt.Errorf("runtime object to meta object with file %s failed: %s", manifest.File, err.Error())
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

func (ac *ApplicationCache) OnDeleteAppResource(cli client.Client, resource appv1beta1.AppResource) {
	appInfo, resources, found := ac.getAppInfoAndResources(resource)
	if found == false {
		return
	}

	for i, r := range resources {
		if r.Namespace == resource.Namespace && r.Type == resource.Type && r.Name == resource.Name {
			resources = append(resources[:i], resources[i+1:]...)
			break
		}
	}

	app := &appv1beta1.Application{}
	if err := cli.Get(context.TODO(), k8stypes.NamespacedName{appInfo.Namespace, appInfo.Name}, app); err != nil {
		log.Warnf("get app %s with namespace %s failed: %s", appInfo.Name, appInfo.Namespace, err.Error())
		return
	}

	if slice.SliceIndex(supportWorkloadTypes, string(resource.Type)) != -1 {
		app.Status.ReadyWorkloadCount -= 1
	}

	updateAppResources(resource, app.Status.AppResources)
	if err := cli.Status().Update(context.TODO(), app); err != nil {
		log.Warnf("update app %s with namespace %s after delete resource %s with namespace %s failed: %s",
			appInfo.Name, appInfo.Namespace, resource.Name, resource.Namespace, err.Error())
		return
	}

	ac.nsAndApplications[appInfo.Namespace][appInfo.Name] = resources
	delete(ac.nsAndAppResources[resource.Namespace], genResourceID(resource))
	if len(resources) == 0 {
		helper.RemoveFinalizer(app, zcloudAppFinalizer)
		if err := cli.Update(context.TODO(), app); err != nil {
			log.Warnf("update app %s with namespace %s after delete app resources failed: %s", app.Name, app.Namespace, err.Error())
		}
	}
}

func (ac *ApplicationCache) getAppInfoAndResources(resource appv1beta1.AppResource) (*ApplicationInfo, appv1beta1.AppResources, bool) {
	appInfo, ok := ac.getAppInfo(resource)
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
