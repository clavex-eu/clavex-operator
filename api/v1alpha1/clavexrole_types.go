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

// ClavexRoleSpec mirrors cmd/clavexctl/iac.go: RoleSpec. Roles have no
// updateable fields on the Admin API beyond name+description at creation
// time (RoleService exposes only Create/Delete plus assignment/child
// helpers — no Update), so once created a role's Name/Description are
// immutable from this controller's perspective: changing them in the CR
// spec has no effect on the live resource (mirrors clavexctl org apply's
// "Roles have no updateable fields beyond name so skip update" behaviour).
// To rename or redescribe a role, delete the CR (or the role in the Admin
// API) and recreate it.
type ClavexRoleSpec struct {
	// orgRef identifies the Clavex organisation that owns this role, for
	// display/human reference only — see ClavexClientSpec.OrgRef for why
	// it cannot be used to resolve the org for API calls.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	OrgRef string `json:"orgRef"`

	// authSecretRef points to a Secret containing the org-scoped Admin API
	// credentials used to reconcile this role. See
	// ClavexClientSpec.AuthSecretRef for the required Secret shape
	// (apiKey + orgId keys).
	//
	// +required
	AuthSecretRef SecretRef `json:"authSecretRef"`

	// name is the stable reconciliation key: since the Admin API assigns
	// its own opaque ID on creation, the controller looks up existing
	// roles by this name to decide between create and no-op (roles are
	// immutable once created — see the type doc above).
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +optional
	Description string `json:"description,omitempty"`
}

// ClavexRoleStatus defines the observed state of ClavexRole.
type ClavexRoleStatus struct {
	// conditions represent the current state of the ClavexRole resource.
	// Expected types: "Ready", "Synced".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clavexRoleId is the Admin API's opaque UUID for this role, once
	// synced.
	// +optional
	ClavexRoleID string `json:"clavexRoleId,omitempty"`

	// lastSyncedAt is the RFC3339 timestamp of the last successful
	// reconciliation against the Admin API.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvxrole,categories=clavex
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.orgRef`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClavexRole is the Schema for the clavexroles API
type ClavexRole struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClavexRole
	// +required
	Spec ClavexRoleSpec `json:"spec"`

	// status defines the observed state of ClavexRole
	// +optional
	Status ClavexRoleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClavexRoleList contains a list of ClavexRole
type ClavexRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClavexRole `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClavexRole{}, &ClavexRoleList{})
		return nil
	})
}
