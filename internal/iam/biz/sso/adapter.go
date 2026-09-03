// Package sso implements enterprise SSO for the manager. The
// public surface is the IDPAdapter interface (which the SAML/OIDC
// implementations satisfy) and SSOService (the biz layer that
// orchestrates StartLogin / HandleCallback / JIT provisioning).
//
// Why the package is split like this:
//
//   - IDPAdapter abstracts away the protocol so the rest of the
//     stack (SSOService, HTTP handlers, audit) doesn't care whether
//     the IdP speaks OIDC or SAML. P2 will add SAMLAdapter; the
//     rest of the system stays unchanged.
//   - SSOService owns the policy decisions: state lifecycle
//     (CSRF defense), JIT provisioning, role mapping. Adapter
//     implementations are dumb — they sign requests and parse
//     responses, nothing else.
package sso

import (
	"context"
	"errors"
)

// Protocol type constants — match iamodel.SSOProviderType* so the
// store layer and the biz layer agree.
const (
	ProviderTypeOIDC = "oidc"
	ProviderTypeSAML = "saml"
)

// IDPAdapter abstracts the protocol differences between OIDC and
// SAML. The interface is narrow on purpose — anything policy-level
// (which scopes to request, which claim to use as the role source,
// whether to JIT-provision) lives in SSOService, not here.
type IDPAdapter interface {
	// Type returns the protocol identifier — "oidc" or "saml".
	// Used by SSOService to route a config row to the right adapter.
	Type() string

	// GetAuthURL constructs the IdP login page URL with PKCE params
	// for OIDC (no-op for SAML — the SAML binding handles its own
	// artifact/redirect).
	GetAuthURL(ctx context.Context, state, codeChallenge string) (string, error)

	// ExchangeCode exchanges the authorization code (OIDC) /
	// assertion consumer service URL result (SAML) for tokens.
	// For OIDC this is /token; for SAML this returns the parsed
	// assertion as a TokenSet-like shape.
	ExchangeCode(ctx context.Context, code, codeVerifier string) (*TokenSet, error)

	// VerifyIDToken validates the ID Token signature + claims
	// (aud, exp, iss) using the IdP's JWKS. OIDC-only — SAML uses
	// VerifyAssertion instead.
	VerifyIDToken(ctx context.Context, rawIDToken string) (*Claims, error)

	// RefreshToken trades a refresh token for a new token pair.
	// OIDC-only; SAML doesn't have an equivalent — return
	// ErrNotImplemented.
	RefreshToken(ctx context.Context, refreshToken string) (*TokenSet, error)

	// FetchUserInfo is the optional userinfo call (OIDC). Most
	// implementations rely on ID Token claims and skip this; the
	// method exists for IdPs that put user attributes elsewhere.
	FetchUserInfo(ctx context.Context, accessToken string) (*Claims, error)
}

// TokenSet is the IdP-issued token triplet. RefreshToken may be
// empty (some IdPs don't issue one for confidential clients with
// rotation disabled).
type TokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
}

// Claims is the normalized user claim bundle across protocols. The
// Raw field keeps the original payload so future features (e.g.
// per-org custom claim extraction) don't need to refetch.
//
// Groups is the role-mapping input: SSOService.MapRole iterates it
// against the configured groups→role table.
type Claims struct {
	Subject       string         // OIDC sub / SAML NameID
	Email         string         // Required
	EmailVerified bool           // If the IdP marks unverified emails
	Name          string         // Display name
	Picture       string         // Avatar URL (optional)
	Groups        []string       // For role mapping
	Raw           map[string]any // Original claims (audit / debug)
}

// ErrNotImplemented is returned by adapter methods that don't apply
// to the chosen protocol (e.g. SAMLAdapter.RefreshToken).
var ErrNotImplemented = errors.New("sso: not implemented for this protocol")
