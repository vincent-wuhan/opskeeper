// Package fixtures holds the response bodies both halves of the contract test
// read.
//
// It is a package rather than a directory of loose files so that the two halves
// cannot drift onto different sets: a contract test that loads "the same
// fixtures" through two hand-maintained lists is two lists, and the moment they
// differ the test stops comparing anything.
package fixtures

import "net/url"

// Case is one query issued to both backends, with the response each is
// expected to normalise to.
//
// The response body is a fixture rather than something a fake backend invents
// per client, so both clients decode byte-identical input. A fake that shaped
// each client's response for it could hand each a body that happened to suit
// it, and the test would still pass.
type Case struct {
	// Name identifies the case in test output.
	Name string
	// Expr is the PromQL expression, sent verbatim to both backends.
	Expr string
	// Path is the API path the query goes to.
	Path string
	// RawQuery is the query string both clients are expected to send,
	// including the fixed start/end/step of a range case.
	RawQuery string
	// Response is the exact body the fake backend returns.
	Response string
	// ResultType is the resultType both normalised results must carry.
	ResultType string
}

// instant builds a Case for an instant query at a fixed time.
func instant(name, expr, at, response, resultType string) Case {
	return Case{
		Name: name, Expr: expr, Path: "/api/v1/query",
		RawQuery: "query=" + queryEscape(expr) + "&time=" + at,
		Response: response, ResultType: resultType,
	}
}

// rangeCase builds a Case for a range query over a fixed window.
func rangeCase(name, expr, start, end, step, response, resultType string) Case {
	return Case{
		Name: name, Expr: expr, Path: "/api/v1/query_range",
		RawQuery: "end=" + end + "&query=" + queryEscape(expr) +
			"&start=" + start + "&step=" + step,
		Response: response, ResultType: resultType,
	}
}

// queryEscape is the encoding url.Values.Encode applies to a value.
//
// It delegates to net/url rather than re-deriving the rules: a hand-rolled
// escaper here would encode spaces as %20 where Go encodes them as +, the test
// would fail on a difference between the fixture and both clients rather than
// between the two clients, and the first person to hit it would have to work
// out which of the three was right. Using the same function the clients use
// leaves exactly one thing to disagree about.
func queryEscape(s string) string { return url.QueryEscape(s) }

// Cases is the fixture set. Both backends answer every one of them, and the
// contract test asserts they produced the same request and the same normalised
// result.
//
// The set covers the three result shapes Prometheus can return (vector, matrix,
// scalar): a client that normalises two of the three correctly is not a client
// that normalises correctly.
func Cases() []Case {
	return []Case{
		instant("instant-vector", `up{job="api",instance="api-1:8080"}`, "1772366400",
			`{"status":"success","data":{"resultType":"vector","result":[`+
				`{"metric":{"__name__":"up","instance":"api-1:8080","job":"api"},`+
				`"value":[1772366400,"1"]}]}}`,
			"vector"),

		instant("instant-empty", `up{job="does-not-exist"}`, "1772366400",
			`{"status":"success","data":{"resultType":"vector","result":[]}}`,
			"vector"),

		instant("instant-scalar", `1 + 1`, "1772366400",
			`{"status":"success","data":{"resultType":"scalar","result":[1772366400,"2"]}}`,
			"scalar"),

		rangeCase("range-matrix", `rate(http_requests_total{job="api"}[5m])`,
			"1772366400", "1772368200", "30",
			`{"status":"success","data":{"resultType":"matrix","result":[`+
				`{"metric":{"__name__":"rate(http_requests_total)","job":"api"},`+
				`"values":[[1772366400,"12.5"],[1772366430,"13.25"],[1772366460,"11.75"]]}]}}`,
			"matrix"),

		rangeCase("range-by-label",
			`sum by (le) (rate(http_request_duration_seconds_bucket[5m]))`,
			"1772366400", "1772368200", "60",
			`{"status":"success","data":{"resultType":"matrix","result":[`+
				`{"metric":{"le":"0.005"},"values":[[1772366400,"1"],[1772366460,"2"]]},`+
				`{"metric":{"le":"+Inf"},"values":[[1772366400,"9"],[1772366460,"8"]]}]}}`,
			"matrix"),
	}
}
