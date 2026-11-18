package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/observability"
)

// problem is an RFC 9457 Problem Details body.
type problem struct {
	Type          string              `json:"type"`
	Title         string              `json:"title"`
	Status        int                 `json:"status"`
	Detail        string              `json:"detail,omitempty"`
	CorrelationID string              `json:"correlation_id,omitempty"`
	Errors        []domain.FieldError `json:"errors,omitempty"`
}

// badRequestError marks malformed input (bad JSON, bad query parameter): 400.
type badRequestError struct {
	msg    string
	fields []domain.FieldError
}

func (e *badRequestError) Error() string { return e.msg }

var errMethodNotAllowed = errors.New("method not allowed")

// writeError is the single place where errors become HTTP responses.
func (a *api) writeError(w http.ResponseWriter, r *http.Request, err error) {
	p := problem{Type: "about:blank", CorrelationID: observability.CorrelationID(r.Context())}

	var (
		ve       *domain.ValidationError
		de       *domain.Error
		br       *badRequestError
		tooLarge *http.MaxBytesError
	)
	switch {
	case errors.As(err, &ve):
		p.Status, p.Title, p.Errors = http.StatusUnprocessableEntity, "Validation failed", ve.Errors
	case errors.As(err, &br):
		p.Status, p.Title, p.Detail, p.Errors = http.StatusBadRequest, "Bad request", br.msg, br.fields
	case errors.Is(err, errMethodNotAllowed):
		p.Status, p.Title = http.StatusMethodNotAllowed, "Method not allowed"
	case errors.As(err, &tooLarge):
		p.Status, p.Title = http.StatusRequestEntityTooLarge, "Request body too large"
	case errors.As(err, &de) && errors.Is(err, domain.ErrNotFound):
		p.Status, p.Title, p.Detail = http.StatusNotFound, "Not found", de.Msg
	case errors.As(err, &de) && errors.Is(err, domain.ErrConflict):
		p.Status, p.Title, p.Detail = http.StatusConflict, "Conflict", de.Msg
	case errors.As(err, &de) && errors.Is(err, domain.ErrIdempotencyMismatch):
		p.Status, p.Title, p.Detail = http.StatusUnprocessableEntity, "Idempotency key reused", de.Msg
	default:
		a.log.ErrorContext(r.Context(), "request failed", "error", err, "method", r.Method, "path", r.URL.Path)
		p.Status, p.Title = http.StatusInternalServerError, "Internal server error"
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON reads exactly one JSON object and rejects unknown fields, so typos in
// optional fields (e.g. "priorty") fail loudly instead of being silently ignored.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return &badRequestError{msg: "invalid JSON body: " + err.Error()}
	}
	if dec.More() {
		return &badRequestError{msg: "invalid JSON body: unexpected data after the object"}
	}
	return nil
}
