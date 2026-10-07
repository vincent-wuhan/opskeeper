package grafana

// This file is what the settings cut bought.
//
// The Grafana service held `*setting.Service` by name, so every path that
// reads configuration — Test, Sync, BootstrapEmbedded, and the client build
// all four of them start there — could only be reached with a settings table,
// a database, and the setting domain in the build graph. The tests beside this
// one said so out loud: "this is the only logic that runs without a live
// Grafana". That sentence was a measurement of the dependency, not of Grafana.
//
// The port changed that. The fake below is the whole of what it takes, and it
// is a map. Nothing in this file imports the setting domain, and that is the
// assertion that matters: if a future change puts a setting type back into
// the port signature, this file stops compiling, which is the failure the
// `core/domain` guard reports more politely.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// fakeSettings is a map. It is deliberately the smallest thing that can
// satisfy the port, because the size is the claim: a port that can be
// satisfied by a map is a port about data, and a port about data is one whose
// producer can be replaced.
type fakeSettings map[string]string

func (f fakeSettings) Get(_ context.Context, category, key string) (string, bool, error) {
	v, ok := f[category+"/"+key]
	return v, ok, nil
}

func (f fakeSettings) Set(_ context.Context, category, key, value string, _ bool) error {
	f[category+"/"+key] = value
	return nil
}

// The compile-time half, stated where it cannot be argued with: this fake is
// the entire production surface the port asks of a test.
var _ domain.SettingStore = fakeSettings{}

func TestTheClientBuildsFromAServiceAccountToken(t *testing.T) {
	t.Parallel()
	s := New(fakeSettings{
		string(domain.SettingCategoryGrafana) + "/" + domain.SettingKeyGrafanaRootURL: "https://grafana.example",
		string(domain.SettingCategoryGrafana) + "/" + domain.SettingKeyGrafanaSAToken: "sa-1",
	}, false, nil)
	c, err := s.client(t.Context())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if c == nil {
		t.Fatal("client is nil with a root url and a token present")
	}
}

// bearerSeen drives one real request through the service and reports the
// Authorization header Grafana would have received.
//
// The first version of the precedence test below asserted only that a client
// was built, and it stayed green when the two credentials were swapped — a
// test whose name claimed a property it did not check, which is the same
// failure mode as a gate that runs nothing. The token is unexported inside
// pkg/grafana, so the only honest way to observe which one won is to let the
// service talk to something and read what it sent.
func bearerSeen(t *testing.T, set fakeSettings) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"database":"ok","version":"11.0.0"}`))
	}))
	t.Cleanup(srv.Close)
	set[string(domain.SettingCategoryGrafana)+"/"+domain.SettingKeyGrafanaRootURL] = srv.URL
	if err := New(set, false, nil).Test(t.Context()); err != nil {
		t.Fatalf("Test against the stub Grafana: %v", err)
	}
	return got
}

// sa_token wins over api_key, and the reason is in the service's own comment:
// the bootstrap path mints the former, the operator pastes the latter. Both
// land in the same header, so an operator cannot tell from Grafana's side
// which credential was used — and an inversion would silently break the
// embedded Grafana while every other test stayed green.
func TestTheServiceAccountTokenWinsOverThePastedAPIKey(t *testing.T) {
	t.Parallel()
	got := bearerSeen(t, fakeSettings{
		string(domain.SettingCategoryGrafana) + "/" + domain.SettingKeyGrafanaSAToken: "sa-1",
		string(domain.SettingCategoryGrafana) + "/" + domain.SettingKeyGrafanaAPIKey:  "api-1",
	})
	if got != "Bearer sa-1" {
		t.Errorf("Authorization = %q, want %q; sa_token is the credential the embedded "+
			"Grafana minted and it has to win", got, "Bearer sa-1")
	}
}

func TestThePastedAPIKeyIsTheFallbackWhenNoTokenWasMinted(t *testing.T) {
	t.Parallel()
	got := bearerSeen(t, fakeSettings{
		string(domain.SettingCategoryGrafana) + "/" + domain.SettingKeyGrafanaAPIKey: "api-1",
	})
	if got != "Bearer api-1" {
		t.Errorf("Authorization = %q, want %q; an operator running their own Grafana cannot "+
			"mint a service-account token, so the pasted key has to work on its own", got, "Bearer api-1")
	}
}

// The two "not configured" answers have to stay distinguishable, and both have
// to be distinguishable from "the settings service is missing", which is a
// wiring fault rather than an operator's. Collapsing the first two would tell
// an operator to fill in a field they already filled in.
func TestTheConfigurationErrorsSayWhichFieldIsMissing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		set  fakeSettings
		want string
	}{
		{
			name: "no root url",
			set:  fakeSettings{string(domain.SettingCategoryGrafana) + "/" + domain.SettingKeyGrafanaSAToken: "sa-1"},
			want: "root_url",
		},
		{
			name: "no credential at all",
			set:  fakeSettings{string(domain.SettingCategoryGrafana) + "/" + domain.SettingKeyGrafanaRootURL: "https://grafana.example"},
			want: "sa_token",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(c.set, false, nil).client(t.Context())
			if err == nil {
				t.Fatal("an incomplete configuration produced a client")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name %q", err, c.want)
			}
		})
	}
}

// A nil port is a wiring fault, and it has to read as one. Before the cut this
// was a nil *setting.Service, which is not the same thing: it passes an
// `if settings == nil` check written for an interface while being a typed nil,
// and the first symptom is a panic inside a health or sync call.
func TestANilPortIsAConfigurationErrorAndNotAPanic(t *testing.T) {
	t.Parallel()
	if _, err := New(nil, false, nil).client(t.Context()); err == nil {
		t.Fatal("a nil settings port produced a client")
	}
}
