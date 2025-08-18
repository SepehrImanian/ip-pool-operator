// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/SepehrImanian/ip-pool-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	finalizerName = "net.techonfire.ir/finalizer"
	annoHash      = "net.techonfire.ir/ippool-hash"
	labelOwner    = "net.techonfire.ir/owner"
)

// IPPoolPolicyReconciler reconciles a IPPoolPolicy object
type IPPoolPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	HTTP   *http.Client
}

// +kubebuilder:rbac:groups=net.techonfire.ir,resources=ippoolpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=net.techonfire.ir,resources=ippoolpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *IPPoolPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ippp v1alpha1.IPPoolPolicy
	if err := r.Get(ctx, req.NamespacedName, &ippp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// ensure finalizer
	if ippp.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&ippp, finalizerName) {
			controllerutil.AddFinalizer(&ippp, finalizerName)
			if err := r.Update(ctx, &ippp); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// handle delete
		if controllerutil.ContainsFinalizer(&ippp, finalizerName) {
			if err := r.cleanupAllNamespaces(ctx, &ippp); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&ippp, finalizerName)
			if err := r.Update(ctx, &ippp); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 1) Build the effective CIDR set
	cidrs, err := r.resolveCIDRs(ctx, &ippp)
	if err != nil {
		logger.Error(err, "resolveCIDRs failed")
		return r.requeueAfter(ippp.Spec.RefreshInterval.Duration), err
	}

	// 2) Determine target namespaces
	nsList := &corev1.NamespaceList{}
	selector := labels.Everything()
	if ippp.Spec.Target.NamespaceSelector != nil {
		if s, err := metav1.LabelSelectorAsSelector(ippp.Spec.Target.NamespaceSelector); err == nil {
			selector = s
		} else {
			return r.requeueAfter(ippp.Spec.RefreshInterval.Duration), err
		}
	}
	if err := r.List(ctx, nsList, &client.ListOptions{LabelSelector: selector}); err != nil {
		return r.requeueAfter(ippp.Spec.RefreshInterval.Duration), err
	}

	names := make([]string, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		names = append(names, ns.Name)
	}
	sort.Strings(names)

	// 3) Desired hash for idempotence
	h := sha256.New()
	for _, c := range cidrs {
		_, _ = h.Write([]byte(c))
		_, _ = h.Write([]byte{'\n'})
	}
	for _, p := range ippp.Spec.Policy.Ports {
		// a simple but stable port hash; more fields can be added if needed
		if p.Port != nil {
			_, _ = h.Write([]byte(p.Port.String()))
		}
		if p.Protocol != nil {
			_, _ = h.Write([]byte(*p.Protocol))
		}
	}
	for _, t := range ippp.Spec.Policy.Types {
		_, _ = h.Write([]byte(t))
	}
	nsKey := strings.Join(names, ",")
	_, _ = h.Write([]byte(nsKey))
	hash := hex.EncodeToString(h.Sum(nil))[:16]

	// 4) Reconcile one NetworkPolicy per namespace
	createdOrUpdated := int32(0)
	for _, ns := range names {
		ok, err := r.reconcileNamespace(ctx, &ippp, ns, cidrs, hash)
		if err != nil {
			return r.requeueAfter(ippp.Spec.RefreshInterval.Duration), err
		}
		if ok {
			createdOrUpdated++
		}
	}

	// 5) Update status
	now := metav1.NewTime(time.Now())
	ippp.Status.ObservedGeneration = ippp.GetGeneration()
	ippp.Status.LastSyncTime = &now
	ippp.Status.EffectiveCIDRs = cidrs
	ippp.Status.Namespaces = names
	ippp.Status.Policies = int32(len(names))
	ippp.Status.Hash = hash

	// --- NEW: pretty strings for kubectl columns ---
	ippp.Status.DirString = joinPolicyTypes(ippp.Spec.Policy.Types)
	ippp.Status.PortList = summarizePorts(ippp.Spec.Policy.Ports)
	ippp.Status.PodSelectorString = selectorToString(ippp.Spec.Target.PodSelector)
	ippp.Status.NSSelectorString = selectorToString(ippp.Spec.Target.NamespaceSelector)

	if err := r.Status().Update(ctx, &ippp); err != nil {
		logger.Error(err, "status update failed")
	}

	logger.Info("reconciled", "policies", len(names), "changed", createdOrUpdated, "hash", hash)
	return r.requeueAfter(ippp.Spec.RefreshInterval.Duration), nil
}

func (r *IPPoolPolicyReconciler) requeueAfter(d time.Duration) ctrl.Result {
	if d <= 0 {
		d = time.Hour
	}
	return ctrl.Result{RequeueAfter: d}
}

