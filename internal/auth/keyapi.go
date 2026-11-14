package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
)

// keyAPI is the admin API for ingest keys. It is mounted only when auth is on:
// with auth off it would be an open endpoint that mints credentials nothing checks.
type keyAPI struct{ keys *Keyring }

func (k *keyAPI) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/keys", k.create)
	mux.HandleFunc("GET /api/v1/keys", k.list)
	mux.HandleFunc("DELETE /api/v1/keys/{id}", k.revoke)
}

type keyView struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Limits     Limits     `json:"limits"`
	// Key is the secret, present only in the response that created it.
	Key string `json:"key,omitempty"`
}

func view(r meta.APIKey) keyView {
	v := keyView{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt}
	if !r.LastUsedAt.IsZero() {
		t := r.LastUsedAt
		v.LastUsedAt = &t
	}
	if !r.RevokedAt.IsZero() {
		t := r.RevokedAt
		v.RevokedAt = &t
	}
	_ = json.Unmarshal([]byte(r.LimitsJSON), &v.Limits)
	return v
}

func (k *keyAPI) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Limits Limits `json:"limits"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected {\"name\": ..., \"limits\": {...}}")
		return
	}
	secret, row, err := k.keys.Create(r.Context(), body.Name, body.Limits)
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		// Ours, not the caller's: say so, and keep the database's wording out of
		// the response.
		writeError(w, http.StatusInternalServerError, "could not create the key")
		return
	}
	v := view(row)
	v.Key = secret
	writeJSON(w, http.StatusCreated, v)
}

func (k *keyAPI) list(w http.ResponseWriter, r *http.Request) {
	rows, err := k.keys.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list keys")
		return
	}
	out := make([]keyView, 0, len(rows))
	for _, r := range rows {
		out = append(out, view(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (k *keyAPI) revoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad key id")
		return
	}
	switch err := k.keys.Revoke(r.Context(), id); {
	case errors.Is(err, meta.ErrNoAPIKey):
		writeError(w, http.StatusNotFound, "no such key")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not revoke the key")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
