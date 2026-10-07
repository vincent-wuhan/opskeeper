package loop

// The subject: the concrete entity an alert fired about.
//
// Why this file exists. A remediation action acts on a specific thing — pod
// order-svc-7d9, queue payments-in, systemd unit nginx, redis client
// 10.0.0.4:52310. The investigation already knows which thing that is: the
// alert's own labels name it, because that is how an alert rule selects the
// object it fires on. The pg resolvers in evidence_resolver.go recover a pid
// and a role from pg_stat_activity rows; the other seven actions had no such
// recorded observation, so they refused. The gap was never "the platform
// cannot find a pod name" — it was that the evidence chain carried the
// alert's identity nowhere, so there was nothing for a resolver to read.
//
// So the subject is recorded as its own evidence item, produced by the
// investigator from the firing alert's labels, and read back by the
// resolvers. It is an observation, not an inference: these are the labels
// that caused the alert, recorded verbatim.
//
// The rule is the same one the pg resolvers already follow. A recorded
// subject is used; a missing or self-contradictory one is a refusal. Two
// labels that name two different pods is a contradiction, and picking one
// would be the guess this whole mechanism exists to prevent.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SubjectEvidenceTool is the Tool name of the evidence item that carries the
// alert's identity labels.
//
// It is a distinct name from resource_alert (which records the alert id)
// so a reader can tell "which alert fired" from "which object it is about",
// and so the subject can be re-stamped into a contract independently.
const SubjectEvidenceTool = "resource_alert_labels"

// NewSubjectEvidenceItem builds the evidence item that records the entity an
// alert fired about.
//
// labels is stored verbatim. The resolvers each declare which keys they
// accept, so an unexpected label cannot inject a value into an action that
// did not ask for it — adding a "queue" label to the system does not give
// k8s.evict_pod a queue to act on.
func NewSubjectEvidenceItem(alertID, target string, labels map[string]string) EvidenceItem {
	clean := make(map[string]string, len(labels))
	for k, v := range labels {
		if key := strings.TrimSpace(k); key != "" {
			if val := strings.TrimSpace(v); val != "" {
				clean[key] = val
			}
		}
	}
	value := map[string]any{"labels": clean}
	if id := strings.TrimSpace(alertID); id != "" {
		value["alert_id"] = id
	}
	if t := strings.TrimSpace(target); t != "" {
		value["target"] = t
	}
	return EvidenceItem{
		Tool:  SubjectEvidenceTool,
		Query: "alert labels (recorded subject)",
		Value: value,
		Count: len(clean),
	}
}

// subjectLabels returns the recorded subject of an evidence chain.
//
// It reads the first subject item it finds and ignores the rest: two subject
// items would mean the chain was assembled from two alerts, and silently
// merging their labels could produce a combination that was never observed.
// The item's Value is decoded leniently — the contract round-trips evidence
// through JSON, so the same map arrives as map[string]string, map[string]any,
// or a raw JSON string depending on the path.
func subjectLabels(evidence []EvidenceItem) (map[string]string, bool) {
	for _, item := range evidence {
		if item.Tool != SubjectEvidenceTool {
			continue
		}
		return decodeLabelMap(item.Value)
	}
	return nil, false
}

func decodeLabelMap(value any) (map[string]string, bool) {
	switch v := value.(type) {
	case map[string]any:
		// Wrapped form produced by NewSubjectEvidenceItem.
		if raw, ok := v["labels"]; ok {
			return decodeLabelMap(raw)
		}
		// A bare map is accepted so a hand-built or externally-produced
		// subject still reads, but only string values count: a subject is
		// a set of names, and a nested object is not a name.
		out := map[string]string{}
		for k, raw := range v {
			if s, ok := raw.(string); ok {
				if key := strings.TrimSpace(k); key != "" {
					if val := strings.TrimSpace(s); val != "" {
						out[key] = val
					}
				}
			}
		}
		return out, len(out) > 0
	case map[string]string:
		out := map[string]string{}
		for k, s := range v {
			if key := strings.TrimSpace(k); key != "" {
				if val := strings.TrimSpace(s); val != "" {
					out[key] = val
				}
			}
		}
		return out, len(out) > 0
	case string:
		var rows []map[string]any
		if err := json.Unmarshal([]byte(v), &rows); err == nil {
			// A list of subject objects: take the first, for the same
			// single-subject reason the loop above does.
			if len(rows) > 0 {
				return decodeLabelMap(rows[0])
			}
			return nil, false
		}
		var one map[string]any
		if err := json.Unmarshal([]byte(v), &one); err == nil {
			return decodeLabelMap(one)
		}
		return nil, false
	case json.RawMessage:
		return decodeLabelMap(string(v))
	default:
		return nil, false
	}
}

// subjectValue reads one logical field from a recorded subject.
//
// accepted is the ordered set of alert-label keys that legitimately carry
// this field. The order is presentation only; the value is the same either
// way. Two different accepted keys naming two different values is a
// contradiction and refuses, because an alert labelled both
// pod=order-svc-a and k8s_pod=order-svc-b is describing something this
// platform does not yet understand, and guessing which one the tool wants
// is exactly the failure this file refuses to permit.
func subjectValue(labels map[string]string, accepted []string) (string, []string, error) {
	found := map[string]string{}
	var keys []string
	for _, key := range accepted {
		if v, ok := labels[key]; ok && v != "" {
			found[key] = v
			keys = append(keys, key)
		}
	}
	switch len(found) {
	case 0:
		return "", nil, nil
	case 1:
		for _, v := range found {
			return v, keys, nil
		}
	}
	// More than one accepted key carried a value. Identical values are not
	// a contradiction — the same object under two labels is normal — so
	// they collapse to one.
	distinct := map[string]struct{}{}
	for _, v := range found {
		distinct[v] = struct{}{}
	}
	sort.Strings(keys)
	if len(distinct) == 1 {
		for _, v := range found {
			return v, keys, nil
		}
	}
	return "", keys, fmt.Errorf("the alert records %s as %s, which name different objects; "+
		"acting on either one would be a guess about which the operator meant",
		strings.Join(keys, " and "), strings.Join(sortedSet(distinct), " and "))
}

func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// missingSubject builds the refusal for an action whose subject the evidence
// chain does not record. It names the argument and the label keys that would
// have supplied it, so the message tells the reader what to record rather
// than only that something is absent.
func missingSubject(action, arg string, accepted []string) error {
	return fmt.Errorf("no recorded subject names the %s this action acts on: the evidence chain carries no "+
		"%q item, so the alert's own labels were never recorded. Record the alert labels as evidence "+
		"(one of: %s) and the dispatch resolves; until then %s is refused rather than guessed",
		arg, SubjectEvidenceTool, strings.Join(accepted, ", "), action)
}
