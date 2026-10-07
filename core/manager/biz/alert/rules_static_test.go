// This file used to be rules.go, in the middle of the production provider.
//
// StaticRulesProvider is a test fixture: a RulesProvider that returns a
// fixed list so an evaluator can be exercised without a database. Production
// boots CachedRulesProvider (cmd/opskeeper/main.go:1180) and nothing else ever
// constructed this one -- `NewStaticRulesProvider` had three callers, all of
// them _test.go files in this same package.
//
// It was shipped in production for the ordinary reason: a test helper written
// next to the thing it fakes is the path of least resistance, and nothing
// objected until the reachability walk counted it. That walk is right to
// count it. A RulesProvider the running binary cannot build is a
// RulesProvider-shaped hole in the package, and the eight methods it declares
// are the reason a reader has to check whether the static one is real.
//
// Moving it here costs the three test files nothing: they are all
// `package alert`, so the names resolve the same way from a _test.go file in
// the same directory as they did from rules.go.
//
// The three setters that no test referenced at all -- WithLogVolumeRules,
// WithTraceLatencyRules and WithTraceErrorRateRules -- are gone rather than
// moved. The snapshot fields they filled are still populated in production by
// CachedRulesProvider, so the log-volume and trace evaluators keep working;
// what was missing was a way to hand them a fixed list, and nobody wanted one.
// The getters stay in rules.go, which is where the interface requires them.

package alert

// StaticRulesProvider serves a fixed snapshot. Used in tests and embedded
// deployments without a DB.
type StaticRulesProvider struct {
	snap rulesSnapshot
}

// NewStaticRulesProvider builds a provider from literal rule lists.
// Pass any combination of WithMetricRawRules / WithMetricAnomalyRules /
// WithMetricForecastRules / WithMetricBurnRateRules.
func NewStaticRulesProvider(opts ...StaticOption) *StaticRulesProvider {
	s := &StaticRulesProvider{}
	for _, opt := range opts {
		opt(&s.snap)
	}
	return s
}

// StaticOption is a functional option for NewStaticRulesProvider — keeps the
// constructor backward-compatible while letting tests inject other kinds.
type StaticOption func(*rulesSnapshot)

// WithMetricRawRules attaches metric_raw kind rules to the static provider.
func WithMetricRawRules(rs []MetricRawRule) StaticOption {
	return func(s *rulesSnapshot) { s.metricRaw = append([]MetricRawRule(nil), rs...) }
}

// WithMetricAnomalyRules attaches metric_anomaly rules to the static provider.
func WithMetricAnomalyRules(rs []MetricAnomalyRule) StaticOption {
	return func(s *rulesSnapshot) { s.metricAnomaly = append([]MetricAnomalyRule(nil), rs...) }
}

// WithMetricForecastRules attaches metric_forecast rules to the static provider.
func WithMetricForecastRules(rs []MetricForecastRule) StaticOption {
	return func(s *rulesSnapshot) { s.metricForecast = append([]MetricForecastRule(nil), rs...) }
}

// WithMetricBurnRateRules attaches metric_burn_rate rules to the static provider.
func WithMetricBurnRateRules(rs []MetricBurnRateRule) StaticOption {
	return func(s *rulesSnapshot) { s.metricBurnRate = append([]MetricBurnRateRule(nil), rs...) }
}

func (s *StaticRulesProvider) MetricRawRules() []MetricRawRule         { return s.snap.metricRaw }
func (s *StaticRulesProvider) MetricAnomalyRules() []MetricAnomalyRule { return s.snap.metricAnomaly }
func (s *StaticRulesProvider) MetricForecastRules() []MetricForecastRule {
	return s.snap.metricForecast
}
func (s *StaticRulesProvider) MetricBurnRateRules() []MetricBurnRateRule {
	return s.snap.metricBurnRate
}
func (s *StaticRulesProvider) LogMatchRules() []LogMatchRule   { return s.snap.logMatch }
func (s *StaticRulesProvider) LogVolumeRules() []LogVolumeRule { return s.snap.logVolume }
func (s *StaticRulesProvider) TraceLatencyRules() []TraceLatencyRule {
	return s.snap.traceLatency
}
func (s *StaticRulesProvider) TraceErrorRateRules() []TraceErrorRateRule {
	return s.snap.traceErrorRate
}

// WithLogMatchRules attaches a Phase-B kind to the static provider, so a
// test can exercise that evaluator without a database.
func WithLogMatchRules(rs []LogMatchRule) StaticOption {
	return func(s *rulesSnapshot) { s.logMatch = append([]LogMatchRule(nil), rs...) }
}
