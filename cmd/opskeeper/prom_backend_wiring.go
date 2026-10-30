package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promauth"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/setting"
	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/toolcore"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/pluginhost"

	vmplugin "github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics"
)

// promBackendPlan is the assembly layer's answer to the only question
// OPSKEEPER_PROM_BACKEND asks: which implementation answers the `prom` signal.
//
// It is a plan rather than a bool because the third-party path is not "the
// same thing with a different URL" — it is a different plugin package, admitted
// through the same kernel gate, registering the same signal name. Naming the
// package here is what lets the wiring in main.go stay a single `if` and lets a
// test assert the default path without reading main.go at all.
type promBackendPlan struct {
	// Backend is the normalised identifier, always one of the two legal
	// values. An unset config lands on the first-party one.
	Backend string

	// ThirdParty is the preinstalled third-party plugin package to activate
	// INSTEAD of the first-party Prometheus datasource. Empty means the
	// first-party path runs, unchanged, exactly as before this field existed.
	ThirdParty string
}

// planPromBackend maps a configured backend identifier to an activation plan.
//
// An unrecognised value is an error rather than a fallback. Falling back would
// answer the operator's queries from a server they did not ask for, and the
// symptom — a healthy dashboard showing the wrong cluster's numbers — is one
// nobody connects back to a typo in a deployment variable. main() turns the
// error into a startup failure; this function is where the rule lives so a test
// can hold it.
//
// An empty identifier is the default path, not a typo: a config struct built by
// hand rather than by config.Load() leaves the string zero.
func planPromBackend(backend string) (promBackendPlan, error) {
	switch strings.TrimSpace(backend) {
	case "", config.PromBackendPrometheus:
		return promBackendPlan{Backend: config.PromBackendPrometheus}, nil

	case config.PromBackendVictoriaMetrics:
		return promBackendPlan{
			Backend:    config.PromBackendVictoriaMetrics,
			ThirdParty: vmplugin.PluginName,
		}, nil

	default:
		return promBackendPlan{}, fmt.Errorf(
			"unknown prom backend %q: legal values are %q and %q",
			backend, config.PromBackendPrometheus, config.PromBackendVictoriaMetrics)
	}
}

// vmCredentialEnv holds the environment variables that carry VictoriaMetrics
// credentials. They are read only — nothing here is written to a database, a
// log line, or an error message, and there is deliberately no config field or
// UI setting behind them. A credential that can be edited through a UI ends up
// in system_settings and in audit records; these stay in the process
// environment where the deployment put them and nowhere else.
const (
	envVMBearerToken   = "OPSKEEPER_VM_BEARER_TOKEN"
	envVMBasicUser     = "OPSKEEPER_VM_BASIC_USERNAME"
	envVMBasicPassword = "OPSKEEPER_VM_BASIC_PASSWORD"
	envVMTLSCAPath     = "OPSKEEPER_VM_TLS_CA_FILE"
)

// vmAuthResolver supplies the VictoriaMetrics credentials, environment only.
//
// There is deliberately no fallback to the first-party resolver. Reusing the
// system_settings-backed Prometheus token when the operator selected a
// different server would send one backend's credential to another backend; a
// missing variable therefore means "no auth header", which fails loudly at the
// server with a 401 rather than quietly leaking a working token.
type vmAuthResolver struct{ cfg promauth.Config }

func (r vmAuthResolver) Resolve(context.Context) (promauth.Config, error) { return r.cfg, nil }

// vmCredentials reads the VictoriaMetrics credentials from the environment.
//
// It returns the pair promauth wants and nothing else. The caller must not log
// the returned struct: it holds a bearer token or a password in the clear.
func vmCredentials() promauth.Config {
	return promauth.Config{
		BearerToken:   os.Getenv(envVMBearerToken),
		BasicUser:     os.Getenv(envVMBasicUser),
		BasicPassword: os.Getenv(envVMBasicPassword),
	}
}

