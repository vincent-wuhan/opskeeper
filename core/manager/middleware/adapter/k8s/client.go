// client.go — a Kubernetes API client built on net/http, not client-go.
//
// Why not client-go. This adapter needs ten verbs: GET a version, LIST a few
// resource kinds, PATCH two of them, POST an eviction, and one exec
// upgrade. client-go brings a 60-package dependency tree, a generated
// clientset for every API group that has ever existed, and an informer cache
// — and none of that is used here. The API this adapter speaks is a plain
// HTTPS JSON API with a bearer token, and the version skew that client-go
// exists to paper over (typed structs that drifted between Kubernetes
// releases) is precisely the part this adapter does not want: reading
// `status.conditions[type=Ready].status` out of an untyped document costs a
// few lines and never breaks on a field that moved.
//
// What is deliberately left out. Exec credential plugins (`users[].exec`) are
// refused rather than ignored: a kubeconfig that authenticates by shelling
// out to an SSO helper cannot be honoured by a client with no shell, and
// accepting the config while silently dropping its auth would turn into a
// 401 at the worst moment. The refusal names the reason.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultTimeout = 30 * time.Second
	// defaultRowLimit caps a list response. The Kubernetes API returns a
	// cluster's worth of pods in one document — a 5000-pod cluster is ~40MB
	// of JSON — and a model that must read all of it to answer "is anything
	// CrashLooping" gives a worse answer than one that reads a page.
	defaultRowLimit = 200
	maxRowLimit     = 2000
	// maxResponseBytes bounds what a single response may cost in memory.
	// The API's own list pagination is used where it matters (node, pod and
	// deployment lists), so this is a backstop against a proxy that ignores
	// `limit`, not the mechanism.
	maxResponseBytes = 32 << 20
)

// kubeClient is a minimal, read/write Kubernetes API client.
type kubeClient struct {
	// base is the API server root, with no trailing slash: "https://host:6443".
	base string
	// token is the bearer credential. Empty means anonymous, which a local
	// `kubectl proxy` endpoint legitimately accepts.
	token string
	// http is the transport. It carries the cluster CA and, when the
	// kubeconfig has one, the client certificate.
	http *http.Client
	// timeout bounds one request, context deadline separate from the
	// caller's: a caller that passes a background context still gets a
	// bounded call.
	timeout time.Duration
	// namespace is the namespace the credential is scoped to — the
	// ServiceAccount namespace in-cluster, or the current kubeconfig
	// context's. It is reported for context and never injected as a tool
	// argument: a namespace the operator did not name is not a namespace
	// the adapter may act in.
	namespace string
}

// apiError is a Kubernetes Status object turned into a Go error.
//
// The API answers every failure with a structured Status carrying a
// human-readable message, and that message is the difference between
// "evict_pod failed" and "Cannot evict pod as it would violate the pod's
// disruption budget". The second one tells an operator what to do.
type apiError struct {
	Method  string
	Path    string
	Code    int
	Reason  string
	Message string
}

func (e *apiError) Error() string {
	detail := strings.TrimSpace(e.Message)
	if detail == "" {
		detail = http.StatusText(e.Code)
	}
	if e.Reason != "" {
		return fmt.Sprintf("k8s: %s %s: %d %s: %s", e.Method, e.Path, e.Code, e.Reason, detail)
	}
	return fmt.Sprintf("k8s: %s %s: %d: %s", e.Method, e.Path, e.Code, detail)
}

// IsNotFound reports whether an error is a 404 from the API server.
//
// It exists so a caller can distinguish "the pod is gone" from "the token is
// not allowed to see the pod": Kubernetes answers 403 for a resource outside
// the credential's RBAC, and a caller that treated both as "gone" would
// report a successful eviction of a pod it never saw.
func IsNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound
}

func newAPIError(method, path string, code int, raw []byte) error {
	e := &apiError{Method: method, Path: path, Code: code}
	var status struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &status); err == nil {
		e.Reason = status.Reason
		e.Message = status.Message
	}
	if e.Message == "" && len(raw) > 0 && len(raw) <= 512 {
		// A non-Status body (a proxy's HTML error page, say) is still the
		// only explanation the operator will get.
		e.Message = strings.TrimSpace(string(raw))
	}
	return e
}

