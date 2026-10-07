package delivery

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The plan's first design principle is that a node holds no cloud vendor key:
// credentials stay in the manager, the node presents its own tunnel pair, and a
// compromised node therefore cannot spend the operator's budget. That principle
// is the entire reason the LLM gateway exists — without it, "the node holds no
// vendor key" is not a design choice, it is a missing feature.
//
// The runtime half of the property is already asserted, and asserted well. The
// e2e suite plants three decoy credentials in the test runner's own
// environment, proves they are still there, and then fails if any of them
// reaches the node's process environment or the node's agent configuration
// directory. That check is the reason this file is a complement to it and not a
// restatement of it.
//
// The gap is what the e2e cannot see. The harness builds the edge's environment
// by hand — tests/e2e/testenv/edge.go names the variables itself and never
// reads the file an operator installs. That file is a template: install-edge.sh
// renders deploy/install/edge/opskeeper-edge.env.example into
// /etc/opskeeper-edge and substitutes into it. The three assertions already in
// this package that touch that file all ask whether a knob is *present*; none
// asks whether it is *empty*, because "the operator has somewhere to put the
// credential" is a different question from "the operator has not put one
// there".
//
// So the failure this file exists to catch is the plausible one. A node that
// will not authenticate is a node that looks configured and answers nothing,
// and the fastest way to make it authenticate is to fill in a working key in
// the template. That edit is invisible to the e2e, which does not read the
// template; invisible to the bundle checks, which are about the agent binary;
// and it ships a credential to every host that installs.

// edgeEnvTemplate is the file install-edge.sh renders into
// /etc/opskeeper-edge/opskeeper-edge.env. It is the environment an operator
// receives, so it is the last point at which "the node holds no vendor key" is
// still a property of the release rather than of the host.
const edgeEnvTemplate = "deploy/install/edge/opskeeper-edge.env.example"

