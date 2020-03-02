package controller

import (
	"context"
	"fmt"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	extv1beta1 "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"

	"github.com/zdnscloud/gok8s/cache"
	"github.com/zdnscloud/gok8s/client"
	gok8sctrl "github.com/zdnscloud/gok8s/controller"
	"github.com/zdnscloud/gok8s/event"
	"github.com/zdnscloud/gok8s/handler"
	"github.com/zdnscloud/gok8s/predicate"

	appv1beta1 "github.com/zdnscloud/application-operator/pkg/apis/app/v1beta1"
)

type ApplicationManager struct {
	appCache   *ApplicationCache
	kubeCache  cache.Cache
	kubeClient client.Client
	stopCh     chan struct{}
	lock       sync.RWMutex
}

func New(c cache.Cache, cli client.Client, eventRecorder record.EventRecorder) (*ApplicationManager, error) {
	ctrl := gok8sctrl.New("applicationCache", c, scheme.Scheme)
	ctrl.Watch(&appsv1.Deployment{})
	ctrl.Watch(&appsv1.DaemonSet{})
	ctrl.Watch(&appsv1.StatefulSet{})
	ctrl.Watch(&corev1.Service{})
	ctrl.Watch(&extv1beta1.Ingress{})
	ctrl.Watch(&corev1.ConfigMap{})
	ctrl.Watch(&corev1.Secret{})
	ctrl.Watch(&batchv1.Job{})
	ctrl.Watch(&batchv1beta1.CronJob{})

	stopCh := make(chan struct{})
	m := &ApplicationManager{
		appCache:   newApplicationCache(eventRecorder),
		kubeCache:  c,
		kubeClient: cli,
		stopCh:     stopCh,
	}
	if err := m.initApplicationManager(); err != nil {
		return nil, fmt.Errorf("init application manager failed: %s", err.Error())
	}

	go ctrl.Start(stopCh, m, predicate.NewIgnoreUnchangedUpdate())
	return m, nil
}

func (m *ApplicationManager) initApplicationManager() error {
	nses := &corev1.NamespaceList{}
	if err := m.kubeCache.List(context.TODO(), nil, nses); err != nil {
		return fmt.Errorf("list namespaces info failed: %s\n", err.Error())
	}

	for _, ns := range nses.Items {
		apps := &appv1beta1.ApplicationList{}
		if err := m.kubeCache.List(context.TODO(), &client.ListOptions{Namespace: ns.Name}, apps); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("list applications info failed: %s", err.Error())
		}

		for _, app := range apps.Items {
			m.appCache.Add(&app)
		}
	}

	return nil
}

func (m *ApplicationManager) OnCreate(e event.CreateEvent) (handler.Result, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	switch obj := e.Object.(type) {
	case *appv1beta1.Application:
		m.appCache.OnCreateApplication(m.kubeClient, obj)
	case *appsv1.Deployment:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeDeployment,
			Replicas:          int(*obj.Spec.Replicas),
			ReadyReplicas:     int(obj.Status.ReadyReplicas),
			CreationTimestamp: obj.CreationTimestamp,
		})
	case *appsv1.DaemonSet:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeDaemonSet,
			Replicas:          int(obj.Status.DesiredNumberScheduled),
			ReadyReplicas:     int(obj.Status.NumberReady),
			CreationTimestamp: obj.CreationTimestamp,
		})
	case *appsv1.StatefulSet:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeStatefulSet,
			Replicas:          int(*obj.Spec.Replicas),
			ReadyReplicas:     int(obj.Status.ReadyReplicas),
			CreationTimestamp: obj.CreationTimestamp,
		})
	case *corev1.Service:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeService,
			CreationTimestamp: obj.CreationTimestamp,
		})
	case *extv1beta1.Ingress:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeIngress,
			CreationTimestamp: obj.CreationTimestamp,
		})
	case *corev1.ConfigMap:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeConfigMap,
			CreationTimestamp: obj.CreationTimestamp,
		})
	case *corev1.Secret:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeSecret,
			CreationTimestamp: obj.CreationTimestamp,
		})
	case *batchv1.Job:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeJob,
			CreationTimestamp: obj.CreationTimestamp,
		})
	case *batchv1beta1.CronJob:
		m.appCache.OnCreateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace:         obj.Namespace,
			Name:              obj.Name,
			Type:              appv1beta1.ResourceTypeCronJob,
			CreationTimestamp: obj.CreationTimestamp,
		})
	}

	return handler.Result{}, nil
}

