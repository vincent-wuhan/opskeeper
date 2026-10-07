package main

import (
	"os"
	"testing"

	managersvcaiops "github.com/vincent-wuhan/opskeeper/core/manager/service/aiops"
)

// TestKernelEnvParsing covers the cmd-level boot-time decision:
//
//	unset / empty → KernelPigSDK (nobody chose; the product's default)
//	"graph" → KernelGraph
//	"pig"   → KernelPig
//	"pig-sdk" → KernelPigSDK
//	"garbage" → KernelLegacy (with a warning - see main())
//
// The actual env wiring lives in main(); this test exercises the
// parser the env value flows through, plus emulates the env-set
// path via os.Setenv.
//
// The two fallbacks are the assertion. Empty and unrecognised used to be
// the same answer, and separating them is what let the default move to the
// embedded SDK driver without taking anyone's explicit choice with it: a
// deployment that set "pig", "graph" or "legacy" is unaffected, and one
// that set a typo stays where it was and is told so.
func TestKernelEnvParsing(t *testing.T) {
	cases := []struct {
		name   string
		envVal string
		setEnv bool
		want   managersvcaiops.Kernel
	}{
		{"unset", "", false, managersvcaiops.KernelPigSDK},
		{"empty_string", "", true, managersvcaiops.KernelPigSDK},
		{"whitespace_only", "   ", true, managersvcaiops.KernelPigSDK},
		{"graph_lower", "graph", true, managersvcaiops.KernelGraph},
		{"graph_upper", "GRAPH", true, managersvcaiops.KernelGraph},
		{"graph_padded", "  graph  ", true, managersvcaiops.KernelGraph},
		{"legacy_explicit", "legacy", true, managersvcaiops.KernelLegacy},
		{"pig_lower", "pig", true, managersvcaiops.KernelPig},
		{"pig_upper", "PIG", true, managersvcaiops.KernelPig},
		{"pig_padded", "  pig  ", true, managersvcaiops.KernelPig},
		{"pig_sdk_lower", "pig-sdk", true, managersvcaiops.KernelPigSDK},
		{"pig_sdk_upper", "PIG-SDK", true, managersvcaiops.KernelPigSDK},
		{"pig_sdk_underscore", "pig_sdk", true, managersvcaiops.KernelPigSDK},
		{"sdk_alias", "sdk", true, managersvcaiops.KernelPigSDK},
		// The bare spelling must keep meaning the bare loop. An operator who
		// has been running "pig" through a cutover must not find that a
		// PiG upgrade silently moved them onto a driver that opens a
		// session per turn.
		{"pig_is_not_the_sdk_driver", "pig", true, managersvcaiops.KernelPig},
		{"garbage", "this-is-not-a-kernel", true, managersvcaiops.KernelLegacy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prev, hadPrev := os.LookupEnv("OPSKEEPER_AGENT_KERNEL")
			t.Cleanup(func() {
				if hadPrev {
					_ = os.Setenv("OPSKEEPER_AGENT_KERNEL", prev)
				} else {
					_ = os.Unsetenv("OPSKEEPER_AGENT_KERNEL")
				}
			})
			if c.setEnv {
				_ = os.Setenv("OPSKEEPER_AGENT_KERNEL", c.envVal)
			} else {
				_ = os.Unsetenv("OPSKEEPER_AGENT_KERNEL")
			}
			got := managersvcaiops.ParseKernel(os.Getenv("OPSKEEPER_AGENT_KERNEL"))
			if got != c.want {
				t.Errorf("ParseKernel(env=%q,set=%v) = %q, want %q", c.envVal, c.setEnv, got, c.want)
			}
		})
	}
}

// TestTheDriverPredicatesAnswerDifferentQuestions keeps the two "which
// driver" helpers from collapsing into one.
//
// UsesChatRuntime and UsesPiGSession look redundant and are not. The first
// asks whether chat turns bypass the legacy for-loop, which every PiG kernel
// does. The second asks whether the loop underneath is a coding.Session,
// which decides whether the assembly has a *pigcoding.Runtime to hand it.
//
// A caller that used the first to answer the second would give the Session
// driver a nil runtime. That is why the failure is asserted here rather than
// left to the nil check in newAgentKernel: the nil check protects the
// process, this test protects the question.
func TestTheDriverPredicatesAnswerDifferentQuestions(t *testing.T) {
	cases := []struct {
		kernel      managersvcaiops.Kernel
		wantRuntime bool
		wantSession bool
	}{
		{managersvcaiops.KernelLegacy, false, false},
		{managersvcaiops.KernelGraph, true, false},
		{managersvcaiops.KernelPig, true, false},
		{managersvcaiops.KernelPigSDK, true, true},
	}
	for _, c := range cases {
		if got := c.kernel.UsesChatRuntime(); got != c.wantRuntime {
			t.Errorf("%q.UsesChatRuntime() = %v, want %v", c.kernel, got, c.wantRuntime)
		}
		if got := c.kernel.UsesPiGSession(); got != c.wantSession {
			t.Errorf("%q.UsesPiGSession() = %v, want %v", c.kernel, got, c.wantSession)
		}
	}
}

// TestTheBootLogCanTellGuessedFromInstructed pins the second half of the
// fallback: a value that is not a kernel must be reported as such, because
// the whole justification for falling back to the pre-2.0 loop is that the
// operator gets told.
func TestTheBootLogCanTellGuessedFromInstructed(t *testing.T) {
	for _, value := range []string{"legacy", "graph", "pig", "pig-sdk", "PIG-SDK", " sdk "} {
		if !managersvcaiops.IsKnownKernel(value) {
			t.Errorf("IsKnownKernel(%q) = false; ParseKernel accepts it, so the boot log "+
				"would warn about a value it honoured", value)
		}
	}
	for _, value := range []string{"", "  ", "gpt", "pig-sdkd", "graph2", "session"} {
		if managersvcaiops.IsKnownKernel(value) {
			t.Errorf("IsKnownKernel(%q) = true, but no kernel answers to it", value)
		}
	}
}
