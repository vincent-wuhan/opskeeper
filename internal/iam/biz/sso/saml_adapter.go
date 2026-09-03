package sso

import "context"

// SAMLAdapter is the placeholder for SAML 2.0 support. The full
// implementation lands in a follow-up change (i1-sso P2 per the
// design doc); for now every method returns ErrNotImplemented so
// the registry can include a SAML row that surfaces a "coming
// soon" error instead of silently dropping it.
//
// Why we ship the stub at all instead of leaving SAML out entirely:
//
//   - The (ProviderType, Adapter) dispatch in SSOService.NewAdapter
//     needs a concrete type for each protocol so the registry
//     validates config rows at startup, not at user-login time.
//   - A misconfigured SAML row (ProviderType=saml but adapter nil)
//     currently fails loud at the first login attempt; the stub
//     makes it fail loud at server start with a clear message.
type SAMLAdapter struct{}

// Compile-time interface check.
var _ IDPAdapter = (*SAMLAdapter)(nil)

// NewSAMLAdapter returns a stub SAML adapter. The real constructor
// will take a *SAMLConfig (EntityID, ACS URL, IdP metadata) once
// P2 lands.
func NewSAMLAdapter() *SAMLAdapter { return &SAMLAdapter{} }

func (SAMLAdapter) Type() string { return ProviderTypeSAML }

func (SAMLAdapter) GetAuthURL(_ context.Context, _, _ string) (string, error) {
	return "", ErrNotImplemented
}

func (SAMLAdapter) ExchangeCode(_ context.Context, _, _ string) (*TokenSet, error) {
	return nil, ErrNotImplemented
}

func (SAMLAdapter) VerifyIDToken(_ context.Context, _ string) (*Claims, error) {
	return nil, ErrNotImplemented
}

func (SAMLAdapter) RefreshToken(_ context.Context, _ string) (*TokenSet, error) {
	return nil, ErrNotImplemented
}

func (SAMLAdapter) FetchUserInfo(_ context.Context, _ string) (*Claims, error) {
	return nil, ErrNotImplemented
}