func (r *IPPoolPolicyReconciler) reconcileNamespace(ctx context.Context, ippp *v1alpha1.IPPoolPolicy, ns string, cidrs []string, hash string) (bool, error) {
	namePrefix := ippp.Spec.Policy.NamePrefix
	if namePrefix == "" {
		namePrefix = "ippool-"
	}
	npName := fmt.Sprintf("%s%s", namePrefix, ippp.Name)

	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      npName,
			Namespace: ns,
			Labels: map[string]string{
				labelOwner: ippp.Name,
			},
			Annotations: map[string]string{
				annoHash: hash,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: emptyOr(ippp.Spec.Target.PodSelector),
			PolicyTypes: ippp.Spec.Policy.Types,
		},
	}

	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(cidrs))
	for _, c := range cidrs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
	}

	if containsType(ippp.Spec.Policy.Types, networkingv1.PolicyTypeIngress) {
		desired.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From:  peers,
			Ports: ippp.Spec.Policy.Ports,
		}}
	}
	if containsType(ippp.Spec.Policy.Types, networkingv1.PolicyTypeEgress) {
		desired.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
			To:    peers,
			Ports: ippp.Spec.Policy.Ports,
		}}
	}

	var existing networkingv1.NetworkPolicy
	key := types.NamespacedName{Namespace: ns, Name: npName}
	if err := r.Get(ctx, key, &existing); client.IgnoreNotFound(err) != nil {
		return false, err
	}

	if existing.Name == "" {
		// create
		if err := r.Create(ctx, desired); err != nil {
			return false, err
		}
		return true, nil
	}

	// update if hash changed or spec differs
	if existing.Annotations == nil || existing.Annotations[annoHash] != hash || !npSpecEqual(existing.Spec, desired.Spec) {
		existing.Labels = desired.Labels
		existing.Annotations = desired.Annotations
		existing.Spec = desired.Spec
		if err := r.Update(ctx, &existing); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (r *IPPoolPolicyReconciler) cleanupAllNamespaces(ctx context.Context, ippp *v1alpha1.IPPoolPolicy) error {
	var nps networkingv1.NetworkPolicyList
	if err := r.List(ctx, &nps, &client.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{labelOwner: ippp.Name})}); err != nil {
		return err
	}
	for _, np := range nps.Items {
		_ = r.Delete(ctx, &np)
	}
	return nil
}

func containsType(types []networkingv1.PolicyType, t networkingv1.PolicyType) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

func npSpecEqual(a, b networkingv1.NetworkPolicySpec) bool {
	// shallow compare of key fields; for production consider a deep-equal
	if len(a.PolicyTypes) != len(b.PolicyTypes) {
		return false
	}
	for i := range a.PolicyTypes {
		if a.PolicyTypes[i] != b.PolicyTypes[i] {
			return false
		}
	}
	if len(a.Ingress) != len(b.Ingress) || len(a.Egress) != len(b.Egress) {
		return false
	}
	return true // hash already covers ports & peers; lengths cover rule presence
}

func emptyOr(sel *metav1.LabelSelector) metav1.LabelSelector {
	if sel == nil {
		return metav1.LabelSelector{}
	}
	return *sel
}

// resolveCIDRs fetches and normalizes all CIDRs.
func (r *IPPoolPolicyReconciler) resolveCIDRs(ctx context.Context, ippp *v1alpha1.IPPoolPolicy) ([]string, error) {
	set := map[string]struct{}{}

	// inline first
	for _, raw := range ippp.Spec.Source.InlineCIDRs {
		if cidr, ok := normalizeIPorCIDR(strings.TrimSpace(raw)); ok {
			set[cidr] = struct{}{}
		}
	}

	// then URLs
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	for _, u := range ippp.Spec.Source.URLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range ippp.Spec.Source.HTTPHeaders {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		func() {
			defer resp.Body.Close()
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
			for scanner.Scan() {
				line := scanner.Text()
				// strip comments
				if i := strings.Index(line, "#"); i >= 0 {
					line = line[:i]
				}
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if cidr, ok := normalizeIPorCIDR(line); ok {
					set[cidr] = struct{}{}
				}
			}
		}()
	}

	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeIPorCIDR(s string) (string, bool) {
	if _, ipnet, err := net.ParseCIDR(s); err == nil && ipnet != nil {
		return ipnet.String(), true
	}
	if ip := net.ParseIP(s); ip != nil {
		if ip.To4() != nil {
			return fmt.Sprintf("%s/32", ip.String()), true
		}
		return fmt.Sprintf("%s/128", ip.String()), true
	}
	return "", false
}

func joinPolicyTypes(ts []networkingv1.PolicyType) string {
	if len(ts) == 0 {
		return "-"
	}
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = string(t)
	}
	return strings.Join(parts, ",")
}

func summarizePorts(ports []networkingv1.NetworkPolicyPort) string {
	if len(ports) == 0 {
		return "-"
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		var s strings.Builder
		if p.Protocol != nil {
			s.WriteString(string(*p.Protocol))
		} else {
			s.WriteString("TCP")
		}
		s.WriteString(":")
		if p.Port != nil {
			s.WriteString(p.Port.String())
		} else {
			s.WriteString("*")
		}
		if p.EndPort != nil {
			s.WriteString(fmt.Sprintf("-%d", *p.EndPort))
		}
		out = append(out, s.String())
	}
	return strings.Join(out, ",")
}

func selectorToString(sel *metav1.LabelSelector) string {
	if sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0) {
		return "<all>"
	}
	// compact summary: just matchLabels
	if len(sel.MatchExpressions) == 0 {
		if len(sel.MatchLabels) == 0 {
			return "<all>"
		}
		pairs := make([]string, 0, len(sel.MatchLabels))
		for k, v := range sel.MatchLabels {
			pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(pairs)
		return strings.Join(pairs, ",")
	}
	// fallback to full selector string
	s, _ := metav1.LabelSelectorAsSelector(sel)
	return s.String()
}

func (r *IPPoolPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.IPPoolPolicy{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