func (m *ApplicationManager) OnUpdate(e event.UpdateEvent) (handler.Result, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	switch obj := e.ObjectNew.(type) {
	case *appv1beta1.Application:
		if oldApp := e.ObjectOld.(*appv1beta1.Application); oldApp.GetDeletionTimestamp() == nil && obj.GetDeletionTimestamp() != nil {
			m.appCache.OnDeleteApplication(m.kubeClient, obj)
		}
	case *appsv1.Deployment:
		if oldDeploy := e.ObjectOld.(*appsv1.Deployment); oldDeploy.Status.ReadyReplicas != obj.Status.ReadyReplicas {
			m.appCache.OnUpdateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
				Namespace:         obj.Namespace,
				Name:              obj.Name,
				Type:              appv1beta1.ResourceTypeDeployment,
				Replicas:          int(*obj.Spec.Replicas),
				ReadyReplicas:     int(obj.Status.ReadyReplicas),
				CreationTimestamp: obj.CreationTimestamp,
			})
		}
	case *appsv1.DaemonSet:
		if oldDs := e.ObjectOld.(*appsv1.DaemonSet); oldDs.Status.NumberReady != obj.Status.NumberReady {
			m.appCache.OnUpdateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
				Namespace:         obj.Namespace,
				Name:              obj.Name,
				Type:              appv1beta1.ResourceTypeDaemonSet,
				Replicas:          int(obj.Status.DesiredNumberScheduled),
				ReadyReplicas:     int(obj.Status.NumberReady),
				CreationTimestamp: obj.CreationTimestamp,
			})
		}
	case *appsv1.StatefulSet:
		if oldSts := e.ObjectOld.(*appsv1.StatefulSet); oldSts.Status.ReadyReplicas != obj.Status.ReadyReplicas {
			m.appCache.OnUpdateAppResource(m.kubeClient, obj, appv1beta1.AppResource{
				Namespace:         obj.Namespace,
				Name:              obj.Name,
				Type:              appv1beta1.ResourceTypeStatefulSet,
				Replicas:          int(*obj.Spec.Replicas),
				ReadyReplicas:     int(obj.Status.ReadyReplicas),
				CreationTimestamp: obj.CreationTimestamp,
			})
		}
	}

	return handler.Result{}, nil
}

func (m *ApplicationManager) OnDelete(e event.DeleteEvent) (handler.Result, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	switch obj := e.Object.(type) {
	case *appsv1.Deployment:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeDeployment,
		})
	case *appsv1.DaemonSet:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeDaemonSet,
		})
	case *appsv1.StatefulSet:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeStatefulSet,
		})
	case *corev1.Service:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeService,
		})
	case *extv1beta1.Ingress:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeIngress,
		})
	case *corev1.ConfigMap:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeConfigMap,
		})
	case *corev1.Secret:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeSecret,
		})
	case *batchv1.Job:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeJob,
		})
	case *batchv1beta1.CronJob:
		m.appCache.OnDeleteAppResource(m.kubeClient, obj, appv1beta1.AppResource{
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Type:      appv1beta1.ResourceTypeCronJob,
		})
	}

	return handler.Result{}, nil
}

func (m *ApplicationManager) OnGeneric(e event.GenericEvent) (handler.Result, error) {
	return handler.Result{}, nil
}