// credentialShapedName matches the variable names a credential would arrive in.
// It is deliberately broad: the point of the first arm is to be unable to miss
// a name, and the cost of a false positive is an empty value being demanded —
// which is the state this file is supposed to be in anyway.
var credentialShapedName = regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASS|CREDENTIAL)`)

// placeholderValue is the only non-empty value a credential-shaped variable may
// hold in the template. The three that exist are substituted by install-edge.sh
// from the tunnel pair the node was issued at enrollment, which is the node's
// own credential and not a vendor key.
var placeholderValue = regexp.MustCompile(`^__[A-Z_]+__$`)

// vendorKeyPrefixes is the second arm, and it is the one that does not trust
// variable names. A key pasted into a variable nobody named carefully, or
// appended to an endpoint as a query parameter, passes a name-based check and
// fails this one.
//
// The prefixes are per-vendor rather than a generic entropy test on purpose. A
// generic "is this long and random" rule fires on every path, every base URL
// and every model slug the file legitimately mentions, and a check that cries
// wolf on the shipped file gets muted — which is strictly worse than not having
// it.
var vendorKeyPrefixes = []string{
	"sk-", "sk-ant-", "AKIA", "ASIA", "ghp_", "gho_", "github_pat_",
	"xoxb-", "xoxp-", "AIza", "eyJ",
}

// envAssignment matches one `NAME=VALUE` line.
var envAssignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// scanForCredentialMaterial is the only implementation of the rule, and both
// the gate and its own self-verification below run it — a self-check that
// exercises a copy of the logic proves the copy is correct, not the gate.
func scanForCredentialMaterial(body string) string {
	var out []string
	for lineNo, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		// Comments and blanks are documentation. Only real assignments are
		// configuration, and only configuration can reach a host.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		match := envAssignment.FindStringSubmatch(trimmed)
		if match == nil {
			continue
		}
		name, rawValue := match[1], match[2]
		// A value's own inline comment is not part of the value. This is not
		// cosmetic: a rule that reads this file's prose as configuration gets
		// "fixed" by deleting the prose, which is the worst available outcome.
		if hash := strings.Index(rawValue, " #"); hash >= 0 {
			rawValue = rawValue[:hash]
		}
		rawValue = strings.TrimSpace(rawValue)
		at := fmt.Sprintf("line %d", lineNo+1)

		if credentialShapedName.MatchString(name) && rawValue != "" &&
			!placeholderValue.MatchString(rawValue) {
			out = append(out, fmt.Sprintf(
				"%s: %s carries a value; a node receives its credential at enrollment or over "+
					"the tunnel, never from the file it is installed with", at, name))
		}
		// The report names the prefix and the line, never the matched text.
		// Printing the value would put a credential into CI logs, which is a
		// second copy of the very thing this file exists to prevent — and CI
		// logs are exactly the place a leaked key is harvested from. The line
		// number is enough for whoever has to fix it, and the prefix is enough
		// to tell a real key from a false positive.
		for _, prefix := range vendorKeyPrefixes {
			if strings.Contains(rawValue, prefix) {
				out = append(out, fmt.Sprintf(
					"%s: %s holds material shaped like a vendor key (%q); the manager resolves "+
						"the real provider credential and the node never sees it", at, name, prefix))
			}
		}
	}
	return strings.Join(out, "\n")
}

func TestTheShippedEdgeEnvironmentCarriesNoCredentialMaterial(t *testing.T) {
	body := read(t, edgeEnvTemplate)

	// The two preconditions are the same discipline the e2e uses with its
	// decoys. A scan that matched nothing because the file stopped looking like
	// what it used to is indistinguishable from a clean file to every assertion
	// layered on top, so the file is asked to still be readable as the thing
	// this check claims to read.
	assignments := 0
	credentialNamed := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := envAssignment.FindStringSubmatch(trimmed); m != nil {
			assignments++
			if credentialShapedName.MatchString(m[1]) {
				credentialNamed++
			}
		}
	}
	if assignments == 0 {
		t.Fatalf("%s yielded no assignments; the scan is reading nothing and cannot fail",
			edgeEnvTemplate)
	}
	if credentialNamed == 0 {
		t.Fatalf("%s names no credential-shaped variable; the pattern has drifted away from "+
			"the file it is supposed to police", edgeEnvTemplate)
	}

	if found := scanForCredentialMaterial(body); found != "" {
		t.Errorf("%s carries credential material:\n%s\n"+
			"the node's only credential is the tunnel pair it was issued at enrollment. One that "+
			"reaches a host from this file reaches every host that installs this release.",
			edgeEnvTemplate, found)
	}
}

// TestTheEdgeEnvironmentTemplateSubstitutesOnlyTheTunnelPair closes the other
// half. Even a perfectly clean template becomes a credential on every host the
// moment install-edge.sh substitutes a real one into it, and that substitution
// is the one edit the first check cannot see: the offending value is not in the
// file being scanned, it is what the file becomes on a host.
//
// So the set of placeholders the installer may fill is itself the assertion.
// Three are legitimate and all three come from the enrollment exchange.
// Anything else — a provider key read from the operator's environment, a
// gateway secret, a cloud account key — is a fourth placeholder, and it would
// be invisible everywhere else.
func TestTheEdgeEnvironmentTemplateSubstitutesOnlyTheTunnelPair(t *testing.T) {
	installer := read(t, "deploy/install/edge/install-edge.sh")

	allowed := map[string]string{
		"__CLOUD_ADDR__": "the manager address the node dials",
		"__ACCESS_KEY__": "half of the node's own tunnel pair, issued at enrollment",
		"__SECRET_KEY__": "the other half of the node's own tunnel pair",
	}

	// `sed -e "s|__NAME__|...|g"` — matched on the placeholder rather than the
	// whole expression, so reformatting the command does not read as a new one.
	substitutionRE := regexp.MustCompile(`s\|(__[A-Z_]+__)\|`)
	used := map[string]bool{}
	for _, m := range substitutionRE.FindAllStringSubmatch(installer, -1) {
		used[m[1]] = true
	}

	if len(used) == 0 {
		t.Fatalf("install-edge.sh substitutes no placeholder into %s; the template-rendering "+
			"path this check polices has changed shape", edgeEnvTemplate)
	}

	var unexpected []string
	for name := range used {
		if _, ok := allowed[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		reasons := make([]string, 0, len(allowed))
		for name, why := range allowed {
			reasons = append(reasons, name+" is "+why)
		}
		sort.Strings(reasons)
		t.Errorf("install-edge.sh substitutes %s into %s. Only the node's own tunnel pair may "+
			"come from the installer (%s). Every other credential has to stay in the manager, or "+
			"the node ends up holding one the moment it is installed",
			strings.Join(unexpected, ", "), edgeEnvTemplate, strings.Join(reasons, "; "))
	}

	// The reverse direction matters as much. A placeholder the installer no
	// longer fills is a line the operator receives as the literal
	// __ACCESS_KEY__, which is credential-shaped, never authenticates, and
	// reads exactly like a corrupted value.
	for name, why := range allowed {
		if !used[name] {
			t.Errorf("install-edge.sh no longer substitutes %s (%s); the template ships it as a "+
				"literal, and an operator cannot tell a placeholder from a broken value",
				name, why)
		}
	}
}

// TestTheCredentialScanFindsAPlantedKey is why the scan above is trustworthy. A
// check that cannot fail is not a check, and this failure mode is the quiet
// one: a regex that stops matching yields an empty match set, and an empty
// match set is indistinguishable from a clean file to every assertion built on
// it.
//
// So the scan runs against a body with each violation planted in it and has to
// find every one. Four plants, not one: a value in a credential-shaped name, a
// vendor key in a variable nobody named carefully, a vendor key appended to a
// URL, and the one that is easy to get wrong — a real key sitting in a comment,
// which must NOT be reported, because a rule that fires on the file's own
// documentation gets the documentation deleted.
func TestTheCredentialScanFindsAPlantedKey(t *testing.T) {
	const planted = `OPSKEEPER_EDGE_ID=