// do performs one request and decodes the response body into out.
//
// out may be nil. body may be nil. contentType is set only for requests that
// carry one, because a GET with a Content-Type confuses some API proxies and
// a PATCH without one is rejected with a 415.
func (c *kubeClient) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	timeout := c.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("k8s: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("k8s: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("k8s: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(method, path, resp.StatusCode, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("k8s: decode %s %s: %w", method, path, err)
	}
	return nil
}

func (c *kubeClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, out)
}

// getRaw performs a GET whose response is not JSON.
//
// Pod logs are the reason: the API answers a log request with the container's
// bytes and a Content-Type of text/plain, so a decoder would fail on the
// first line that happens not to be valid JSON — which is most of them. The
// bounds are the same as every other call, because a log request is exactly
// as capable of returning a gigabyte as a list request is.
func (c *kubeClient) getRaw(ctx context.Context, path string) ([]byte, error) {
	timeout := c.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("k8s: build GET %s: %w", path, err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("k8s: read GET %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(http.MethodGet, path, resp.StatusCode, raw)
	}
	return raw, nil
}

// patch applies a strategic-merge (for objects with lists of like-named
// fields) or JSON-merge patch. Both content types are accepted by every
// Kubernetes API since 1.15; the merge-patch type is used everywhere here
// because every patch this adapter sends is a scalar replacement — replicas,
// a label, an annotation, unschedulable — where the two strategies agree.
func (c *kubeClient) patch(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("k8s: encode patch: %w", err)
	}
	return c.do(ctx, http.MethodPatch, path, "application/merge-patch+json", body, out)
}

func (c *kubeClient) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("k8s: encode body: %w", err)
	}
	return c.do(ctx, http.MethodPost, path, "application/json", body, out)
}

// ── DSN parsing ────────────────────────────────────────────────────────

// newKubeClient builds a client from a connection DSN.
//
// Accepted forms, in the order they are tried:
//
//	kubeconfig:///abs/path/to/config   a kubeconfig on disk
//	kubeconfig:                        $KUBECONFIG, else ~/.kube/config
//	incluster://                       the pod's ServiceAccount
//	https://host:6443?token=...        a direct endpoint
//	<yaml containing "apiVersion: v1"> an inline kubeconfig
//
// The direct-endpoint form exists because the two ways a manager process
// legitimately reaches a cluster are "it runs in the cluster" and "an
// operator handed it an endpoint and a token" — the second one is what a
// multi-cluster console configures, and requiring it to materialise a
// kubeconfig file first would be asking it to write a credential to disk.
func newKubeClient(dsn string, timeout time.Duration) (*kubeClient, error) {
	d := strings.TrimSpace(dsn)
	if d == "" {
		return nil, errors.New("k8s: DSN is required; a client with no API server cannot be probed")
	}
	switch {
	case strings.HasPrefix(d, "incluster://") || d == "in-cluster":
		return inClusterClient(timeout)
	case strings.HasPrefix(d, "kubeconfig://"):
		path := strings.TrimSpace(strings.TrimPrefix(d, "kubeconfig://"))
		if path == "" {
			path = os.Getenv("KUBECONFIG")
		}
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("k8s: no kubeconfig path and no home directory: %w", err)
			}
			path = filepath.Join(home, ".kube", "config")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("k8s: read kubeconfig %s: %w", path, err)
		}
		return kubeClientFromKubeconfig(raw, filepath.Dir(path), timeout)
	case strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://"):
		return clientFromEndpoint(d, timeout)
	case strings.Contains(d, "apiVersion:") || strings.Contains(d, "clusters:"):
		return kubeClientFromKubeconfig([]byte(d), "", timeout)
	default:
		return nil, fmt.Errorf("k8s: unsupported DSN form %q: expected kubeconfig://<path>, incluster://, an https:// endpoint, or an inline kubeconfig", d)
	}
}

