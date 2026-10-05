// Package admin serves the gateway's admin API: creating tenants, issuing
// and revoking their keys, and reporting their usage.
//
// It does no authentication of its own. The gateway mounts it behind
// middleware.AdminAuth, so a request that reaches these handlers already
// presented the admin key.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// Store is what the admin API needs: tenant and key management, which
// *tenant.Store provides, and usage reports, which *usage.Reports provides.
type Store interface {
	Create(ctx context.Context, name string) (tenant.Tenant, tenant.NewKey, error)
	IssueKey(ctx context.Context, tenantID string) (tenant.NewKey, error)
	RevokeKey(ctx context.Context, keyID int64) error
	TenantUsage(ctx context.Context, tenantID string, from, to time.Time) (usage.Summary, error)
}

// Handler returns the admin routes, at their full /admin/... paths.
func Handler(store Store, logger *slog.Logger) http.Handler {
	a := &api{store: store, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/tenants", a.createTenant)
	mux.HandleFunc("POST /admin/tenants/{id}/keys", a.issueKey)
	mux.HandleFunc("DELETE /admin/keys/{id}", a.revokeKey)
	mux.HandleFunc("GET /admin/tenants/{id}/usage", a.tenantUsage)
	// Everything else under /admin/, wrong methods included, gets a JSON 404
	// rather than ServeMux's plain-text 404 or 405.
	mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such admin endpoint")
	})
	return mux
}

type api struct {
	store  Store
	logger *slog.Logger
}

// tenantJSON and keyJSON are the wire shapes. They are separate from the
// tenant package's types so the API's field names are chosen here, and so a
// field added to tenant.Tenant isn't published by accident.
type tenantJSON struct {
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	RateLimitTokensPerMin int64     `json:"rate_limit_tokens_per_min"`
	BudgetMicros          int64     `json:"budget_micros"`
	BudgetPeriod          string    `json:"budget_period"`
	DefaultMaxTokens      int       `json:"default_max_tokens"`
	CreatedAt             time.Time `json:"created_at"`
}

type keyJSON struct {
	ID        int64     `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Prefix    string    `json:"prefix"`
	Plaintext string    `json:"plaintext"`
	CreatedAt time.Time `json:"created_at"`
}

func toTenantJSON(t tenant.Tenant) tenantJSON {
	return tenantJSON{
		ID:                    t.ID,
		Name:                  t.Name,
		RateLimitTokensPerMin: t.RateLimitTokensPerMin,
		BudgetMicros:          t.BudgetMicros,
		BudgetPeriod:          t.BudgetPeriod,
		DefaultMaxTokens:      t.DefaultMaxTokens,
		CreatedAt:             t.CreatedAt,
	}
}

func toKeyJSON(k tenant.NewKey) keyJSON {
	return keyJSON{
		ID:        k.ID,
		TenantID:  k.TenantID,
		Prefix:    k.Prefix,
		Plaintext: k.Plaintext,
		CreatedAt: k.CreatedAt,
	}
}

// createTenant creates a tenant and returns it with its first key. This
// response is the only time that key's plaintext exists outside the client.
func (a *api) createTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", `body must be JSON: {"name": "..."}`)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name must not be empty")
		return
	}

	t, key, err := a.store.Create(r.Context(), name)
	if err != nil {
		a.internalError(w, "creating tenant", err)
		return
	}

	a.logger.Info("tenant created", "tenant_id", t.ID, "key_id", key.ID, "key_prefix", key.Prefix)
	writeSecret(w, map[string]any{"tenant": toTenantJSON(t), "key": toKeyJSON(key)})
}

// issueKey gives an existing tenant another key, for rotation.
func (a *api) issueKey(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")

	key, err := a.store.IssueKey(r.Context(), tenantID)
	if errors.Is(err, tenant.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no tenant with that ID")
		return
	}
	if err != nil {
		a.internalError(w, "issuing key", err)
		return
	}

	a.logger.Info("key issued", "tenant_id", tenantID, "key_id", key.ID, "key_prefix", key.Prefix)
	writeSecret(w, map[string]any{"key": toKeyJSON(key)})
}

// revokeKey stops a key working. Revoking an already-revoked key succeeds.
func (a *api) revokeKey(w http.ResponseWriter, r *http.Request) {
	keyID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || keyID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "key ID must be a positive integer")
		return
	}

	err = a.store.RevokeKey(r.Context(), keyID)
	if errors.Is(err, tenant.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no key with that ID")
		return
	}
	if err != nil {
		a.internalError(w, "revoking key", err)
		return
	}

	a.logger.Info("key revoked", "key_id", keyID)
	w.WriteHeader(http.StatusNoContent)
}

// tenantUsage reports a tenant's usage over [from, to): RFC 3339 query
// parameters, each defaulting to the current calendar month in UTC, the
// period a monthly budget covers.
func (a *api) tenantUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)

	for name, t := range map[string]*time.Time{"from": &from, "to": &to} {
		v := r.URL.Query().Get(name)
		if v == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", name+" must be an RFC 3339 time, such as 2026-10-01T00:00:00Z")
			return
		}
		*t = parsed.UTC()
	}
	if !from.Before(to) {
		writeError(w, http.StatusBadRequest, "invalid_request", "from must be before to")
		return
	}

	tenantID := r.PathValue("id")
	summary, err := a.store.TenantUsage(r.Context(), tenantID, from, to)
	if errors.Is(err, tenant.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no tenant with that ID")
		return
	}
	if err != nil {
		a.internalError(w, "reading usage", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": tenantID,
		"from":      from,
		"to":        to,
		"total":     summary.Total,
		"by_model":  summary.ByModel,
	})
}

// internalError logs err and returns a 500 without its text, which may name
// database hosts or SQL.
func (a *api) internalError(w http.ResponseWriter, action string, err error) {
	a.logger.Error("admin request failed", "action", action, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "the request failed; see the gateway log")
}

// writeSecret writes a 201 whose body carries a key's plaintext. no-store
// keeps it out of any cache between the gateway and the admin client.
func writeSecret(w http.ResponseWriter, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, v)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
