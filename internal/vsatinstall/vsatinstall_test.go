package vsatinstall

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	container string
	scripts   []string
	err       error
}

func (f *fakeRunner) Exec(_ context.Context, name, script string) ([]byte, error) {
	f.container = name
	f.scripts = append(f.scripts, script)
	if f.err != nil {
		return nil, f.err
	}
	return []byte("ok"), nil
}

func TestInstallCCMCreatesPairingCodeAndRunsVSatctl(t *testing.T) {
	var sawAPIKey bool
	var pairingRequest struct {
		EnvironmentID  string `json:"environmentId"`
		ReuseCount     int    `json:"reuseCount"`
		ExpirationDate string `json:"expirationDate"`
	}
	var renamedEdge string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("tppl-api-key") == "secret-key" {
			sawAPIKey = true
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/useraccounts":
			json.NewEncoder(w).Encode(map[string]any{"company": map[string]string{"urlPrefix": "demo", "id": "org-1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments":
			json.NewEncoder(w).Encode(map[string]any{"environments": []map[string]string{{"id": "env-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/edgeinstances":
			json.NewEncoder(w).Encode(map[string]any{"edgeInstances": []map[string]string{{"id": "edge-1", "pairingCodeId": "pair-id-1", "edgeStatus": "ACTIVE"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/pairingcodes/satellite":
			if err := json.NewDecoder(r.Body).Decode(&pairingRequest); err != nil {
				t.Fatalf("decode pairing request: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "pair-id-1", "pairingCode": "pair-123"})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/edgeinstances/edge-1":
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode rename request: %v", err)
			}
			renamedEdge = body.Name
			w.WriteHeader(http.StatusNoContent)
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
	if pairingRequest.EnvironmentID != "env-1" || pairingRequest.ReuseCount != 1 || pairingRequest.ExpirationDate == "" {
		t.Fatalf("unexpected pairing request: %+v", pairingRequest)
	}
	if renamedEdge != "vsat-a" {
		t.Fatalf("expected edge rename to vsat-a, got %q", renamedEdge)
	}
	if result.PairingCode != "pair-123" || result.EdgeStatus != "active" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.TenantURL != "https://demo.venafi.cloud" || result.CompanyID != "demo" || result.OrganizationID != "org-1" ||
		result.APIBaseURL == "" || result.APIKey != "secret-key" || result.EdgeInstanceID != "edge-1" || result.PairingCodeID != "pair-id-1" {
		t.Fatalf("unexpected tenant metadata: %+v", result)
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
	if strings.Contains(runner.scripts[1], "--api-url "+server.URL+"/ ") {
		t.Fatalf("install script should pass api-url without trailing slash: %s", runner.scripts[1])
	}
}

func TestInstallCCMReportsTenantMetadataBeforeInstallFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments":
			json.NewEncoder(w).Encode(map[string]any{"environments": []map[string]string{{"id": "env-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/useraccounts":
			json.NewEncoder(w).Encode(map[string]any{"company": map[string]string{"urlPrefix": "demo", "organizationId": "org-1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/edgeinstances":
			json.NewEncoder(w).Encode(map[string]any{"edgeInstances": []map[string]string{}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/pairingcodes/satellite":
			json.NewEncoder(w).Encode(map[string]string{"id": "pair-id-1", "pairingCode": "pair-123"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var captured InstallResult
	_, err := InstallCCM(context.Background(), InstallOpts{
		Container:    "vsat-a",
		APIKey:       "secret-key",
		HTTPClient:   server.Client(),
		RegionBases:  []string{server.URL},
		Runner:       &fakeRunner{err: errors.New("install interrupted")},
		PollInterval: time.Millisecond,
		PollTimeout:  time.Second,
		OnTenantMetadata: func(meta InstallResult) {
			captured = meta
		},
	})
	if err == nil || !strings.Contains(err.Error(), "install interrupted") {
		t.Fatalf("expected install failure, got %v", err)
	}
	if captured.TenantURL != "https://demo.venafi.cloud" || captured.CompanyID != "demo" || captured.OrganizationID != "org-1" ||
		captured.APIBaseURL == "" || captured.APIKey != "secret-key" || captured.PairingCodeID != "pair-id-1" {
		t.Fatalf("tenant metadata should be reported before install failure, got %+v", captured)
	}
}

func TestInstallCCMRequiresAPIKey(t *testing.T) {
	_, err := InstallCCM(context.Background(), InstallOpts{Container: "vsat-a", Runner: &fakeRunner{}})
	if err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("expected API key error, got %v", err)
	}
}

func TestInstallNGTSUsesOAuthAndTSGScopedInstallURL(t *testing.T) {
	var sawBearer bool
	var pairingRequest struct {
		EnvironmentID string `json:"environmentId"`
		ReuseCount    int    `json:"reuseCount"`
	}
	var renamedEdge string
	tenant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer ngts-token" {
			sawBearer = true
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments":
			json.NewEncoder(w).Encode(map[string]any{"environments": []map[string]string{{
				"id": "env-1", "companyId": "company-1", "name": "My Environment",
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/edgeinstances":
			json.NewEncoder(w).Encode(map[string]any{"edgeInstances": []map[string]string{{"id": "edge-1", "pairingCodeId": "pair-id-1", "edgeStatus": "ACTIVE"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/pairingcodes/satellite":
			if err := json.NewDecoder(r.Body).Decode(&pairingRequest); err != nil {
				t.Fatalf("decode pairing request: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "pair-id-1", "pairingCode": "pair-123"})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/edgeinstances/edge-1":
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode rename request: %v", err)
			}
			renamedEdge = body.Name
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected tenant request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer tenant.Close()

	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected token POST, got %s", r.Method)
		}
		clientID, clientSecret, ok := r.BasicAuth()
		if !ok || clientID != "client-id" || clientSecret != "client-secret" {
			t.Fatalf("unexpected token basic auth: %q %q %v", clientID, clientSecret, ok)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "tsg_id:1926383011" {
			t.Fatalf("unexpected token form: %s", r.Form.Encode())
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "ngts-token", "expires_in": 899})
	}))
	defer token.Close()
	oldTokenURL := ngtsTokenURL
	ngtsTokenURL = token.URL
	defer func() { ngtsTokenURL = oldTokenURL }()

	runner := &fakeRunner{}
	result, err := InstallNGTS(context.Background(), InstallOpts{
		Container:      "vsat-a",
		APIEndpointURL: tenant.URL,
		ClientID:       "client-id",
		ClientSecret:   "client-secret",
		TSGID:          "1926383011",
		HTTPClient:     tenant.Client(),
		Runner:         runner,
		PollInterval:   time.Millisecond,
		PollTimeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("InstallNGTS: %v", err)
	}
	if !sawBearer {
		t.Fatal("expected bearer auth on NGTS requests")
	}
	if pairingRequest.EnvironmentID != "env-1" || pairingRequest.ReuseCount != 1 {
		t.Fatalf("unexpected pairing request: %+v", pairingRequest)
	}
	if renamedEdge != "vsat-a" {
		t.Fatalf("expected edge rename to vsat-a, got %q", renamedEdge)
	}
	if result.Backend != BackendNGTS || result.TenantURL != "https://stratacloudmanager.paloaltonetworks.com/?tsg_id=1926383011" ||
		result.CompanyID != "1926383011" || result.OrganizationID != "company-1" || result.TSGID != "1926383011" ||
		result.ClientID != "client-id" || result.ClientSecret != "client-secret" || result.EdgeInstanceID != "edge-1" {
		t.Fatalf("unexpected NGTS result: %+v", result)
	}
	if len(runner.scripts) != 3 {
		t.Fatalf("expected ensure+preflight+install scripts, got %d", len(runner.scripts))
	}
	if !strings.Contains(runner.scripts[0], "https://dl.ngts.paloaltonetworks.com/vsatctl") {
		t.Fatalf("ensure script missing NGTS download: %s", runner.scripts[0])
	}
	if !strings.Contains(runner.scripts[1], "preflight --api-url https://1926383011.ngts.paloaltonetworks.com") {
		t.Fatalf("preflight script missing TSG-scoped api-url: %s", runner.scripts[1])
	}
	if !strings.Contains(runner.scripts[2], "--api-url https://1926383011.ngts.paloaltonetworks.com") ||
		!strings.Contains(runner.scripts[2], "--pairing-code pair-123") {
		t.Fatalf("install script missing NGTS install inputs: %s", runner.scripts[2])
	}
}

func TestInstallNGTSRequiresSCMCredentials(t *testing.T) {
	_, err := InstallNGTS(context.Background(), InstallOpts{Container: "vsat-a", Runner: &fakeRunner{}})
	if err == nil || !strings.Contains(err.Error(), "Client ID is required") {
		t.Fatalf("expected Client ID error, got %v", err)
	}
}

func TestEnsureVSatctlDownloadsToTmpPath(t *testing.T) {
	runner := &fakeRunner{}
	if err := ensureVSatctl(context.Background(), runner, "vsat-a"); err != nil {
		t.Fatalf("ensureVSatctl: %v", err)
	}
	if len(runner.scripts) != 1 {
		t.Fatalf("expected one script, got %d", len(runner.scripts))
	}
	script := runner.scripts[0]
	if !strings.Contains(script, "curl -fsSLo /tmp/vsatctl https://dl.venafi.cloud/vsatctl") {
		t.Fatalf("expected curl to write /tmp/vsatctl directly, got: %s", script)
	}
	if strings.Contains(script, "curl -fsSLO") {
		t.Fatalf("curl -O saves to cwd and breaks chmod /tmp/vsatctl: %s", script)
	}
}

func TestUninstallRunsVSatctlUninstallBeforeRemoval(t *testing.T) {
	runner := &fakeRunner{}
	if err := Uninstall(context.Background(), runner, "vsat-a"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(runner.scripts) != 1 {
		t.Fatalf("expected one uninstall script, got %d", len(runner.scripts))
	}
	if !strings.Contains(runner.scripts[0], "test -d /opt/vsatellite") {
		t.Fatalf("uninstall should skip missing install dir safely: %s", runner.scripts[0])
	}
	if !strings.Contains(runner.scripts[0], "curl -fsSLo /tmp/vsatctl https://dl.venafi.cloud/vsatctl") {
		t.Fatalf("uninstall should download vsatctl to /tmp/vsatctl when needed: %s", runner.scripts[0])
	}
	if !strings.Contains(runner.scripts[0], "yes | /tmp/vsatctl uninstall --silent --install-dir /opt/vsatellite") {
		t.Fatalf("missing vsatctl uninstall command: %s", runner.scripts[0])
	}
}