// clientFromEndpoint builds a client from "https://host:port?token=...".
//
// The token is stripped from the URL rather than left in it, because the URL
// is logged and stored: a credential in a query string is a credential in
// every access log between here and the API server.
func clientFromEndpoint(dsn string, timeout time.Duration) (*kubeClient, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("k8s: parse endpoint: %w", err)
	}
	q := u.Query()
	token := q.Get("token")
	if token == "" {
		token = q.Get("bearer_token")
	}
	insecure := q.Get("insecure") == "1" || q.Get("insecure") == "true"
	ca := q.Get("ca_data")
	q.Del("token")
	q.Del("bearer_token")
	q.Del("insecure")
	q.Del("ca_data")
	u.RawQuery = q.Encode()
	u.Path = strings.TrimSuffix(u.Path, "/")

	var pool *x509.CertPool
	if ca != "" {
		pool = x509.NewCertPool()
		pem, err := base64.StdEncoding.DecodeString(ca)
		if err != nil {
			pem = []byte(ca)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("k8s: ca_data is not a PEM certificate")
		}
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{ //nolint:gosec // insecure is an explicit operator choice
		RootCAs:            pool,
		InsecureSkipVerify: insecure,
	}}
	return &kubeClient{
		base:    strings.TrimSuffix(u.String(), "/"),
		token:   token,
		http:    &http.Client{Transport: transport},
		timeout: timeout,
	}, nil
}

// inClusterClient reads the pod's ServiceAccount credential.
//
// The three files it reads are the contract the kubelet mounts into every
// pod, and their absence is the signal that this process is not in a
// cluster — which is a deployment fault to report at Connect, not a reason
// to fall back to something else.
func inClusterClient(timeout time.Duration) (*kubeClient, error) {
	const dir = "/var/run/secrets/kubernetes.io/serviceaccount"
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host = os.Getenv("KUBERNETES_SERVICE_HOST_OVERRIDE")
	}
	if port == "" {
		port = "443"
	}
	if host == "" {
		return nil, errors.New("k8s: KUBERNETES_SERVICE_HOST is unset; this process is not running in a cluster")
	}
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return nil, fmt.Errorf("k8s: read ServiceAccount token: %w", err)
	}
	ns := "default"
	if raw, err := os.ReadFile(filepath.Join(dir, "namespace")); err == nil {
		if s := strings.TrimSpace(string(raw)); s != "" {
			ns = s
		}
	}
	pool := x509.NewCertPool()
	if raw, err := os.ReadFile(filepath.Join(dir, "ca.crt")); err == nil {
		if !pool.AppendCertsFromPEM(raw) {
			return nil, errors.New("k8s: ServiceAccount ca.crt does not contain a PEM certificate")
		}
	} else {
		return nil, fmt.Errorf("k8s: read ServiceAccount CA: %w", err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return &kubeClient{
		base:      "https://" + net.JoinHostPort(host, port),
		token:     strings.TrimSpace(string(token)),
		http:      &http.Client{Transport: transport},
		timeout:   timeout,
		namespace: ns,
	}, nil
}

// kubeconfig is the subset of the kubeconfig schema this client honours.
//
// It is a subset on purpose: the fields it does not decode are refused below
// when they are the ones carrying the credential, rather than being dropped
// silently.
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
			TLSServerName            string `yaml:"tls-server-name"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			TokenFile             string `yaml:"tokenFile"`
			ClientCertificate     string `yaml:"client-certificate"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKey             string `yaml:"client-key"`
			ClientKeyData         string `yaml:"client-key-data"`
			Exec                  any    `yaml:"exec"`
			AuthProvider          any    `yaml:"auth-provider"`
		} `yaml:"user"`
	} `yaml:"users"`
}

