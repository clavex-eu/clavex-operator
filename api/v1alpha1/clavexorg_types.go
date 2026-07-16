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

// ClavexOrgSpec is deliberately scoped to per-org **settings**, not
// organisation lifecycle (create/delete). Creating/deleting an
// Organization itself is a superadmin-only bootstrap operation
// (sdk/go/auth_orgs.go: OrgService.Create/Delete require superadmin
// privileges) that is orthogonal to this operator's per-org auth model
// (Model A: an org-scoped API key via authSecretRef, established in
// Fase 0 — see plan.md). A ClavexOrg CR therefore always targets an
// *existing* org and only manages settings reachable with an org-scoped
// key.
//
// Field names deliberately mirror the SDK's actual wire types
// (sdk/go/types.go: PasswordPolicy, RateLimitConfig) rather than
// cmd/clavexctl/iac.go's PasswordPolicySpec/RateLimitsSpec, whose json
// tags (e.g. require_uppercase, require_digit, require_symbol) do not
// match the real API schema the SDK targets (require_upper,
// require_number, require_special) — a pre-existing mismatch in iac.go
// that would silently zero these fields on apply. This CRD avoids
// inheriting that bug by going through the SDK's PasswordPolicyService/
// RateLimitService directly.
//
// Both sections are optional and independently reconciled: a nil section
// means "don't manage this — leave the live value untouched", which lets
// a ClavexOrg CR manage just one of the two settings groups if desired.
// Scope is currently limited to the two settings with full SDK Get/Put
// support; LockoutPolicy/EmailPolicy/FeatureFlags from iac.go's OrgSpec
// have no SDK service yet (raw-HTTP only in iac.go) and are deferred,
// same as ClavexAuthPolicy.
type ClavexOrgSpec struct {
	// orgRef identifies the Clavex organisation whose settings this CR
	// manages, for display/human reference only — see
	// ClavexClientSpec.OrgRef for why it cannot be used to resolve the
	// org for API calls.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	OrgRef string `json:"orgRef"`

	// authSecretRef points to a Secret containing the org-scoped Admin
	// API credentials used to reconcile these settings. See
	// ClavexClientSpec.AuthSecretRef for the required Secret shape
	// (apiKey + orgId keys).
	//
	// +required
	AuthSecretRef SecretRef `json:"authSecretRef"`

	// passwordPolicy, when set, is kept in sync with the org's password
	// complexity rules via PasswordPolicyService.Put. Omit to leave the
	// live password policy unmanaged.
	//
	// +optional
	PasswordPolicy *PasswordPolicySpec `json:"passwordPolicy,omitempty"`

	// rateLimits, when set, is kept in sync with the org's login
	// rate-limit / lockout thresholds via RateLimitService.Update. Omit
	// to leave the live rate limits unmanaged.
	//
	// +optional
	RateLimits *RateLimitsSpec `json:"rateLimits,omitempty"`
}

// PasswordPolicySpec mirrors sdk/go/types.go: PasswordPolicy (minus the
// server-assigned OrgID field).
type PasswordPolicySpec struct {
	// +kubebuilder:validation:Minimum=1
	MinLength int `json:"minLength"`
	// +optional
	RequireUpper bool `json:"requireUpper"`
	// +optional
	RequireLower bool `json:"requireLower"`
	// +optional
	RequireNumber bool `json:"requireNumber"`
	// +optional
	RequireSpecial bool `json:"requireSpecial"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxAgeDays int `json:"maxAgeDays,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	HistoryCount int `json:"historyCount,omitempty"`
}

// RateLimitsSpec mirrors sdk/go/types.go: RateLimitConfig (minus the
// server-assigned OrgID field).
type RateLimitsSpec struct {
	// +kubebuilder:validation:Minimum=1
	MaxAttemptsPerMinute int `json:"maxAttemptsPerMinute"`
	// +kubebuilder:validation:Minimum=0
	LockoutDurationSeconds int `json:"lockoutDurationSeconds"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	IPMaxAttemptsPerMinute int `json:"ipMaxAttemptsPerMinute,omitempty"`
}

// ClavexOrgStatus defines the observed state of ClavexOrg.
type ClavexOrgStatus struct {
	// conditions represent the current state of the ClavexOrg
	// resource. Expected types: "Ready", "Synced".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastSyncedAt is the RFC3339 timestamp of the last successful
	// reconciliation against the Admin API.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvxorg,categories=clavex
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.orgRef`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClavexOrg is the Schema for the clavexorgs API
type ClavexOrg struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClavexOrg
	// +required
	Spec ClavexOrgSpec `json:"spec"`

	// status defines the observed state of ClavexOrg
	// +optional
	Status ClavexOrgStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClavexOrgList contains a list of ClavexOrg
type ClavexOrgList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClavexOrg `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClavexOrg{}, &ClavexOrgList{})
		return nil
	})
}
