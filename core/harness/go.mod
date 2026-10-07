// Module harness is the evaluation plane: the golden-case corpus, the
// injectors that stage a failure, the judges that score a response, and
// the loop that runs a whole incident end to end.
//
// Invariants:
//
//   - harness depends on core (contracts), core/pig (the model
//     vocabulary) and the standard library. Nothing else. It reaches no
//     database, no HTTP client, and no other Opskeeper module.
//   - The one capability it needs from outside — a model completion — is
//     taken as pigmodel.Completer, injected by the caller. The harness
//     never learns how a provider is configured, and a rubric that could
//     import a provider client would drag that client's whole dependency
//     graph into every evaluation run.
//   - core/pig was added here deliberately (decision 67). The judge
//     receives a settled *pigai.AssistantMessage and reads its text off the
//     content blocks, so declaring a harness-local one-method interface
//     over a hand-rolled response struct would buy nothing: the harness
//     would still be reading PiG's message type, only behind a second
//     vocabulary that has to be kept in step with the first. The zero-
//     provider-SDK property is preserved — pigmodel is a contract, not
//     a client — and PiG stays confined to the one module allowed to name
//     it: this module reaches PiG's types through core/pig/pigai, which
//     is aliases, so no import path under github.com/MichaelKinsy/PiG
//     appears here at all.
//   - harness holds no production logic. Nothing in the control plane
//     imports it to make a decision; it is read by the eval command and by
//     tests.
//
// The zero-dependency property is the point, not a coincidence: it is what
// lets a third party fork the corpus and run it without resolving the
// server.
module github.com/vincent-wuhan/opskeeper/core/harness

go 1.26.0

require github.com/vincent-wuhan/opskeeper/core/pig v0.0.0

require (
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	github.com/MichaelKinsy/PiG v0.4.0 // indirect
	github.com/MichaelKinsy/PiG/extensions/sdk v0.4.0 // indirect
	github.com/aws/aws-sdk-go-v2 v1.41.7 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.10 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.17 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.16 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.50.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.42.1 // indirect
	github.com/aws/smithy-go v1.25.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/vincent-wuhan/opskeeper/core v0.0.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

// Sibling modules resolve by path during development; the workspace covers
// this in a normal build, the replace keeps a bare module directory
// buildable in CI jobs that disable workspaces.
//
// PiG itself is neither required nor replaced here. This module names no
// PiG symbol of its own — it reads PiG's types through core/pig/pigai,
// which is aliases — so core/pig's own requirement is what pins the
// version, and adding a second one would create an independent version
// constraint on a dependency this module cannot even import directly.
replace (
	github.com/vincent-wuhan/opskeeper/core => ../
	github.com/vincent-wuhan/opskeeper/core/pig => ../pig
)
