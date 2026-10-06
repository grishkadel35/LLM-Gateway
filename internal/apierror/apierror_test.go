package apierror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, http.StatusBadGateway, "upstream_unavailable", "could not reach it", map[string]any{"provider": "groq"})

	if rec.Code != http.StatusBadGateway || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("status %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body struct {
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"type": "upstream_unavailable", "message": "could not reach it", "provider": "groq"}
	if len(body.Error) != len(want) {
		t.Errorf("error = %v, want %v", body.Error, want)
	}
	for k, v := range want {
		if body.Error[k] != v {
			t.Errorf("error[%q] = %q, want %q", k, body.Error[k], v)
		}
	}
}
