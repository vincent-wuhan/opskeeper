package vmclient

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinels a caller can match with errors.Is, one per class of upstream
// failure. They exist because "the query returned nothing" and "we could not
// ask" have to be told apart by whatever renders this for an operator, and the
// difference is in the error, not in an HTTP status the caller never sees.
//
// A timeout is deliberately not a sentinel: net/http already wraps it in
// context.DeadlineExceeded, and errors.Is against that is the idiomatic answer
// and does not need a second name.
var (
	// ErrUnauthorized is a 401: the credential was missing, wrong or
	// expired. The backend rejected us, not the query.
	ErrUnauthorized = errors.New(ErrVM + "unauthorized")

	// ErrForbidden is a 403: the credential is valid and the backend is
	// still refusing this call.
	ErrForbidden = errors.New(ErrVM + "forbidden")

	// ErrNotFound is a 404: the API root points at something that is not a
	// Prometheus-compatible API, which is almost always a URL typo.
	ErrNotFound = errors.New(ErrVM + "not found")

	// ErrBadRequest is a 4xx the backend reached with a parsed body — a
	// syntactically or semantically bad expression. This one is the
	// backend's answer rather than a transport failure, which is why it is
	// unwrapped and decoded by the caller.
	ErrBadRequest = errors.New(ErrVM + "bad request")

	// ErrServer is a 5xx: the backend is up enough to answer and failed
	// while answering.
	ErrServer = errors.New(ErrVM + "server error")
)

// StatusError is a non-2xx, non-400 upstream response. It keeps the status
// code and the body: the body is where the Prometheus-compatible APIs put the
// reason, and an operator reading a log line needs the reason more than the
// number.
type StatusError struct {
	// StatusCode is the HTTP status the backend returned.
	StatusCode int
	// Path is the API path that was called.
	Path string
	// Body is the response body, truncated by the caller-side read cap.
	Body string
	// class is the sentinel for this status, so errors.Is works.
	class error
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s%s returned %d (%s): %s",
		ErrVM, e.Path, e.StatusCode, e.class, e.Body)
}

// Unwrap exposes the class sentinel, so errors.Is(err, ErrServer) answers the
// question a caller actually has.
func (e *StatusError) Unwrap() error { return e.class }

// statusError classifies an upstream response.
func statusError(path string, code int, body []byte) error {
	return &StatusError{
		StatusCode: code,
		Path:       path,
		Body:       string(body),
		class:      classOf(code),
	}
}

func classOf(code int) error {
	switch {
	case code == http.StatusUnauthorized:
		return ErrUnauthorized
	case code == http.StatusForbidden:
		return ErrForbidden
	case code == http.StatusNotFound:
		return ErrNotFound
	case code >= 400 && code < 500:
		return ErrBadRequest
	default:
		return ErrServer
	}
}

// StatusCode reports the status carried by err, or 0 when err did not come
// from an HTTP status.
func StatusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.StatusCode
	}
	return 0
}
