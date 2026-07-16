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

// ClavexClientSpec mirrors cmd/clavexctl/iac.go: ClientSpec 1:1 so that the
// same org YAML fields translate directly to a CR. A shared internal/iacspec
// package (tracked follow-up) is intended to eventually be the single source
// of truth for both clavexctl and this operator.
//
// +kubebuilder:validation:XValidation:rule="self.isPublic == false || !has(self.tokenEndpointAuthMethod) || self.tokenEndpointAuthMethod == 'none'",message="public clients must use tokenEndpointAuthMethod: none"
type ClavexClientSpec struct {
	// orgRef identifies the Clavex organisation that owns this client, for
	// display/human reference (e.g. "kubectl get" output, logs). It matches
	// org.slug in the Admin API. It is NOT used to resolve the org for API
	// calls: the org-scoped API key in authSecretRef can only call
	// /api/v1/organizations/{org_id}/... for its own org, and it has no
	// permission to list/resolve orgs by slug (that endpoint is
	// superadmin-only). The actual org UUID used on the wire comes from the
	// "orgId" key in the same Secret — see authSecretRef.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	OrgRef string `json:"orgRef"`

	// authSecretRef points to a Secret containing the credentials used to
	// reconcile this client against the Admin API. The Secret MUST contain
	// two data keys:
	//   - apiKey (or the name set in Key): an org-scoped Clavex API key
	//     (org_id scoping added in migration 000177_org_scoped_admin_api_keys)
	//   - orgId: the UUID of the organisation that key is scoped to — used
	//     verbatim as the {org_id} path segment. Whoever mints the API key
	//     already knows this UUID (it's returned by org creation / the
	//     superadmin API-key-creation call), so the controller never needs
	//     superadmin privileges to resolve orgRef (slug) to an ID itself.
	//
	// If namespace is omitted, it defaults to the CR's own namespace to
	// avoid accidental cross-namespace secret reads.
	//
	// +required
	AuthSecretRef SecretRef `json:"authSecretRef"`

	// clientId is the stable reconciliation key. If a client with this ID
	// already exists in orgRef it is updated (PATCH); otherwise created.
	// Immutable after creation (changing it would orphan the old client).
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clientId is immutable"
	ClientID string `json:"clientId"`

	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +required
	// +kubebuilder:validation:MinItems=1
	RedirectURIs []string `json:"redirectUris"`

	// +optional
	PostLogoutRedirectURIs []string `json:"postLogoutRedirectUris,omitempty"`

	// +required
	// +kubebuilder:validation:MinItems=1
	GrantTypes []string `json:"grantTypes"`

	// +optional
	ResponseTypes []string `json:"responseTypes,omitempty"`

	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// +optional
	// +kubebuilder:default=false
	IsPublic bool `json:"isPublic"`

	// +optional
	// +kubebuilder:default=true
	IsActive bool `json:"isActive"`

	// +optional
	// +kubebuilder:validation:Enum=none;client_secret_basic;client_secret_post;private_key_jwt
	TokenEndpointAuthMethod string `json:"tokenEndpointAuthMethod,omitempty"`

	// +optional
	// +kubebuilder:validation:Enum=RS256;RS384;RS512;ES256;ES384;ES512;PS256;PS384;PS512
	IDTokenSignedResponseAlg string `json:"idTokenSignedResponseAlg,omitempty"`

	// clientSecretRef optionally points to a Secret from which the
	// controller reads/writes the confidential client secret. Never
	// inlined in the spec (mirrors IDPSpec's exclusion of ClientSecret
	// from export in iac.go). Left nil for public clients.
	//
	// +optional
	ClientSecretRef *SecretRef `json:"clientSecretRef,omitempty"`
}

// SecretRef is a reference to a Kubernetes Secret, optionally in another
// namespace. Used both for AuthSecretRef (operator credentials) and
// ClientSecretRef (managed resource secrets).
type SecretRef struct {
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +optional
	Namespace string `json:"namespace,omitempty"`

	// key is the Secret data key holding the value. Defaults to "apiKey"
	// for authSecretRef and "clientSecret" for clientSecretRef (defaulting
	// is enforced by the reconciler, not the CRD schema, since the default
	// differs by field).
	//
	// +optional
	Key string `json:"key,omitempty"`
}

// ClavexClientStatus defines the observed state of ClavexClient.
type ClavexClientStatus struct {
	// conditions represent the current state of the ClavexClient resource.
	// Expected types: "Ready", "Synced".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clavexClientId is the immutable client_id echoed back for
	// operator/debugging convenience (equal to spec.clientId once synced).
	// +optional
	ClavexClientID string `json:"clavexClientId,omitempty"`

	// lastSyncedAt is the RFC3339 timestamp of the last successful
	// reconciliation against the Admin API.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvxclient,categories=clavex
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.orgRef`
// +kubebuilder:printcolumn:name="ClientID",type=string,JSONPath=`.spec.clientId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClavexClient is the Schema for the clavexclients API
type ClavexClient struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClavexClient
	// +required
	Spec ClavexClientSpec `json:"spec"`

	// status defines the observed state of ClavexClient
	// +optional
	Status ClavexClientStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClavexClientList contains a list of ClavexClient
type ClavexClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClavexClient `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClavexClient{}, &ClavexClientList{})
		return nil
	})
}
