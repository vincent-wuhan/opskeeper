// objects.go navigates untyped Kubernetes documents.
//
// The adapter decodes into map[string]any rather than into generated typed
// structs, for the reason given in client.go: the API returns one document
// shape that has been stable for a decade, and the typed clients exist to
// track the fields *inside* it. Reading a field costs a lookup; the lookup
// functions below are that lookup, written once, tolerant of the field being
// absent (an older API server, a resource that has not been reconciled yet)
// and never panicking on a type that changed.
package k8s

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// object is one Kubernetes resource, decoded generically.
type object = map[string]any

func nested(o object, path ...string) object {
	cur := o
	for _, key := range path {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

func str(o object, path ...string) string {
	if len(path) == 0 || o == nil {
		return ""
	}
	cur := o
	for _, key := range path[:len(path)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	s, _ := cur[path[len(path)-1]].(string)
	return s
}

func num(o object, path ...string) float64 {
	if len(path) == 0 || o == nil {
		return 0
	}
	cur := o
	for _, key := range path[:len(path)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return 0
		}
		cur = next
	}
	switch v := cur[path[len(path)-1]].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func items(o object) []object {
	raw, ok := o["items"].([]any)
	if !ok {
		return nil
	}
	out := make([]object, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func metaName(o object) string      { return str(o, "metadata", "name") }
func metaNamespace(o object) string { return str(o, "metadata", "namespace") }

// conditionStatus returns the status of one entry of status.conditions.
//
// The conditions array is the API's universal health vocabulary — node
// Ready, pod Ready, deployment Available — and each entry is identified by a
// `type` whose position in the array is not stable. Indexing it is the
// classic bug that reports PodScheduled's status as Ready.
func conditionStatus(o object, condType string) string {
	conds, ok := nested(o, "status")["conditions"].([]any)
	if !ok {
		return ""
	}
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := c["type"].(string); s == condType {
			st, _ := c["status"].(string)
			return st
		}
	}
	return ""
}

// readyCondition is the condition every workload's readiness is expressed
// through, spelled once.
const readyCondition = "Ready"

// isReady reports whether an object's Ready condition is True.
func isReady(o object) bool { return conditionStatus(o, readyCondition) == "True" }

// listPath namespaces a collection path.
//
// An empty namespace means "every namespace", which is the API's own
// convention: `/api/v1/pods` is cluster-wide and
// `/api/v1/namespaces/prod/pods` is not.
func listPath(prefix, namespace, collection string) string {
	if namespace == "" {
		return prefix + "/" + collection
	}
	return fmt.Sprintf("%s/namespaces/%s/%s", prefix, namespace, collection)
}

// objectPath namespaces a single-object path.
func objectPath(prefix, namespace, collection, name string) string {
	return fmt.Sprintf("%s/namespaces/%s/%s/%s", prefix, namespace, collection, name)
}

// clusterObjectPath addresses a cluster-scoped object.
//
// Nodes are the ones this adapter touches. Their path has no namespace
// segment at all, and building it with the namespaced helper produces
// "/api/v1/namespaces//nodes/n1" — a 404 that reads like a missing node
// rather than a malformed path.
func clusterObjectPath(prefix, collection, name string) string {
	return fmt.Sprintf("%s/%s/%s", prefix, collection, name)
}

// apiPrefix is the two API roots this adapter speaks: the core group, which
// is unversioned in its path shape, and apps/v1, which is where every
// workload lives.
const (
	corePrefix = "/api/v1"
	appsPrefix = "/apis/apps/v1"
)

// collections, by kind, in the terms the API paths use.
const (
	colPods        = "pods"
	colNodes       = "nodes"
	colEvents      = "events"
	colDeployments = "deployments"
	colReplicaSets = "replicasets"
	colServices    = "services"
	colPVCs        = "persistentvolumeclaims"
)

// kindPrefix is the API root a collection belongs to.
func kindPrefix(collection string) string {
	switch collection {
	case colDeployments, colReplicaSets:
		return appsPrefix
	default:
		return corePrefix
	}
}

// resolveNamespace finds the unique namespace of a named object.
//
// This is the answer to "the operator named a pod but not a namespace".
// kubectl resolves that with a namespace from the kubeconfig, which is a
// default the *user* configured; an adapter has no such configuration, and
// inventing "default" would silently act on a different cluster object than
// the one the alert named. Searching all namespaces and requiring exactly
// one match is the alternative: it either finds the object the caller meant
// or it refuses and lists what it found. Ambiguity is reported, never
// resolved by picking one.
func (c *kubeClient) resolveNamespace(ctx context.Context, collection, objName, namespace string) (string, error) {
	if namespace != "" {
		return namespace, nil
	}
	var list object
	if err := c.get(ctx, listPath(kindPrefix(collection), "", collection), &list); err != nil {
		return "", err
	}
	var matches []string
	for _, item := range items(list) {
		if metaName(item) == objName {
			matches = append(matches, metaNamespace(item))
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("k8s: no %s named %q exists in any namespace", strings.TrimSuffix(collection, "s"), objName)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		// Two matches cannot be resolved by preference. The API server
		// allows the same name in two namespaces on purpose, and a
		// remediation that picks one is a remediation that may act on
		// staging when the alert was about production.
		return "", fmt.Errorf("k8s: %s %q exists in %d namespaces (%s); name the namespace",
			strings.TrimSuffix(collection, "s"), objName, len(matches), strings.Join(matches, ", "))
	}
}

// getObject fetches one object, resolving its namespace when it was omitted.
func (c *kubeClient) getObject(ctx context.Context, collection, objName, namespace string) (object, string, error) {
	ns, err := c.resolveNamespace(ctx, collection, objName, namespace)
	if err != nil {
		return nil, "", err
	}
	var obj object
	if err := c.get(ctx, objectPath(kindPrefix(collection), ns, collection, objName), &obj); err != nil {
		return nil, ns, err
	}
	return obj, ns, nil
}

// urlValues builds a query string, skipping empty values.
func urlValues(pairs map[string]string) string {
	q := url.Values{}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := pairs[k]; v != "" {
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}
