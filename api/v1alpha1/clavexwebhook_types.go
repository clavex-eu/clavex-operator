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

// ClavexWebhookSpec mirrors cmd/clavexctl/iac.go: WebhookSpec, with one
// deliberate deviation: the Admin API's webhooks table has no `name`
// column (see internal/models/models.go: Webhook and
// internal/repository/webhooks.go) — WebhookSpec.Name in iac.go is a
// client-side-only YAML label that is never persisted or returned by the
// API, which means clavexctl's own by-name reconciliation for webhooks
// (indexBy(..., Name) in iac.go's diff logic) silently keys every live
// webhook under an empty string on export. This CRD avoids inheriting that
// bug by reconciling on **url** instead — the one field the API does
// treat as effectively-unique per webhook in practice. Two webhooks with
// genuinely the same URL but different Events are a known, accepted
// limitation (mirrors the ambiguity already present in the platform).
type ClavexWebhookSpec struct {
	// orgRef identifies the Clavex organisation that owns this webhook,
	// for display/human reference only — see ClavexClientSpec.OrgRef for
	// why it cannot be used to resolve the org for API calls.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	OrgRef string `json:"orgRef"`

	// authSecretRef points to a Secret containing the org-scoped Admin API
	// credentials used to reconcile this webhook. See
	// ClavexClientSpec.AuthSecretRef for the required Secret shape
	// (apiKey + orgId keys).
	//
	// +required
	AuthSecretRef SecretRef `json:"authSecretRef"`

	// url is the stable reconciliation key (see the type doc above for
	// why `name`, used by clavexctl's iac.go, is not viable here).
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// events lists the event types (e.g. "user.login",
	// "user.password.changed") this webhook subscribes to.
	//
	// +required
	// +kubebuilder:validation:MinItems=1
	Events []string `json:"events"`

	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// signingKeyRef optionally points to a Secret holding the webhook's
	// HMAC signing secret. Never inlined in the spec. The reconciler
	// reads this value and sends it as CreateWebhookParams.Secret on
	// every create/update call; it is never written back — the Admin API
	// never echoes the signing secret back in Get/List responses
	// (internal/models.Webhook.Secret is tagged json:"-"), mirroring
	// ClavexIdentityProvider's clientSecretRef asymmetry.
	//
	// +optional
	SigningKeyRef *SecretRef `json:"signingKeyRef,omitempty"`
}

// ClavexWebhookStatus defines the observed state of ClavexWebhook.
type ClavexWebhookStatus struct {
	// conditions represent the current state of the ClavexWebhook
	// resource. Expected types: "Ready", "Synced".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clavexWebhookId is the Admin API's opaque UUID for this webhook,
	// once synced.
	// +optional
	ClavexWebhookID string `json:"clavexWebhookId,omitempty"`

	// lastSyncedAt is the RFC3339 timestamp of the last successful
	// reconciliation against the Admin API.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvxwebhook,categories=clavex
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.orgRef`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClavexWebhook is the Schema for the clavexwebhooks API
type ClavexWebhook struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClavexWebhook
	// +required
	Spec ClavexWebhookSpec `json:"spec"`

	// status defines the observed state of ClavexWebhook
	// +optional
	Status ClavexWebhookStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClavexWebhookList contains a list of ClavexWebhook
type ClavexWebhookList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClavexWebhook `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClavexWebhook{}, &ClavexWebhookList{})
		return nil
	})
}
