package ports

import "testing"

// Billed is the number a ledger charges, and the rule is narrow on purpose:
// the provider's own total when there is one, the sum of everything the
// transcript stores when there isn't. Recomputing a reported total would
// quietly change the bill.

// The provider's total wins even when the stored components would sum to
// something else. Reasoning models bill reasoning tokens the transcript does
// not name in either input or output, so the sum under-reports and the cap it
// drives would be a cap on the wrong number.
func TestTheProvidersReportedTotalIsTheBill(t *testing.T) {
	usage := TranscriptUsage{InputTokens: 10, OutputTokens: 5, ReportedTotal: 5_000}
	if got := usage.Billed(); got != 5_000 {
		t.Errorf("billed = %d, want the provider's 5000", got)
	}
}

// A provider that reported nothing is still tokens that were bought, so the
// fallback sums every component the ledger stores — including cache, which is
// billed at its own rate and is the component a naive sum drops first.
func TestASilentProviderIsChargedEveryComponentItDidReport(t *testing.T) {
	usage := TranscriptUsage{
		InputTokens: 10, OutputTokens: 5, CacheReadTokens: 100, CacheWriteTokens: 5,
	}
	if got := usage.Billed(); got != 120 {
		t.Errorf("billed = %d, want 120", got)
	}
}

// A turn that used nothing costs nothing, and must not be mistaken for a
// provider that reported a total of zero.
func TestNothingUsedIsNothingCharged(t *testing.T) {
	if got := (TranscriptUsage{}).Billed(); got != 0 {
		t.Errorf("billed = %d, want 0", got)
	}
}
