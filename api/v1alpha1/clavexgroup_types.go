/*
Copyright 2026.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ClavexGroupSpec mirrors cmd/clavexctl/iac.go: GroupSpec. Unlike ClavexRole
// (immutable), the group's role membership is actively reconciled on every
// pass: the controller resolves each entry in Roles (a role *name*, not
// ID — mirrors GroupSpec.Roles) to its live role ID via a List call, then
// calls GroupService.AssignRole/RemoveRole to converge membership to
// exactly the set requested. A role name that doesn't yet exist in the org
// is treated as a (transient) reconcile error and retried — this lets a
// ClavexGroup and its ClavexRole dependencies be applied in any order.
type ClavexGroupSpec struct {
	// orgRef identifies the Clavex organisation that owns this group, for
	// display/human reference only — see ClavexClientSpec.OrgRef for why
	// it cannot be used to resolve the org for API calls.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	OrgRef string `json:"orgRef"`

	// authSecretRef points to a Secret containing the org-scoped Admin API
	// credentials used to reconcile this group. See
	// ClavexClientSpec.AuthSecretRef for the required Secret shape
	// (apiKey + orgId keys).
	//
	// +required
	AuthSecretRef SecretRef `json:"authSecretRef"`

	// name is the stable reconciliation key: since the Admin API assigns
	// its own opaque ID on creation, the controller looks up existing
	// groups by this name to decide between create and update.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// roles lists the *names* (not IDs) of the ClavexRoles that should be
	// assigned to this group. The controller resolves each name to a live
	// role ID on every reconcile and converges membership exactly —
	// entries removed from this list are unassigned from the group.
	//
	// +optional
	Roles []string `json:"roles,omitempty"`
}

// ClavexGroupStatus defines the observed state of ClavexGroup.
type ClavexGroupStatus struct {
	// conditions represent the current state of the ClavexGroup resource.
	// Expected types: "Ready", "Synced".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clavexGroupId is the Admin API's opaque UUID for this group, once
	// synced.
	// +optional
	ClavexGroupID string `json:"clavexGroupId,omitempty"`

	// lastSyncedAt is the RFC3339 timestamp of the last successful
	// reconciliation against the Admin API.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvxgroup,categories=clavex
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.orgRef`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClavexGroup is the Schema for the clavexgroups API
type ClavexGroup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClavexGroup
	// +required
	Spec ClavexGroupSpec `json:"spec"`

	// status defines the observed state of ClavexGroup
	// +optional
	Status ClavexGroupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClavexGroupList contains a list of ClavexGroup
type ClavexGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClavexGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClavexGroup{}, &ClavexGroupList{})
		return nil
	})
}
