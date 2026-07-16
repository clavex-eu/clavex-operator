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

// ClavexIdentityProviderSpec mirrors cmd/clavexctl/iac.go: IDPSpec, with the
// provider-specific OIDC/SAML fields (authorizationUrl, idpMetadataUrl,
// certificates, …) folded into the free-form `config` map, matching the
// generic shape of sdk/go's CreateIDPParams.Config — the Admin API itself
// makes no structural distinction between OIDC and SAML config beyond the
// providerType discriminator and a handful of well-known keys.
//
// Unlike ClavexClient, there is no client-supplied stable ID for an IDP: the
// Admin API assigns an opaque UUID on creation. The reconciler therefore
// uses `name` as the reconciliation key (mirrors iac.go's `indexBy(...,
// Name)`), listing existing providers and matching by name.
type ClavexIdentityProviderSpec struct {
	// orgRef identifies the Clavex organisation that owns this identity
	// provider, for display/human reference only — see the equivalent
	// field on ClavexClientSpec for why it cannot be used to resolve the
	// org for API calls.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	OrgRef string `json:"orgRef"`

	// authSecretRef points to a Secret containing the org-scoped Admin API
	// credentials used to reconcile this identity provider. See
	// ClavexClientSpec.AuthSecretRef for the required Secret shape
	// (apiKey + orgId keys).
	//
	// +required
	AuthSecretRef SecretRef `json:"authSecretRef"`

	// name is the stable reconciliation key: since the Admin API assigns
	// its own opaque ID on creation, the controller looks up existing
	// identity providers by this name to decide between create and
	// update. Renaming this field therefore creates a new IdP rather than
	// renaming the existing one — mirrors the same limitation in
	// `clavexctl org apply`.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// providerType selects the protocol/provider (oidc, saml, google,
	// github, …), matching the Admin API's `type` field one-to-one.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	ProviderType string `json:"providerType"`

	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// clientId is used by OIDC/OAuth2-style providers (oidc, google,
	// github, …); left empty for providers that don't use it (e.g. pure
	// SAML, which instead relies on config["idp_metadata_url"] or
	// config["idp_cert"]).
	//
	// +optional
	ClientID string `json:"clientId,omitempty"`

	// clientSecretRef optionally points to a Secret holding the
	// provider's confidential client secret (or SAML signing
	// key/passphrase, depending on providerType). Never inlined in the
	// spec — mirrors iac.go's IDPSpec, which intentionally omits
	// ClientSecret from export. The reconciler reads this value and
	// merges it into config["client_secret"] before calling the Admin
	// API; it is never written back (unlike ClavexClient's
	// ClientSecretRef, which the controller writes to).
	//
	// +optional
	ClientSecretRef *SecretRef `json:"clientSecretRef,omitempty"`

	// config carries the remaining provider-specific settings verbatim
	// (e.g. authorizationUrl/tokenUrl/userinfoUrl/scopes for OIDC,
	// idpMetadataUrl/idpCert/spEntityId for SAML), matching
	// sdk/go's CreateIDPParams.Config shape. Values that are themselves
	// secrets (other than the client secret, which has its own
	// clientSecretRef) should not be placed here — this map is stored
	// verbatim in the CR.
	//
	// +optional
	Config map[string]string `json:"config,omitempty"`

	// +optional
	AllowJIT bool `json:"allowJit,omitempty"`

	// +optional
	RolesClaim string `json:"rolesClaim,omitempty"`

	// +optional
	RoleClaimMappings map[string]string `json:"roleClaimMappings,omitempty"`
}

// ClavexIdentityProviderStatus defines the observed state of ClavexIdentityProvider.
type ClavexIdentityProviderStatus struct {
	// conditions represent the current state of the
	// ClavexIdentityProvider resource. Expected types: "Ready", "Synced".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clavexIdpId is the Admin API's opaque UUID for this identity
	// provider, once synced — needed since, unlike ClavexClient, the CR
	// doesn't carry a client-supplied ID of its own.
	// +optional
	ClavexIDPID string `json:"clavexIdpId,omitempty"`

	// lastSyncedAt is the RFC3339 timestamp of the last successful
	// reconciliation against the Admin API.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvxidp,categories=clavex
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.orgRef`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.providerType`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClavexIdentityProvider is the Schema for the clavexidentityproviders API
type ClavexIdentityProvider struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClavexIdentityProvider
	// +required
	Spec ClavexIdentityProviderSpec `json:"spec"`

	// status defines the observed state of ClavexIdentityProvider
	// +optional
	Status ClavexIdentityProviderStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClavexIdentityProviderList contains a list of ClavexIdentityProvider
type ClavexIdentityProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClavexIdentityProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClavexIdentityProvider{}, &ClavexIdentityProviderList{})
		return nil
	})
}
