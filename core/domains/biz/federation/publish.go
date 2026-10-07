package federation

// publish.go — the half of the artifact store this root was missing.
//
// PublishedDistributor answers "is the store holding the bytes I signed, and
// where does a child fetch them". It never puts them there, and until now
// nothing in this package did either: the test that covers the store shape
// seeds the digest by hand and calls it "the deployment's publish step". That
// step is an operator uploading a file, which means the deployment shape that
// most multi-cluster installations actually have — a bucket, a CDN, anything
// the organisation already runs — was reachable only by a person with a
// command line, and the package's own comment said so in as many words
// ("with the root publishing into it by some means that is not this package").
//
// This file is that means. It is deliberately not a store client: there is no
// S3 SDK here and no credential handling, because the artefact store is an
// operator's own infrastructure and this process should not grow a second set
// of opinions about how to authenticate to it. What this owns is the part
// that is genuinely ours and genuinely easy to get wrong — the bytes.
//
// The bytes are the whole difficulty. A child compares a digest before it
// unpacks, so the archive that reaches the store has to be the exact archive
// the delivery already wrote, byte for byte. Repacking the tree produces a
// tarball that differs in a header field and a digest that matches nothing,
// and the failure surfaces at the far end as a transfer that cannot verify.
// So Publish reads the archive the distributor kept and never repacks, and
// Publish twice produces the same bytes twice.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// ArtifactSink is the write side of the store PublishedDistributor reads.
//
// Put returns the digest the store will serve for what it was handed, not the
// one it was asked to store. Those differ when a store transforms what it is
// given — a store that re-compresses, normalises, or appends metadata has
// stored something that is not the archive this root signed, and a publisher
// that reported the digest it *sent* would name a URL whose bytes no child
// could verify. Asking what the store will serve is what makes the next
// comparison a real one.
type ArtifactSink interface {
	Put(ctx context.Context, name string, data []byte) (digest string, err error)
}

// PublishedRecorder is the write side of the ledger PublishedDistributor reads.
//
// It is separate from PublishedLedger rather than folded into it because the
// read half is the safety property — a distributor that asked a store what it
// holds before naming a URL — while this one is convenience, and a deployment
// with a read-only manifest is a legitimate shape that a publishing root
// simply does not use. Publish uses it when it is available and does not
// require it.
type PublishedRecorder interface {
	RecordPublished(name, digest string) error
}

// PublishedPublisher puts the archive a delivery already wrote into the store
// that delivery is addressed at.
//
// It holds the *PublishedDistributor rather than a base URL of its own so that
// the publisher and the distributor cannot disagree about where the bytes
// live: the URL a publisher names and the URL a distributor hands a child are
// both computed by the same object, from the same base, by the same function.
// Two copies of that arithmetic would be two answers to one question.
type PublishedPublisher struct {
	published *PublishedDistributor
	sink      ArtifactSink
}

var _ = (*PublishedPublisher)(nil)

// NewPublishedPublisher builds a publisher for the store dist addresses.
//
// Both halves are required. A publisher with no distributor has nowhere to
// publish to and would have to invent an address; a publisher with no sink has
// nothing to write with, and reporting success from a publisher that never
// wrote anything is the exact failure PublishedDistributor exists to prevent.
func NewPublishedPublisher(published *PublishedDistributor, sink ArtifactSink) (*PublishedPublisher, error) {
	if published == nil {
		return nil, errors.New("federation: a publisher needs the distributor whose store it publishes to")
	}
	if sink == nil {
		return nil, errors.New("federation: a publisher needs a sink to write into")
	}
	return &PublishedPublisher{published: published, sink: sink}, nil
}

