package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

func newProbeContext(t *testing.T, path string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	return ctx, recorder
}

func TestHealthzAlwaysOK(t *testing.T) {
	ctx, recorder := newProbeContext(t, "/api/healthz")
	Healthz(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", recorder.Code)
	}
}

// Readyz must fail closed: with no database handle the instance cannot serve
// traffic, so the probe has to report 503 instead of the historical
// /api/status behaviour of always answering success.
func TestReadyzReportsUnavailableWithoutDatabase(t *testing.T) {
	previous := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = previous })

	ctx, recorder := newProbeContext(t, "/api/readyz")
	Readyz(ctx)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", recorder.Code)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", body.Status)
	}
	if body.Checks["database"] != "unavailable" {
		t.Fatalf("checks.database = %q, want unavailable", body.Checks["database"])
	}
}