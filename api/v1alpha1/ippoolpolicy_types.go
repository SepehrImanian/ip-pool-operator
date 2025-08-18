// SPDX-License-Identifier: Apache-2.0
// +kubebuilder:object:generate=true
// +groupName=net.techonfire.ir

package v1alpha1

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SourceSpec describes where to fetch IP/CIDR entries from.
// Each URL should serve plain text with one entry per line. Lines may contain comments using '#'.
// Entries may be single IPs (IPv4/IPv6) or CIDRs; single IPs will be normalized to /32 or /128.
//
// Example line formats:
//
//	1.2.3.4
//	1.2.3.0/24
//	2001:db8::/32
//	2001:db8::1
//	# comment
//	10.0.0.1   # inline comment ok
//
// If both URLs and InlineCIDRs are provided, the union will be used.
// Duplicates are removed automatically.
//
// // Removed problematic XValidation rules for compatibility with CRD installation
// NOTE: you can reintroduce CEL validations later once cluster supports them
type SourceSpec struct {

	// URLs with one IP/CIDR per line (comments with '#').
	// +kubebuilder:validation:Optional
	URLs []string `json:"urls,omitempty"`

	// InlineCIDRs provides additional IPs/CIDRs.
	// +kubebuilder:validation:Optional
	InlineCIDRs []string `json:"inlineCIDRs,omitempty"`

	// Optional HTTP headers to include when fetching URLs (e.g., Authorization).
	// +kubebuilder:validation:Optional
	HTTPHeaders map[string]string `json:"httpHeaders,omitempty"`
}

// TargetSpec selects where the NetworkPolicies are applied.
// The operator creates one NetworkPolicy per matched namespace.
// PodSelector defaults to all pods in the namespace if omitted.
type TargetSpec struct {
	// NamespaceSelector chooses namespaces (required for Cluster-scoped CRD).
	// If omitted or empty, no namespaces will match (be explicit!).
	// +kubebuilder:validation:Optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// PodSelector chooses pods inside each matched namespace.
	// Empty selector means all pods.
	// +kubebuilder:validation:Optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
}

// PolicySpec controls the resulting NetworkPolicy spec.
type PolicySpec struct {
	// Types must include either Egress, Ingress, or both.
	// +kubebuilder:validation:MinItems=1
	Types []networkingv1.PolicyType `json:"types"`

	// Ports (applied to both ingress/egress rules if present).
	// +kubebuilder:validation:Optional
	Ports []networkingv1.NetworkPolicyPort `json:"ports,omitempty"`

	// Optional prefix for created NetworkPolicy names. Default: "ippool-".
	// +kubebuilder:validation:Optional
	NamePrefix string `json:"namePrefix,omitempty"`
}

// IPPoolPolicySpec defines the desired state of IPPoolPolicy.
type IPPoolPolicySpec struct {
	// Where to fetch/define the IPs
	Source SourceSpec `json:"source"`

	// Where to apply the policy
	Target TargetSpec `json:"target"`

	// How the policy should look
	Policy PolicySpec `json:"policy"`

	// How often to re-fetch & reconcile, e.g., "30m", "1h". Default 1h.
	// +kubebuilder:default="1h"
	RefreshInterval metav1.Duration `json:"refreshInterval,omitempty"`
}

// IPPoolPolicyStatus captures the operator's last observed state.
type IPPoolPolicyStatus struct {
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	LastSyncTime       *metav1.Time `json:"lastSyncTime,omitempty"`
	EffectiveCIDRs     []string     `json:"effectiveCIDRs,omitempty"`
	Namespaces         []string     `json:"namespaces,omitempty"`
	Policies           int32        `json:"policies,omitempty"`
	Hash               string       `json:"hash,omitempty"`

	// --- Pretty strings for kubectl columns ---
	PortList          string `json:"portList,omitempty"`          // e.g. "80,443"
	PodSelectorString string `json:"podSelectorString,omitempty"` // e.g. "app:web,tier:frontend"
	NSSelectorString  string `json:"nsSelectorString,omitempty"`  // e.g. "enforce-ip-allowlist:true"
	DirString         string `json:"dirString,omitempty"`         // e.g. "Egress" or "Ingress,Egress"
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ippp

// Pretty columns that read from .status (strings you set in Reconcile):
// NOTE: keep the quotes as plain ASCII " characters.
// +kubebuilder:printcolumn:name="DIR",type=string,JSONPath=".status.dirString"
// +kubebuilder:printcolumn:name="PORTS",type=string,JSONPath=".status.portList"
// +kubebuilder:printcolumn:name="POD-SELECTOR",type=string,JSONPath=".status.podSelectorString"
// +kubebuilder:printcolumn:name="NS-SELECTOR",type=string,JSONPath=".status.nsSelectorString"
type IPPoolPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IPPoolPolicySpec   `json:"spec,omitempty"`
	Status            IPPoolPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type IPPoolPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPPoolPolicy `json:"items"`
}

// Register the types with the scheme so v1alpha1.AddToScheme adds them.
func init() {
	SchemeBuilder.Register(&IPPoolPolicy{}, &IPPoolPolicyList{})
}