// promQueryFallback is the env-seeded PromQL API root: QueryURL when set,
// URL otherwise. Both the first-party and the third-party client resolve
// through the same hot-reloading resolver, so this value is only what a
// deployment with no system_settings row gets — and both of them compute it
// here so the two cannot drift.
func promQueryFallback(cfg *config.Config) string {
	if cfg.Prom.QueryURL != "" {
		return cfg.Prom.QueryURL
	}
	return cfg.Prom.URL
}

// activateVictoriaMetricsDatasource admits and activates the preinstalled
// VictoriaMetrics package into the control-plane host.
//
// It goes through kernel.Activate — the same entry a third-party tarball
// arrives through, against the same ManagerPolicy() ceiling, with the manifest
// decoded from the package's own pig-ops.yaml rather than synthesised in Go.
// That is what makes the third-party path exercised on every boot that selects
// it rather than only in a test, and it is why the admission result below is a
// startup failure: a manifest that has drifted above the ceiling should stop
// the manager, not be quietly ignored.
func activateVictoriaMetricsDatasource(
	ph *pluginhost.Host,
	cfg *config.Config,
	promResolver *setting.PromResolver,
	log *slog.Logger,
) error {
	pig, err := vmplugin.Manifest()
	if err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}

	// The same TLS posture as the first-party client, with the CA file
	// overridable per backend: a VictoriaMetrics deployment usually presents a
	// different certificate authority from the in-cluster Prometheus, and
	// forcing one deployment to serve both is a constraint with no upside.
	caPath := cfg.Prom.TLSCAPath
	if v := os.Getenv(envVMTLSCAPath); v != "" {
		caPath = v
	}

	timeout := toolcore.QueryPromqlCallTimeout
	hc, err := promauth.BuildClient(
		promauth.TLSConfig{
			Insecure: cfg.Prom.TLSInsecure,
			CAPath:   caPath,
		},
		vmAuthResolver{cfg: vmCredentials()},
		timeout,
	)
	if err != nil {
		return fmt.Errorf("build http client: %w", err)
	}

	// The resolver is asked for the base URL on every call, so an admin UI
	// edit moves the backend without a restart — the same operational
	// property the first-party client has. Nothing about the client itself is
	// VictoriaMetrics-aware beyond the base URL.
	client := vmplugin.NewWithResolverAndHTTPClient(promResolver, hc, log.With(slog.String("comp", "victoriametrics")))

	if err := ph.Kernel().Activate(context.Background(), pig, vmplugin.Contribution(client)); err != nil {
		return fmt.Errorf("activate %s: %w", vmplugin.PluginName, err)
	}
	return nil
}

// promResidualFirstPartySurfaces are the control-plane surfaces that build
// their own metrics client and therefore keep pointing at first-party
// Prometheus regardless of OPSKEEPER_PROM_BACKEND.
//
// This list is not documentation. It is logged at startup whenever a third-party
// backend is selected, because the split is invisible from the outside: the
// tool registry answers from VictoriaMetrics, and an operator debugging an
// alert still finds Prometheus on the report path, with nothing in either
// response saying which is which. The list is sorted so the log line is stable
// across builds and greppable across releases — a surface added to the split
// has to be added here to become visible, which is the point.
var promResidualFirstPartySurfaces = []string{
	"cmd/opskeeper integrations probe (prometheus first-contact check)",
	"core/manager/biz/aiops/server metric-domain handler",
	"core/manager/biz/aiops/alert preview (PreviewPromQuerier)",
	"core/manager/biz/aiops/report data source",
	"dashboard Monitor page proxy",
	"alert-rule evaluation pipeline",
}

// promResidualSurfaceLog renders the split-plane list for the startup log.
func promResidualSurfaceLog() []string {
	out := append([]string(nil), promResidualFirstPartySurfaces...)
	sort.Strings(out)
	return out
}