// Publish uploads one already-packed archive and reports where a child can
// fetch it.
//
// The order is the safety property and it has three steps, each of which can
// refuse. Read the bytes this root signed. Ask the store to hold them and
// compare what it says it will serve against what this root hashed. Then ask
// the store again, through the same ledger the distributor reads, whether it
// is serving them yet — because a store that accepted an upload and does not
// yet serve it is the "not yet" ErrNotPublished was written for, and answering
// anything else here would name a URL a child would fail to fetch.
//
// That last step is why Publish can fail on its own success. It is deliberate:
// a publish that returned a URL the store had not confirmed is a publish that
// reports work it has not seen land, which is the one behaviour the whole
// published path is arranged to prevent.
func (p *PublishedPublisher) Publish(ctx context.Context, b federation.Bundle) (tunnel.PolicySource, error) {
	name := archiveName(b)

	// The exact bytes the delivery wrote. Not repacked — see the file header.
	path := filepath.Join(p.published.local.Dir(), name)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return tunnel.PolicySource{}, fmt.Errorf(
				"federation: %s has not been packed yet, so there is nothing to publish; a "+
					"publish is the second half of a delivery, not a way to make one", name)
		}
		return tunnel.PolicySource{}, fmt.Errorf("federation: read the packed archive %s: %w", name, err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	stored, err := p.sink.Put(ctx, name, data)
	if err != nil {
		return tunnel.PolicySource{}, fmt.Errorf("federation: put %s into the artifact store: %w", name, err)
	}
	if !equalDigest(stored, digest) {
		// Same reading as PublishedDistributor's own mismatch: two
		// authorities disagree, a retry does not resolve it, and a delivery
		// loop that spins on a conflict never says why.
		return tunnel.PolicySource{}, fmt.Errorf("%w: the store says %s will be served with digest "+
			"%s, and this root hashed %s", ErrPublishedMismatch, name, stored, digest)
	}

	// Record before confirming. The confirmation reads back through the same
	// ledger the distributor uses, so a store that serves the bytes and a
	// ledger that forgot them are one state, not two: without this the
	// publish would report not-yet for a tree it had just successfully put,
	// and every delivery would repeat the upload forever.
	if recorder, ok := p.published.ledger.(PublishedRecorder); ok {
		if err := recorder.RecordPublished(name, digest); err != nil {
			return tunnel.PolicySource{}, fmt.Errorf("federation: record %s as published: %w", name, err)
		}
	}

	published, ok := p.published.ledger.PublishedDigest(name)
	switch {
	case !ok:
		return tunnel.PolicySource{}, fmt.Errorf("%w: the store took %s but does not serve it yet",
			ErrNotPublished, name)
	case !equalDigest(published, digest):
		return tunnel.PolicySource{}, fmt.Errorf("%w: the store serves %s with digest %s, and this "+
			"root signed %s", ErrPublishedMismatch, name, published, digest)
	}

	return tunnel.PolicySource{
		URL:           p.published.urlFor(name),
		ArchiveSHA256: digest,
	}, nil
}

