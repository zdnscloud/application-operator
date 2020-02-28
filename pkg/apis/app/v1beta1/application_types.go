package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ApplicationSpec defines the desired state of Application
type ApplicationSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "operator-sdk generate k8s" to regenerate code after modifying this file
	// Add custom validation using kubebuilder tags: https://book-v1.book.kubebuilder.io/beyond_basics/generating_crd.html
	OwnerChart        ChartInfo  `json:"ownerChart"`
	InjectServiceMesh bool       `json:"injectServiceMesh,omitempty"`
	CreatedByAdmin    bool       `json:"createdByAdmin,omitempty"`
	Manifests         []Manifest `json:"manifests,omitempty"`
	CRDManifests      []Manifest `json:"crdManifests,omitempty"`
}

type ChartInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Icon        string `json:"icon"`
	SystemChart bool   `json:"systemChart"`
}

type Manifest struct {
	File      string `json:"file,omitempty"`
	Content   string `json:"content,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

type AppResource struct {
	Namespace         string       `json:"namespace"`
	Name              string       `json:"name"`
	Type              ResourceType `json:"type"`
	Exists            bool         `json:"exists"`
	Replicas          int          `json:"replicas,omitempty"`
	ReadyReplicas     int          `json:"readyReplicas,omitempty"`
	CreationTimestamp metav1.Time  `json:"creationTimestamp,omitempty"`
}

type AppResources []AppResource

func (r AppResources) Len() int {
	return len(r)
}

func (r AppResources) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

func (r AppResources) Less(i, j int) bool {
	if r[i].Type == r[j].Type {
		return r[i].Name < r[j].Name
	} else {
		return r[i].Type < r[j].Type
	}
}

// ApplicationStatus defines the observed state of Application
type ApplicationStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "operator-sdk generate k8s" to regenerate code after modifying this file
	// Add custom validation using kubebuilder tags: https://book-v1.book.kubebuilder.io/beyond_basics/generating_crd.html
	State              ApplicationStatusState `json:"state"`
	WorkloadCount      int                    `json:"workloadCount,omitempty"`
	ReadyWorkloadCount int                    `json:"readyWorkloadCount,omitempty"`
	AppResources       AppResources           `json:"appResources,omitempty"`
}

type ApplicationStatusState string

const (
	ApplicationStatusStateCreate  ApplicationStatusState = "create"
	ApplicationStatusStateDelete  ApplicationStatusState = "delete"
	ApplicationStatusStateSucceed ApplicationStatusState = "succeed"
	ApplicationStatusStateFailed  ApplicationStatusState = "failed"
)

type ResourceType string

const (
	ResourceTypeDeployment  ResourceType = "deployment"
	ResourceTypeDaemonSet   ResourceType = "daemonset"
	ResourceTypeStatefulSet ResourceType = "statefulset"
	ResourceTypeJob         ResourceType = "job"
	ResourceTypeCronJob     ResourceType = "cronjob"
	ResourceTypeConfigMap   ResourceType = "configmap"
	ResourceTypeSecret      ResourceType = "secret"
	ResourceTypeService     ResourceType = "service"
	ResourceTypeIngress     ResourceType = "ingress"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Application is the Schema for the applications API
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=applications,scope=Namespaced
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApplicationSpec   `json:"spec,omitempty"`
	Status ApplicationStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ApplicationList contains a list of Application
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}
