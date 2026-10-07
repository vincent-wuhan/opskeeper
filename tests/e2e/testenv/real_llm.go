//go:build e2e

package testenv

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// RealLLMEnv is the variable that points this harness at a real inference
// engine instead of the fake.
//
// It exists because "the delivery path is green" and "our request survived a
// real model" are different claims, and only the fake has ever been
// exercised. The fake accepts every shape; a real engine does not, and the
// thing that breaks first in production is the translation rather than the
// connection.
//
// Loopback only, and that restriction is the whole design rather than a
// precaution. This harness scrubs every credential-shaped variable out of
// every child process (see credentialShapedEnv), because one of the
// properties it must demonstrate is that a node's environment holds no cloud
// vendor key. A real provider needs a key by definition, so pointing this at
// a hosted endpoint would either smuggle a credential into the run or teach
// the next reader that secrets belong in a test env file. Neither is a
// trade worth making for one extra assertion.
//
// A local engine (ollama, llama.cpp, vLLM) is the shape that fits: real
// tokenization, real streaming, real tool-call protocol, and no secret to
// leak. It does not prove compatibility with any particular hosted vendor's
// dialect — see RealLLMLimits for what a green run here does and does not
// establish.
const RealLLMEnv = "E2E_REAL_LLM_BASE_URL"

// RealLLMLimits is what a green real-model run establishes, written down so
// the next reader does not have to infer it from a green log.
//
// It DOES establish: our request shape is served by a real inference engine;
// streaming frames are produced by real token generation rather than by a
// stub writing a canned string; tool declarations are accepted by a real
// model's tool-calling protocol.
//
// It does NOT establish: that a specific hosted vendor accepts the same
// shape, or that the answers are any good. A local model is a real model and
// also a weak one, and confusing those two is the error this note exists to
// prevent.
const RealLLMLimits = "a green run proves the translation survives a real engine; " +
	"it does not prove any hosted vendor's dialect, and it says nothing about answer quality"

// RealLLMBaseURL returns the configured real endpoint, or "" when the harness
// should keep using the fake.
//
// A non-loopback value is refused rather than honoured, with the reason
// returned as the error, because the caller is a test harness and silently
// ignoring an operator's setting is how a run ends up testing something other
// than what it claims.
func RealLLMBaseURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv(RealLLMEnv))
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s=%q is not a URL: %w", RealLLMEnv, raw, err)
	}
	host := parsed.Hostname()
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return "", fmt.Errorf(
			"%s=%q is not a loopback address, and this harness scrubs credential-shaped "+
				"variables out of every child process, so a hosted endpoint would either leak a "+
				"key into a run that must demonstrate no node holds one, or push this harness "+
				"toward storing secrets in a file. Point it at a local engine (ollama, llama.cpp, "+
				"vLLM) instead", RealLLMEnv, raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// RealLLMModelEnv names the model the real engine serves.
//
// It is separate from the endpoint because the endpoint does not know which
// model the operator has pulled, and a run that silently picked one would
// report a green result for a model nobody chose.
const RealLLMModelEnv = "E2E_REAL_LLM_MODEL"

// DefaultRealLLMModel is used when RealLLMModelEnv is unset.
//
// It is a deliberately small default. The point of this path is to prove the
// translation survives a real engine, and the cheapest model that can hold a
// tool schema does that as well as a large one does — while a large one would
// make the run slow enough that people stop running it.
const DefaultRealLLMModel = "qwen2.5:1.5b"

// RealLLMModel is the model the real engine serves.
func RealLLMModel() string {
	if m := strings.TrimSpace(os.Getenv(RealLLMModelEnv)); m != "" {
		return m
	}
	return DefaultRealLLMModel
}

// UsingRealLLM reports whether this run's manager will call a real engine
// instead of the fake.
//
// It exists for the assertions that only make sense in one of the two modes.
// A test that hardcodes "the reply contains the string the fake was told to
// serve" is not wrong in fake mode and is not merely useless in real mode --
// it is a test that cannot be run at all, which is how the real path would
// have stayed unexercised: the assertion is the thing that keeps the fake
// honest, and dropping it to make a real run possible drops the check too.
func UsingRealLLM() bool {
	base, err := RealLLMBaseURL()
	return err == nil && base != ""
}