func kubeClientFromKubeconfig(raw []byte, dir string, timeout time.Duration) (*kubeClient, error) {
	var cfg kubeconfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("k8s: parse kubeconfig: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, errors.New("k8s: kubeconfig declares no clusters")
	}
	// The current context is what `kubectl` would use, and choosing a
	// different one here would make the adapter act on a cluster the
	// operator's own tooling does not point at. A config with exactly one
	// context and no current-context is unambiguous and accepted; anything
	// else with no current-context is refused rather than guessed.
	ctxName := cfg.CurrentContext
	if ctxName == "" {
		if len(cfg.Contexts) == 1 {
			ctxName = cfg.Contexts[0].Name
		} else {
			return nil, fmt.Errorf("k8s: kubeconfig has %d contexts and no current-context; pick one rather than letting the adapter guess", len(cfg.Contexts))
		}
	}
	var clusterName, userName, namespace string
	found := false
	for _, c := range cfg.Contexts {
		if c.Name == ctxName {
			clusterName, userName, namespace = c.Context.Cluster, c.Context.User, c.Context.Namespace
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("k8s: kubeconfig has no context named %q", ctxName)
	}

	var cluster struct {
		server, caData, caFile, serverName string
		insecure                           bool
	}
	matched := false
	for _, c := range cfg.Clusters {
		if c.Name != clusterName {
			continue
		}
		cluster.server = c.Cluster.Server
		cluster.caData = c.Cluster.CertificateAuthorityData
		cluster.caFile = c.Cluster.CertificateAuthority
		cluster.serverName = c.Cluster.TLSServerName
		cluster.insecure = c.Cluster.InsecureSkipTLSVerify
		matched = true
		break
	}
	if !matched {
		return nil, fmt.Errorf("k8s: kubeconfig context %q references cluster %q, which is not defined", ctxName, clusterName)
	}
	if strings.TrimSpace(cluster.server) == "" {
		return nil, fmt.Errorf("k8s: kubeconfig cluster %q has no server", clusterName)
	}

	var token string
	var certData, keyData []byte
	if userName != "" {
		for _, u := range cfg.Users {
			if u.Name != userName {
				continue
			}
			if u.User.Exec != nil || u.User.AuthProvider != nil {
				// A kubeconfig that authenticates by running a command
				// cannot be honoured without running it, and this process
				// has no business executing a credential helper it was
				// handed by a config file.
				return nil, fmt.Errorf("k8s: kubeconfig user %q authenticates with an exec/auth-provider plugin; "+
					"run the plugin yourself and pass a token instead", userName)
			}
			token = u.User.Token
			if token == "" && u.User.TokenFile != "" {
				b, err := readRelative(dir, u.User.TokenFile)
				if err != nil {
					return nil, err
				}
				token = strings.TrimSpace(string(b))
			}
			if u.User.ClientCertificateData != "" {
				certData, _ = base64.StdEncoding.DecodeString(u.User.ClientCertificateData)
			} else if u.User.ClientCertificate != "" {
				b, err := readRelative(dir, u.User.ClientCertificate)
				if err != nil {
					return nil, err
				}
				certData = b
			}
			if u.User.ClientKeyData != "" {
				keyData, _ = base64.StdEncoding.DecodeString(u.User.ClientKeyData)
			} else if u.User.ClientKey != "" {
				b, err := readRelative(dir, u.User.ClientKey)
				if err != nil {
					return nil, err
				}
				keyData = b
			}
			break
		}
	}

	pool := x509.NewCertPool()
	switch {
	case cluster.caData != "":
		pem, err := base64.StdEncoding.DecodeString(cluster.caData)
		if err != nil {
			return nil, fmt.Errorf("k8s: certificate-authority-data is not base64: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("k8s: certificate-authority-data is not a PEM certificate")
		}
	case cluster.caFile != "":
		pem, err := readRelative(dir, cluster.caFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("k8s: certificate-authority %s is not a PEM certificate", cluster.caFile)
		}
	}

	tlsCfg := &tls.Config{ //nolint:gosec // InsecureSkipVerify is an explicit kubeconfig directive
		RootCAs:            pool,
		InsecureSkipVerify: cluster.insecure, //nolint:gosec
		ServerName:         cluster.serverName,
		MinVersion:         tls.VersionTLS12,
	}
	if len(certData) > 0 && len(keyData) > 0 {
		pair, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			return nil, fmt.Errorf("k8s: kubeconfig client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}
	if token == "" && len(tlsCfg.Certificates) == 0 {
		return nil, fmt.Errorf("k8s: kubeconfig user %q carries no token and no client certificate", userName)
	}
	return &kubeClient{
		base:      strings.TrimSuffix(cluster.server, "/"),
		token:     token,
		http:      &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
		timeout:   timeout,
		namespace: namespace,
	}, nil
}

// readRelative resolves a kubeconfig-referenced path.
//
// Kubeconfig paths are relative to the file that contains them, not to the
// process's working directory. Resolving against the working directory is
// the classic way a client silently reads the wrong CA — or fails on a
// cluster where the operator's `kubectl` works fine.
func readRelative(dir, p string) ([]byte, error) {
	if dir != "" && !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("k8s: read %s: %w", p, err)
	}
	return b, nil
}
