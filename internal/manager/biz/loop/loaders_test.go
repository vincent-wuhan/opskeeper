package loop

import (
	"context"
	"errors"
	"testing"
)

type noopRC struct{}

func (noopRC) LoadRootCause(_ context.Context, _, _ string) (*RootCauseJSON, error) {
	return nil, nil
}

type noopCS struct{}

func (noopCS) LoadCritique(_ context.Context, _, _ string) (*CritiqueScore, error) {
	return nil, nil
}

type noopVD struct{}

func (noopVD) LoadVerifiedDelta(_ context.Context, _, _ string) (*VerifiedDelta, error) {
	return nil, nil
}

type noopTL struct{}

func (noopTL) LoadTimeline(_ context.Context, _, _ string) ([]TimelineEvent, error) {
	return nil, nil
}

type errRC struct{}

func (errRC) LoadRootCause(_ context.Context, _, _ string) (*RootCauseJSON, error) {
	return nil, errors.New("db down")
}

func TestLoaders_Validate(t *testing.T) {
	cases := []struct {
		name    string
		loaders Loaders
		wantErr bool
	}{
		{
			name:    "all nil",
			loaders: Loaders{},
			wantErr: true,
		},
		{
			name:    "all set",
			loaders: Loaders{RootCause: noopRC{}, Critique: noopCS{}, Verified: noopVD{}, Timeline: noopTL{}},
			wantErr: false,
		},
		{
			name:    "missing rootcause",
			loaders: Loaders{Critique: noopCS{}, Verified: noopVD{}, Timeline: noopTL{}},
			wantErr: true,
		},
		{
			name:    "missing critique",
			loaders: Loaders{RootCause: noopRC{}, Verified: noopVD{}, Timeline: noopTL{}},
			wantErr: true,
		},
		{
			name:    "missing verified",
			loaders: Loaders{RootCause: noopRC{}, Critique: noopCS{}, Timeline: noopTL{}},
			wantErr: true,
		},
		{
			name:    "missing timeline",
			loaders: Loaders{RootCause: noopRC{}, Critique: noopCS{}, Verified: noopVD{}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.loaders.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestLoaders_InterfacesAreSatisfied is a compile-time assertion
// (via the test runner) that our stubs satisfy the public interfaces.
// If a future PR changes the interface, this test breaks loudly.
func TestLoaders_InterfacesAreSatisfied(t *testing.T) {
	var (
		_ RootCauseLoader     = noopRC{}
		_ CritiqueLoader      = noopCS{}
		_ VerifiedDeltaLoader = noopVD{}
		_ TimelineLoader      = noopTL{}
	)
}

func TestLoaderMissingError_Message(t *testing.T) {
	err := errLoaderMissing("Critique")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	msg := err.Error()
	if msg != "loop: required loader missing: Critique" {
		t.Errorf("unexpected error message: %q", msg)
	}
}
