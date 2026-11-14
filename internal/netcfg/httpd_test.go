package netcfg

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTelemetryServerLifecycleAndEndpoints(t *testing.T) {
	ts := NewTelemetryServer()
	if running, _ := ts.Status(); running {
		t.Fatal("expected server initially stopped")
	}

	if err := ts.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	running, addr := ts.Status()
	if !running || addr == "" {
		t.Fatalf("expected running server, got running=%v addr=%q", running, addr)
	}

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ts.Stop(ctx)
	}()

	// GET /
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "myos v2.0") {
		t.Fatalf("unexpected dashboard response: status=%d", resp.StatusCode)
	}

	// GET /api/metrics
	mresp, err := http.Get("http://" + addr + "/api/metrics")
	if err != nil {
		t.Fatalf("GET /api/metrics failed: %v", err)
	}
	var payload MetricsPayload
	err = json.NewDecoder(mresp.Body).Decode(&payload)
	mresp.Body.Close()
	if err != nil {
		t.Fatalf("decode metrics failed: %v", err)
	}
	if mresp.StatusCode != http.StatusOK || payload.Memory.TotalKB == 0 || payload.Timestamp == 0 {
		t.Fatalf("unexpected metrics payload: %+v", payload)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ts.Stop(ctx); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if running, _ := ts.Status(); running {
		t.Fatal("expected server stopped after Stop")
	}
}
