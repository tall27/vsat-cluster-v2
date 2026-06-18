package vsatinstall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	container string
	scripts   []string
}

func (f *fakeRunner) Exec(_ context.Context, name, script string) ([]byte, error) {
	f.container = name
	f.scripts = append(f.scripts, script)
	return []byte("ok"), nil
}

func TestInstallCCMCreatesPairingCodeAndRunsVSatctl(t *testing.T) {
	var sawAPIKey bool
	var pairingRequest struct {
		EnvironmentID string `json:"environmentId"`
		ReuseCount    int    `json:"reuseCount"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("tppl-api-key") == "secret-key" {
			sawAPIKey = true
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments":
			json.NewEncoder(w).Encode(map[string]any{"environments": []map[string]string{{"id": "env-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/edgeinstances":
			json.NewEncoder(w).Encode(map[string]any{"edgeInstances": []map[string]string{{"name": "vsat-a", "edgeStatus": "active"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/edgeinstances/pairingcode":
			if err := json.NewDecoder(r.Body).Decode(&pairingRequest); err != nil {
				t.Fatalf("decode pairing request: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]string{"pairingCode": "pair-123"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	runner := &fakeRunner{}
	result, err := InstallCCM(context.Background(), InstallOpts{
		Container:    "vsat-a",
		APIKey:       "secret-key",
		HTTPClient:   server.Client(),
		RegionBases:  []string{server.URL},
		Runner:       runner,
		PollInterval: time.Millisecond,
		PollTimeout:  time.Second,
	})
	if err != nil {
		t.Fatalf("InstallCCM: %v", err)
	}
	if !sawAPIKey {
		t.Fatal("expected API key header on CCM requests")
	}
	if pairingRequest.EnvironmentID != "env-1" || pairingRequest.ReuseCount != 1 {
		t.Fatalf("unexpected pairing request: %+v", pairingRequest)
	}
	if result.PairingCode != "pair-123" || result.EdgeStatus != "active" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(runner.scripts) != 2 {
		t.Fatalf("expected ensure+install scripts, got %d", len(runner.scripts))
	}
	if !strings.Contains(runner.scripts[0], "https://dl.venafi.cloud/vsatctl") {
		t.Fatalf("ensure script missing vsatctl download: %s", runner.scripts[0])
	}
	if !strings.Contains(runner.scripts[1], "--pairing-code pair-123") {
		t.Fatalf("install script missing pairing code: %s", runner.scripts[1])
	}
}

func TestInstallCCMRequiresAPIKey(t *testing.T) {
	_, err := InstallCCM(context.Background(), InstallOpts{Container: "vsat-a", Runner: &fakeRunner{}})
	if err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("expected API key error, got %v", err)
	}
}