OPSKEEPER_EDGE_AGENT_TOKEN=sk-planted-must-be-found
OPSKEEPER_EDGE_SCRAPE_URL=https://prom.example/api?key=AKIAPLANTEDEXAMPLE
OPSKEEPER_EDGE_AGENT_TOKEN=
# OPSKEEPER_EDGE_AGENT_TOKEN=sk-only-in-a-comment
OPSKEEPER_EDGE_CLOUD_ADDR=__CLOUD_ADDR__
`

	// The expectations name the prefix rather than the planted literal on
	// purpose: scanForCredentialMaterial reports the prefix and the line, and
	// a self-verification that demanded the full string back would push the
	// implementation toward echoing key material.
	found := scanForCredentialMaterial(planted)
	for _, want := range []string{"line 2", "line 3", "AKIA"} {
		if !strings.Contains(found, want) {
			t.Errorf("scan missed %q; it reported only:\n%s", want, found)
		}
	}
	for _, plantedKey := range []string{"sk-planted-must-be-found", "AKIAPLANTEDEXAMPLE"} {
		if strings.Contains(found, plantedKey) {
			t.Errorf("scan echoed the planted key %q into its report; a violation message "+
				"becomes a second copy of the credential, in CI logs:\n%s", plantedKey, found)
		}
	}
	for _, unwanted := range []string{
		"sk-only-in-a-comment",
		"__CLOUD_ADDR__",
		"line 4", // the empty re-assignment is the clean case
		"line 6", // the placeholder is the clean case
	} {
		if strings.Contains(found, unwanted) {
			t.Errorf("scan reported %q, which is a clean line:\n%s", unwanted, found)
		}
	}
}