// equalDigest compares two hex digests without caring how either was cased.
// The ledger's contents come from whatever tool wrote it, and a store that
// reports an uppercase digest is reporting the same bytes. Trimming first is
// the other half: the same is true of a digest that arrived with a trailing
// newline from a shell pipeline, and refusing it would turn a formatting
// difference into a conflict that never resolves.
func equalDigest(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// HTTPSink is an ArtifactStore that speaks plain HTTP.
//
// It exists because "publishing into the store by some means that is not this
// package" turned out to mean "by hand, with a command line", and the object
// store most deployments already run is reachable over HTTP whatever else it
// is. Deliberately not an S3 or OSS client: this process should not grow a
// second opinion about how to authenticate to infrastructure the operator
// owns, so the credential is one bearer token the deployment supplies, and
// the base URL is one it chose.
//
// The read-back is the part that is not optional. Put reports the digest of
// what the store will serve, and the only honest way to know that is to fetch
// the object back and hash it — a store behind a CDN, a compressing proxy or
// a synchroniser may all serve something other than what it was handed, and
// the archive's whole contract is that a child can verify it before unpacking.
type HTTPSink struct {
	base   string
	token  string
	client *http.Client
}

var _ ArtifactSink = (*HTTPSink)(nil)

// NewHTTPSink builds a sink against an artifact base URL.
//
// The token is required rather than defaulted for the reason
// NewPublishedDistributor refuses a scheme-less base: putting signed policy
// trees somewhere unauthenticated is not a deployment shape, it is a leak with
// a URL in it.
func NewHTTPSink(base, token string, client *http.Client) (*HTTPSink, error) {
	if strings.TrimSpace(base) == "" {
		return nil, errors.New("federation: an http sink needs an artifact base URL")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("federation: an http sink needs a token; publishing signed policy " +
			"trees to a store anyone can write is not a deployment shape")
	}
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("federation: parse the artifact base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("federation: the artifact base URL scheme %q is not http or https", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("federation: the artifact base URL %q names no host", base)
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &HTTPSink{base: trimmed, token: strings.TrimSpace(token), client: client}, nil
}

// Put uploads an archive and reports the digest of what the store serves back.
func (s *HTTPSink) Put(ctx context.Context, name string, data []byte) (string, error) {
	target := s.base + "/" + url.PathEscape(name)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("federation: build the upload for %s: %w", name, err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/gzip")
	req.ContentLength = int64(len(data))

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("federation: upload %s: %w", name, err)
	}
	defer drain(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("federation: the artifact store refused %s: %s", name, resp.Status)
	}

	// Read it back rather than reporting what was sent. A store that
	// transforms what it stores must be caught here, by the caller, as a
	// conflict — not discovered by a child three networks away.
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("federation: build the read-back for %s: %w", name, err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err = s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("federation: read %s back from the artifact store: %w", name, err)
	}
	defer drain(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("federation: the artifact store took %s but will not serve it: %s",
			name, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil {
		return "", fmt.Errorf("federation: read back %s: %w", name, err)
	}
	if len(body) > maxArtifactBytes {
		return "", fmt.Errorf("federation: the artifact store serves %s at %d bytes, past the %d cap "+
			"this package accepts", name, len(body), maxArtifactBytes)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// maxArtifactBytes is the ceiling on a read-back. It matches the cap the
// child enforces on an inbound tree, so a store cannot hand back something the
// receiving end would refuse anyway.
const maxArtifactBytes = 4 << 20

// drain closes a response and reads the remainder so the connection can be
// reused. Without it a publish loop leaks one connection per attempt.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// PublishingDistributor delivers to a store by filling it.
//
// The published distributor refuses to name a URL for a tree the store does
// not hold, which is right and useless on its own: without a writer, the only
// way out of ErrNotPublished is a person. This type closes that loop — deliver,
// and on the not-yet, publish the bytes this root just packed and ask again.
//
// It republishes rather than repacks, and only after a delivery has written an
// archive, which is the same discipline every other half of this package keeps.
type PublishingDistributor struct {
	published *PublishedDistributor
	publisher *PublishedPublisher
}

var (
	_ Distributor = (*PublishingDistributor)(nil)
	_ Redeliverer = (*PublishingDistributor)(nil)
)

// NewPublishingDistributor wraps a published distributor with the writer that
// fills the store it addresses. Both are required: one without the other is
// either a distributor nobody fills or a writer whose bytes nobody fetches.
func NewPublishingDistributor(published *PublishedDistributor, publisher *PublishedPublisher) (*PublishingDistributor, error) {
	if published == nil {
		return nil, errors.New("federation: a publishing distributor needs the published distributor it fills a store for")
	}
	if publisher == nil {
		return nil, errors.New("federation: a publishing distributor needs a publisher; without one " +
			"this is the published distributor with an extra word in its name")
	}
	return &PublishingDistributor{published: published, publisher: publisher}, nil
}

// Distribute packs, delivers, and fills the store when the store is empty.
func (d *PublishingDistributor) Distribute(ctx context.Context, b federation.Bundle, stagedRoot string) (tunnel.PolicySource, error) {
	src, err := d.published.Distribute(ctx, b, stagedRoot)
	if err == nil {
		return src, nil
	}
	if !errors.Is(err, ErrNotPublished) {
		// A conflict is not a not-yet. Filling the store again would publish
		// over bytes a child may be fetching, and the second attempt would
		// produce the same conflict.
		return tunnel.PolicySource{}, err
	}
	if _, err := d.publisher.Publish(ctx, b); err != nil {
		return tunnel.PolicySource{}, err
	}
	return d.published.SourceFor(b)
}

// SourceFor is the redelivery path and is deliberately unchanged: a redelivery
// reads the archive rather than repacking, and this type has nothing to add to
// it beyond the first delivery having filled the store.
func (d *PublishingDistributor) SourceFor(b federation.Bundle) (tunnel.PolicySource, error) {
	return d.published.SourceFor(b)
}

// Dir reports where the exact bytes are kept locally.
//
// It is here because both shapes of distributor answer it, and a caller that
// had to branch on which one it got would be reading a type it should not have
// to know about. A publishing distributor keeps its bytes exactly where the
// published one does — that is the whole reason it can republish without
// repacking.
func (d *PublishingDistributor) Dir() string { return d.published.local.Dir() }
