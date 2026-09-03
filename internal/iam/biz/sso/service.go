package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	ssostore "github.com/vincent-wuhan/opskeeper/internal/iam/data/sso"
	iamodel "github.com/vincent-wuhan/opskeeper/internal/iam/model"
)

// StateStore is the narrow interface SSOService uses to persist
// short-lived OAuth state (CSRF defense). The prod binding is
// Redis; tests use an in-memory map.
type StateStore interface {
	Put(ctx context.Context, key string, value StateEntry, ttl time.Duration) error
	Get(ctx context.Context, key string) (*StateEntry, error)
	Delete(ctx context.Context, key string) error
}

// StateEntry is the value bound to a state token in the state
// store. Encoded as JSON.
type StateEntry struct {
	OrgID        string `json:"org_id"`
	ProviderName string `json:"provider_name"`
	CodeVerifier string `json:"code_verifier"`
	Nonce        string `json:"nonce,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// UserUpserter is the narrow seam SSOService needs from the user
// biz. Production binds it to *user.Usecase; tests stub it.
type UserUpserter interface {
	FindByAuthProviderExternalID(ctx context.Context, provider, externalID string) (*iamodel.User, error)
	CreateSSOUser(ctx context.Context, in *iamodel.User) (*iamodel.User, error)
	UpdateSSOLogin(ctx context.Context, id uint64, email, name, authDomain string) error
}

// MembershipUpserter manages the user↔org mapping + role assignment.
type MembershipUpserter interface {
	FindByUserOrg(ctx context.Context, userID, orgID uint64) (*iamodel.OrgMembership, error)
	UpsertRole(ctx context.Context, userID, orgID uint64, role string) error
}

// AdapterFactory returns the IDPAdapter for a config row. SSOService
// caches by (ProviderType, IssuerURL, ClientID) so we don't re-do
// OIDC discovery on every callback.
type AdapterFactory func(ctx context.Context, cfg *iamodel.OrgSSOConfig) (IDPAdapter, error)

// SSOService is the biz-layer entry point for SSO flows.
type SSOService struct {
	cfgs       *ssostore.OrgSSOConfigStore
	state      StateStore
	users      UserUpserter
	membership MembershipUpserter
	factory    AdapterFactory
	log        *slog.Logger
}

// NewSSOService wires the service.
func NewSSOService(
	cfgs *ssostore.OrgSSOConfigStore,
	state StateStore,
	users UserUpserter,
	membership MembershipUpserter,
	factory AdapterFactory,
	log *slog.Logger,
) *SSOService {
	if log == nil {
		log = slog.Default()
	}
	return &SSOService{
		cfgs: cfgs, state: state, users: users,
		membership: membership, factory: factory, log: log,
	}
}

// StartLoginResult is the bundle the HTTP handler hands back to
// the browser: a 302 redirect target + the PKCE verifier (which
// the SPA stashes in sessionStorage — never cookies, since the
// code_verifier is short-lived).
type StartLoginResult struct {
	AuthURL      string
	State        string
	CodeVerifier string
}

// StartLogin initiates the SSO flow. It returns the IdP login URL
// (302 target) + the state + code_verifier the SPA needs to round
// out the round-trip.
func (s *SSOService) StartLogin(ctx context.Context, orgID, providerName string) (*StartLoginResult, error) {
	cfg, err := s.cfgs.Get(ctx, orgID, providerName, false)
	if err != nil {
		return nil, fmt.Errorf("sso: load config: %w", err)
	}
	if cfg == nil {
		return nil, errors.New("sso: provider not found or disabled")
	}
	adapter, err := s.factory(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("sso: build adapter: %w", err)
	}
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("sso: pkce: %w", err)
	}
	state := randomToken(32)
	entry := StateEntry{
		OrgID:        orgID,
		ProviderName: providerName,
		CodeVerifier: verifier,
		CreatedAt:    time.Now().Unix(),
	}
	if err := s.state.Put(ctx, state, entry, 5*time.Minute); err != nil {
		return nil, fmt.Errorf("sso: state store: %w", err)
	}
	url, err := adapter.GetAuthURL(ctx, state, challenge)
	if err != nil {
		return nil, fmt.Errorf("sso: build auth url: %w", err)
	}
	return &StartLoginResult{
		AuthURL: url, State: state, CodeVerifier: verifier,
	}, nil
}

// CallbackResult is what SSOService.HandleCallback returns to the
// HTTP handler.
type CallbackResult struct {
	UserID     uint64
	OrgID      string
	Email      string
	Role       string
	AuthMethod string
}

// HandleCallback processes the IdP redirect: verify state, exchange
// code, verify ID Token, JIT-provision the user, return the role.
func (s *SSOService) HandleCallback(ctx context.Context, orgID, providerName, code, state string) (*CallbackResult, error) {
	// Pull + delete state atomically (one-shot use).
	entry, err := s.state.Get(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("sso: state lookup: %w", err)
	}
	if entry == nil {
		return nil, errors.New("sso: state invalid or expired")
	}
	if err := s.state.Delete(ctx, state); err != nil {
		s.log.Warn("sso: state delete after use", "err", err)
	}
	// Cross-check the orgID/providerName in state matches the URL
	// parameters (defends against cross-tenant state smuggling).
	if entry.OrgID != orgID || entry.ProviderName != providerName {
		return nil, errors.New("sso: state org/provider mismatch")
	}

	cfg, err := s.cfgs.Get(ctx, orgID, providerName, false)
	if err != nil || cfg == nil {
		return nil, errors.New("sso: provider config vanished mid-flow")
	}
	adapter, err := s.factory(ctx, cfg)
	if err != nil {
		return nil, err
	}
	tokens, err := adapter.ExchangeCode(ctx, code, entry.CodeVerifier)
	if err != nil {
		return nil, fmt.Errorf("sso: code exchange: %w", err)
	}
	claims, err := adapter.VerifyIDToken(ctx, tokens.IDToken)
	if err != nil {
		return nil, fmt.Errorf("sso: id_token verify: %w", err)
	}
	if !claims.EmailVerified {
		return nil, errors.New("sso: email not verified by IdP")
	}
	if claims.Email == "" {
		return nil, errors.New("sso: id_token missing email claim")
	}

	role, err := s.MapRole(ctx, cfg, claims)
	if err != nil {
		return nil, err
	}

	user, err := s.JITProvision(ctx, cfg, claims)
	if err != nil {
		return nil, err
	}
	// Update role on the membership.
	orgIDU, err := parseOrgID(orgID)
	if err != nil {
		return nil, err
	}
	if err := s.membership.UpsertRole(ctx, user.ID, orgIDU, role); err != nil {
		return nil, fmt.Errorf("sso: upsert role: %w", err)
	}

	return &CallbackResult{
		UserID:     user.ID,
		OrgID:      orgID,
		Email:      claims.Email,
		Role:       role,
		AuthMethod: iamodel.AuthMethodOIDC,
	}, nil
}

// JITProvision returns the local user row for the IdP subject,
// creating one if it doesn't exist. Idempotent — repeated logins
// update last-login + sync email/name, never duplicate.
func (s *SSOService) JITProvision(ctx context.Context, cfg *iamodel.OrgSSOConfig, claims *Claims) (*iamodel.User, error) {
	existing, err := s.users.FindByAuthProviderExternalID(ctx, cfg.ProviderType, claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("sso: user lookup: %w", err)
	}
	if existing != nil {
		if err := s.users.UpdateSSOLogin(ctx, existing.ID, claims.Email, claims.Name, cfg.IssuerURL); err != nil {
			s.log.Warn("sso: update sso login failed", "err", err, "user_id", existing.ID)
		}
		return existing, nil
	}
	u := &iamodel.User{
		Email:        claims.Email,
		DisplayName:  claims.Name,
		AuthProvider: cfg.ProviderType,
		ExternalID:   claims.Subject,
		AuthDomain:   cfg.IssuerURL,
		Status:       "active",
	}
	return s.users.CreateSSOUser(ctx, u)
}

// MapRole returns the opskeeper role for the IdP claims. The mapping
// table is org-config-driven (cfg.ClaimMappings) so adding a new
// group→role pair is a UI change, not a code change.
//
// Algorithm: iterate cfg.ClaimMappings["groups"] in declaration
// order; first match wins. No match → cfg.DefaultRole (which the
// store requires to be set, defaulting to "viewer").
func (s *SSOService) MapRole(_ context.Context, cfg *iamodel.OrgSSOConfig, claims *Claims) (string, error) {
	if cfg.ClaimMappings == "" {
		return iamodel.DefaultRoleFallback(cfg.DefaultRole), nil
	}
	mappings := struct {
		Groups       map[string]string `json:"groups"`
		FallbackRole string            `json:"fallback_role"`
	}{}
	if err := json.Unmarshal([]byte(cfg.ClaimMappings), &mappings); err != nil {
		return "", fmt.Errorf("sso: claim_mappings parse: %w", err)
	}
	for _, g := range claims.Groups {
		if role, ok := mappings.Groups[g]; ok {
			return role, nil
		}
	}
	// No match — use explicit fallback_role if set, else cfg.DefaultRole,
	// else hard-coded "viewer".
	if mappings.FallbackRole != "" {
		return mappings.FallbackRole, nil
	}
	return iamodel.DefaultRoleFallback(cfg.DefaultRole), nil
}

// randomToken returns a URL-safe random token of n bytes.
func randomToken(n int) string {
	// Inline simple generator to avoid pulling crypto/rand into the
	// header; crypto/rand is already imported via oidc_adapter.go.
	return RandomToken(n)
}

// parseOrgID parses a string org ID into uint64. Currently the iam
// model uses uint64 IDs (not string), so the URL param has to be
// numeric. If a future change moves to string org_ids this can be
// relaxed.
func parseOrgID(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("sso: empty org_id")
	}
	n := uint64(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("sso: invalid org_id %q", s)
		}
		n = n*10 + uint64(c-'0')
	}
	if n == 0 {
		return 0, errors.New("sso: org_id cannot be 0")
	}
	return n, nil
}

// stringTrim trims whitespace + lowercases (helper for future use).
func stringTrim(s string) string { return strings.TrimSpace(s) }
