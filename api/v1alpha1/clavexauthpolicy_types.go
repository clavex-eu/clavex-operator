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

// ClavexAuthPolicySpec mirrors cmd/clavexctl/iac.go: AuthPolicySpec and the
// SDK's PolicyRule/PolicyConditions (sdk/go/policies.go,
// sdk/go/types.go). Unlike ClavexRole, the Admin API's PolicyService
// exposes a full Update, so an existing rule is kept fully in sync with
// spec, not just created-once. Rules are looked up and reconciled by
// spec.name — same stable key convention already documented in iac.go's
// AuthPolicySpec ("Name is used as a stable key for reconciliation").
type ClavexAuthPolicySpec struct {
	// orgRef identifies the Clavex organisation that owns this policy
	// rule, for display/human reference only — see
	// ClavexClientSpec.OrgRef for why it cannot be used to resolve the
	// org for API calls.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	OrgRef string `json:"orgRef"`

	// authSecretRef points to a Secret containing the org-scoped Admin
	// API credentials used to reconcile this rule. See
	// ClavexClientSpec.AuthSecretRef for the required Secret shape
	// (apiKey + orgId keys).
	//
	// +required
	AuthSecretRef SecretRef `json:"authSecretRef"`

	// name is the stable reconciliation key (see the type doc above).
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// priority controls evaluation order — lower numbers are checked
	// first. Rules with equal priority are evaluated in declaration
	// order server-side.
	//
	// +optional
	// +kubebuilder:default=100
	Priority int `json:"priority,omitempty"`

	// action is the outcome applied when all conditions match.
	//
	// +required
	// +kubebuilder:validation:Enum=allow;deny;require_mfa;step_up
	Action string `json:"action"`

	// enabled allows the rule to be disabled without deleting it.
	//
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// conditions holds the optional match criteria for this rule. A nil
	// field within Conditions means "no constraint on that signal".
	//
	// +optional
	Conditions AuthPolicyConditions `json:"conditions,omitempty"`
}

// AuthPolicyConditions mirrors sdk/go/types.go: PolicyConditions.
type AuthPolicyConditions struct {
	// ipCidrs is an IP CIDR allowlist — matches if the request IP is in
	// any of these ranges.
	//
	// +optional
	IPCIDRs []string `json:"ipCidrs,omitempty"`

	// countries is an ISO 3166-1 alpha-2 allowlist.
	//
	// +optional
	Countries []string `json:"countries,omitempty"`

	// notCountries is an ISO 3166-1 alpha-2 denylist.
	//
	// +optional
	NotCountries []string `json:"notCountries,omitempty"`

	// clientIds restricts the rule to specific OIDC client_id values.
	//
	// +optional
	ClientIDs []string `json:"clientIds,omitempty"`

	// mfaEnrolled matches users who have (true) or have not (false)
	// enrolled in MFA. Omit for no constraint.
	//
	// +optional
	MFAEnrolled *bool `json:"mfaEnrolled,omitempty"`

	// newCountry matches logins from a country not seen in the user's
	// 90-day baseline. Omit for no constraint.
	//
	// +optional
	NewCountry *bool `json:"newCountry,omitempty"`

	// daysOfWeek restricts the rule to specific UTC days
	// ("Mon","Tue",...).
	//
	// +optional
	DaysOfWeek []string `json:"daysOfWeek,omitempty"`

	// hourRange restricts the rule to a UTC hour window (0-23).
	//
	// +optional
	HourRange *AuthPolicyHourRange `json:"hourRange,omitempty"`

	// lastLoginBefore matches users whose last login is older than this
	// Go duration string (e.g. "720h"). Users who never logged in always
	// match.
	//
	// +optional
	LastLoginBefore string `json:"lastLoginBefore,omitempty"`
}

// AuthPolicyHourRange mirrors sdk/go/types.go: HourRange.
type AuthPolicyHourRange struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=23
	From int `json:"from"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=23
	To int `json:"to"`
}

// ClavexAuthPolicyStatus defines the observed state of ClavexAuthPolicy.
type ClavexAuthPolicyStatus struct {
	// conditions represent the current state of the ClavexAuthPolicy
	// resource. Expected types: "Ready", "Synced".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clavexAuthPolicyId is the Admin API's opaque UUID for this rule,
	// once synced.
	// +optional
	ClavexAuthPolicyID string `json:"clavexAuthPolicyId,omitempty"`

	// lastSyncedAt is the RFC3339 timestamp of the last successful
	// reconciliation against the Admin API.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvxauthpolicy,categories=clavex
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.orgRef`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClavexAuthPolicy is the Schema for the clavexauthpolicies API
type ClavexAuthPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClavexAuthPolicy
	// +required
	Spec ClavexAuthPolicySpec `json:"spec"`

	// status defines the observed state of ClavexAuthPolicy
	// +optional
	Status ClavexAuthPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClavexAuthPolicyList contains a list of ClavexAuthPolicy
type ClavexAuthPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClavexAuthPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClavexAuthPolicy{}, &ClavexAuthPolicyList{})
		return nil
	})
}
