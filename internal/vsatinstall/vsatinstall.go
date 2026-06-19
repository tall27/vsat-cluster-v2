// Package vsatinstall installs a VSatellite agent inside an existing container.
package vsatinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var defaultRegionBases = []string{
	"https://api.venafi.cloud/",
	"https://api.eu.venafi.cloud/",
	"https://api.au.venafi.cloud/",
	"https://api.uk.venafi.cloud/",
	"https://api.ca.venafi.cloud/",
}

const (
	BackendCCM        = "ccm"
	BackendNGTS       = "ngts"
	defaultNGTSBase   = "https://api.strata.paloaltonetworks.com/ngts/"
	ccmVSatctlURL     = "https://dl.venafi.cloud/vsatctl"
	ngtsVSatctlURL    = "https://dl.ngts.paloaltonetworks.com/vsatctl"
	scmTenantURLBase  = "https://stratacloudmanager.paloaltonetworks.com/"
	ngtsInstallSuffix = ".ngts.paloaltonetworks.com"
)

var ngtsTokenURL = "https://auth.apps.paloaltonetworks.com/oauth2/access_token"

// Runner executes a shell script inside a container.
type Runner interface {
	Exec(ctx context.Context, name, script string) ([]byte, error)
}

// InstallOpts configures a VSatellite install.
type InstallOpts struct {
	Container        string
	Protocol         string
	APIEndpointURL   string
	APIKey           string
	TenantID         string
	SecretSignature  string
	ClientID         string
	ClientSecret     string
	TSGID            string
	Runner           Runner
	HTTPClient       *http.Client
	RegionBases      []string
	PollInterval     time.Duration
	PollTimeout      time.Duration
	OnTenantMetadata func(InstallResult)
}

// InstallResult is returned to the web UI after a successful install.
type InstallResult struct {
	Backend        string `json:"backend,omitempty"`
	PairingCode    string `json:"pairingCode,omitempty"`
	PairingCodeID  string `json:"pairingCodeId,omitempty"`
	EdgeStatus     string `json:"edgeStatus,omitempty"`
	TenantURL      string `json:"tenantUrl,omitempty"`
	CompanyID      string `json:"companyId,omitempty"`
	OrganizationID string `json:"organizationId,omitempty"`
	APIBaseURL     string `json:"apiBaseUrl,omitempty"`
	APIKey         string `json:"apiKey,omitempty"`
	ClientID       string `json:"clientId,omitempty"`
	ClientSecret   string `json:"clientSecret,omitempty"`
	TSGID          string `json:"tsgId,omitempty"`
	EdgeInstanceID string `json:"edgeInstanceId,omitempty"`
}

type tenantMetadata struct {
	TenantURL      string
	CompanyID      string
	OrganizationID string
}

type edgeInstance struct {
	ID                       string `json:"id"`
	Name                     string `json:"name"`
	EdgeStatus               string `json:"edgeStatus"`
	PairingCode              string `json:"pairingCode"`
	PairingCodeID            string `json:"pairingCodeId"`
	Environment              string `json:"environmentId"`
	EnvironmentID            string `json:"environmentID"`
	IntegrationServicesCount int    `json:"integrationServicesCount"`
}

type requestAuth interface {
	Apply(context.Context, *http.Request) error
}

type apiKeyAuth string

func (a apiKeyAuth) Apply(_ context.Context, req *http.Request) error {
	req.Header.Set("tppl-api-key", string(a))
	return nil
}

type bearerAuth struct {
	client       *http.Client
	clientID     string
	clientSecret string
	tsgID        string
	token        string
	expiresAt    time.Time
}

func (a *bearerAuth) Apply(ctx context.Context, req *http.Request) error {
	token, err := a.Token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (a *bearerAuth) Token(ctx context.Context) (string, error) {
	if a.token != "" && time.Now().Before(a.expiresAt.Add(-60*time.Second)) {
		return a.token, nil
	}
	body := strings.NewReader("grant_type=client_credentials&scope=tsg_id:" + url.QueryEscape(a.tsgID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ngtsTokenURL, body)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(a.clientID, a.clientSecret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("OAuth token fetch failed: %s: %s", resp.Status, msg)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("malformed OAuth token response: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("OAuth token response did not include access_token")
	}
	if payload.ExpiresIn <= 0 {
		payload.ExpiresIn = 900
	}
	a.token = payload.AccessToken
	a.expiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	return a.token, nil
}

// InstallCCM creates a pairing code from the CCM API and runs vsatctl install
// inside the container.
func InstallCCM(ctx context.Context, opts InstallOpts) (*InstallResult, error) {
	if opts.Runner == nil {
		return nil, errors.New("installer runner is required")
	}
	container := strings.TrimSpace(opts.Container)
	if container == "" {
		return nil, errors.New("container is required")
	}
	apiKey := strings.TrimSpace(opts.APIKey)
	if apiKey == "" {
		return nil, errors.New("API key is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	base, environmentID, err := probeRegions(ctx, client, apiKey, opts.APIEndpointURL, opts.RegionBases)
	if err != nil {
		return nil, err
	}
	tenant, _ := fetchTenantMetadata(ctx, client, base, apiKey)
	if opts.OnTenantMetadata != nil && (tenant.TenantURL != "" || tenant.CompanyID != "" || tenant.OrganizationID != "") {
		opts.OnTenantMetadata(InstallResult{
			Backend:        BackendCCM,
			TenantURL:      tenant.TenantURL,
			CompanyID:      tenant.CompanyID,
			OrganizationID: tenant.OrganizationID,
			APIBaseURL:     base,
			APIKey:         apiKey,
		})
	}
	auth := apiKeyAuth(apiKey)
	if err := deleteStaleEdgeInstances(ctx, client, base, auth, environmentID); err != nil {
		return nil, err
	}
	pairingCode, pairingCodeID, err := createPairingCode(ctx, client, base, auth, environmentID)
	if err != nil {
		return nil, err
	}
	if opts.OnTenantMetadata != nil {
		opts.OnTenantMetadata(InstallResult{
			Backend:        BackendCCM,
			TenantURL:      tenant.TenantURL,
			CompanyID:      tenant.CompanyID,
			OrganizationID: tenant.OrganizationID,
			APIBaseURL:     base,
			APIKey:         apiKey,
			PairingCodeID:  pairingCodeID,
		})
	}
	if err := ensureVSatctl(ctx, opts.Runner, container); err != nil {
		return nil, err
	}
	if err := installVSatellite(ctx, opts.Runner, container, strings.TrimRight(base, "/"), pairingCode); err != nil {
		return nil, err
	}
	status, err := pollActive(ctx, client, base, auth, pairingCodeID, opts.PollInterval, opts.PollTimeout)
	if err != nil {
		return nil, err
	}
	edgeInstanceID, err := renameEdgeInstance(ctx, client, base, auth, pairingCodeID, container)
	if err != nil {
		return nil, err
	}
	return &InstallResult{
		Backend:        BackendCCM,
		PairingCode:    pairingCode,
		PairingCodeID:  pairingCodeID,
		EdgeStatus:     status,
		TenantURL:      tenant.TenantURL,
		CompanyID:      tenant.CompanyID,
		OrganizationID: tenant.OrganizationID,
		APIBaseURL:     base,
		APIKey:         apiKey,
		EdgeInstanceID: edgeInstanceID,
	}, nil
}

// InstallNGTS creates a pairing code through the SCM/NGTS API and runs
// vsatctl preflight + install against the TSG-scoped NGTS install endpoint.
func InstallNGTS(ctx context.Context, opts InstallOpts) (*InstallResult, error) {
	if opts.Runner == nil {
		return nil, errors.New("installer runner is required")
	}
	container := strings.TrimSpace(opts.Container)
	if container == "" {
		return nil, errors.New("container is required")
	}
	clientID := strings.TrimSpace(opts.ClientID)
	if clientID == "" {
		return nil, errors.New("Client ID is required")
	}
	clientSecret := strings.TrimSpace(opts.ClientSecret)
	if clientSecret == "" {
		return nil, errors.New("Client Secret is required")
	}
	tsgID := strings.TrimSpace(opts.TSGID)
	if tsgID == "" {
		return nil, errors.New("TSG ID is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	base := normalizeBase(opts.APIEndpointURL)
	if base == "" {
		base = defaultNGTSBase
	}
	auth := &bearerAuth{client: client, clientID: clientID, clientSecret: clientSecret, tsgID: tsgID}
	env, err := fetchNGTSEnvironment(ctx, client, base, auth)
	if err != nil {
		return nil, err
	}
	tenant := tenantMetadata{
		TenantURL:      scmTenantURLBase + "?tsg_id=" + tsgID,
		CompanyID:      tsgID,
		OrganizationID: env.CompanyID,
	}
	if opts.OnTenantMetadata != nil {
		opts.OnTenantMetadata(InstallResult{
			Backend:        BackendNGTS,
			TenantURL:      tenant.TenantURL,
			CompanyID:      tenant.CompanyID,
			OrganizationID: tenant.OrganizationID,
			APIBaseURL:     base,
			ClientID:       clientID,
			ClientSecret:   clientSecret,
			TSGID:          tsgID,
		})
	}
	if err := deleteStaleEdgeInstances(ctx, client, base, auth, env.ID); err != nil {
		return nil, err
	}
	pairingCode, pairingCodeID, err := createPairingCode(ctx, client, base, auth, env.ID)
	if err != nil {
		return nil, err
	}
	if opts.OnTenantMetadata != nil {
		opts.OnTenantMetadata(InstallResult{
			Backend:        BackendNGTS,
			TenantURL:      tenant.TenantURL,
			CompanyID:      tenant.CompanyID,
			OrganizationID: tenant.OrganizationID,
			APIBaseURL:     base,
			ClientID:       clientID,
			ClientSecret:   clientSecret,
			TSGID:          tsgID,
			PairingCodeID:  pairingCodeID,
		})
	}
	if err := ensureVSatctlFrom(ctx, opts.Runner, container, ngtsVSatctlURL); err != nil {
		return nil, err
	}
	installAPIURL := ngtsInstallAPIURL(tsgID)
	if err := preflightVSatellite(ctx, opts.Runner, container, installAPIURL); err != nil {
		return nil, err
	}
	if err := installVSatellite(ctx, opts.Runner, container, installAPIURL, pairingCode); err != nil {
		return nil, err
	}
	status, err := pollActive(ctx, client, base, auth, pairingCodeID, opts.PollInterval, opts.PollTimeout)
	if err != nil {
		return nil, err
	}
	edgeInstanceID, err := renameEdgeInstance(ctx, client, base, auth, pairingCodeID, container)
	if err != nil {
		return nil, err
	}
	return &InstallResult{
		Backend:        BackendNGTS,
		PairingCode:    pairingCode,
		PairingCodeID:  pairingCodeID,
		EdgeStatus:     status,
		TenantURL:      tenant.TenantURL,
		CompanyID:      tenant.CompanyID,
		OrganizationID: tenant.OrganizationID,
		APIBaseURL:     base,
		ClientID:       clientID,
		ClientSecret:   clientSecret,
		TSGID:          tsgID,
		EdgeInstanceID: edgeInstanceID,
	}, nil
}

type ngtsEnvironment struct {
	ID        string
	CompanyID string
	Name      string
}

func fetchNGTSEnvironment(ctx context.Context, client *http.Client, base string, auth requestAuth) (ngtsEnvironment, error) {
	var payload struct {
		Environments []struct {
			ID        string `json:"id"`
			CompanyID string `json:"companyId"`
			Name      string `json:"name"`
		} `json:"environments"`
	}
	if err := doJSON(ctx, client, http.MethodGet, base+"v1/environments", auth, nil, &payload); err != nil {
		return ngtsEnvironment{}, err
	}
	if len(payload.Environments) == 0 || strings.TrimSpace(payload.Environments[0].ID) == "" {
		return ngtsEnvironment{}, errors.New("no environments returned")
	}
	env := payload.Environments[0]
	return ngtsEnvironment{
		ID:        strings.TrimSpace(env.ID),
		CompanyID: strings.TrimSpace(env.CompanyID),
		Name:      strings.TrimSpace(env.Name),
	}, nil
}

func fetchTenantMetadata(ctx context.Context, client *http.Client, base, apiKey string) (tenantMetadata, error) {
	var payload struct {
		Company struct {
			URLPrefix      string `json:"urlPrefix"`
			ID             string `json:"id"`
			OrganizationID string `json:"organizationId"`
		} `json:"company"`
	}
	if err := doJSON(ctx, client, http.MethodGet, base+"v1/useraccounts", apiKeyAuth(apiKey), nil, &payload); err != nil {
		return tenantMetadata{}, err
	}
	companyID := strings.TrimSpace(payload.Company.URLPrefix)
	meta := tenantMetadata{
		CompanyID:      companyID,
		OrganizationID: strings.TrimSpace(payload.Company.ID),
	}
	if meta.OrganizationID == "" {
		meta.OrganizationID = strings.TrimSpace(payload.Company.OrganizationID)
	}
	if companyID != "" {
		meta.TenantURL = "https://" + companyID + ".venafi.cloud"
	}
	return meta, nil
}

func probeRegions(ctx context.Context, client *http.Client, apiKey, explicitBase string, regionBases []string) (string, string, error) {
	bases := []string{explicitBase}
	if strings.TrimSpace(explicitBase) == "" {
		bases = regionBases
		if len(bases) == 0 {
			bases = defaultRegionBases
		}
	}
	var lastErr error
	for _, candidate := range bases {
		base := normalizeBase(candidate)
		if base == "" {
			continue
		}
		envID, err := decodeEnvironmentID(ctx, client, base, apiKeyAuth(apiKey))
		if err == nil && envID != "" {
			return base, envID, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", "", fmt.Errorf("probe regions: %w", lastErr)
	}
	return "", "", errors.New("probe regions: no API endpoint responded")
}

func decodeEnvironmentID(ctx context.Context, client *http.Client, base string, auth requestAuth) (string, error) {
	var payload struct {
		Environments []struct {
			ID string `json:"id"`
		} `json:"environments"`
	}
	if err := doJSON(ctx, client, http.MethodGet, base+"v1/environments", auth, nil, &payload); err != nil {
		return "", err
	}
	if len(payload.Environments) == 0 || payload.Environments[0].ID == "" {
		return "", errors.New("no environments returned")
	}
	return payload.Environments[0].ID, nil
}

func createPairingCode(ctx context.Context, client *http.Client, base string, auth requestAuth, environmentID string) (string, string, error) {
	body := struct {
		EnvironmentID  string `json:"environmentId"`
		ReuseCount     int    `json:"reuseCount"`
		ExpirationDate string `json:"expirationDate,omitempty"`
	}{
		EnvironmentID: environmentID,
		ReuseCount:    1,
		ExpirationDate: time.Now().UTC().
			Add(24 * time.Hour).
			Format(time.RFC3339),
	}
	var payload struct {
		ID          string `json:"id"`
		PairingCode string `json:"pairingCode"`
	}
	if err := doJSON(ctx, client, http.MethodPost, base+"v1/pairingcodes/satellite", auth, body, &payload); err != nil {
		return "", "", fmt.Errorf("create pairing code: %w", err)
	}
	if payload.PairingCode == "" {
		return "", "", errors.New("create pairing code: empty pairing code")
	}
	if payload.ID == "" {
		return "", "", errors.New("create pairing code: empty pairing code id")
	}
	return payload.PairingCode, payload.ID, nil
}

func ensureVSatctl(ctx context.Context, runner Runner, container string) error {
	return ensureVSatctlFrom(ctx, runner, container, ccmVSatctlURL)
}

func ensureVSatctlFrom(ctx context.Context, runner Runner, container, downloadURL string) error {
	cmd := fmt.Sprintf(`if [ ! -x /tmp/vsatctl ]; then
  curl -fsSLo /tmp/vsatctl %s
  chmod +x /tmp/vsatctl
fi
/tmp/vsatctl version >/dev/null`, shellQuote(downloadURL))
	_, err := runner.Exec(ctx, container, cmd)
	return err
}

// Uninstall removes an installed VSatellite from a container before the
// container itself is deleted. Missing install dir is treated as already clean.
func Uninstall(ctx context.Context, runner Runner, container string) error {
	if runner == nil {
		return errors.New("installer runner is required")
	}
	container = strings.TrimSpace(container)
	if container == "" {
		return errors.New("container is required")
	}
	cmd := `if ! test -d /opt/vsatellite; then
  exit 0
fi
if [ ! -x /tmp/vsatctl ]; then
  curl -fsSLo /tmp/vsatctl https://dl.venafi.cloud/vsatctl
  chmod +x /tmp/vsatctl
fi
yes | /tmp/vsatctl uninstall --silent --install-dir /opt/vsatellite`
	_, err := runner.Exec(ctx, container, cmd)
	return err
}

func installVSatellite(ctx context.Context, runner Runner, container, apiURL, pairingCode string) error {
	cmd := fmt.Sprintf(
		"yes | /tmp/vsatctl install --accept-license-agreement --silent --api-url %s --pairing-code %s --install-dir /opt/vsatellite",
		shellQuote(apiURL),
		shellQuote(pairingCode),
	)
	_, err := runner.Exec(ctx, container, cmd)
	return err
}

func preflightVSatellite(ctx context.Context, runner Runner, container, apiURL string) error {
	cmd := fmt.Sprintf("/tmp/vsatctl preflight --api-url %s", shellQuote(apiURL))
	_, err := runner.Exec(ctx, container, cmd)
	return err
}

func deleteStaleEdgeInstances(ctx context.Context, client *http.Client, base string, auth requestAuth, environmentID string) error {
	instances, err := listEdgeInstances(ctx, client, base, auth)
	if err != nil {
		return fmt.Errorf("list edge instances: %w", err)
	}
	for _, inst := range instances {
		instEnvironmentID := inst.EnvironmentID
		if instEnvironmentID == "" {
			instEnvironmentID = inst.Environment
		}
		if instEnvironmentID != environmentID {
			continue
		}
		if strings.EqualFold(inst.EdgeStatus, "ACTIVE") || strings.EqualFold(inst.EdgeStatus, "CONNECTED") {
			continue
		}
		if inst.IntegrationServicesCount != 0 {
			continue
		}
		id := strings.TrimSpace(inst.ID)
		if id == "" {
			continue
		}
		if err := doJSON(ctx, client, http.MethodDelete, base+"v1/edgeinstances/"+id, auth, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func pollActive(ctx context.Context, client *http.Client, base string, auth requestAuth, pairingCodeID string, interval, timeout time.Duration) (string, error) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		status, err := fetchEdgeStatus(ctx, client, base, auth, pairingCodeID)
		if err == nil && strings.EqualFold(status, "active") {
			return strings.ToLower(status), nil
		}
		select {
		case <-ctx.Done():
			if status != "" {
				return status, fmt.Errorf("edge status returned %s", status)
			}
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

func fetchEdgeStatus(ctx context.Context, client *http.Client, base string, auth requestAuth, pairingCodeID string) (string, error) {
	instances, err := listEdgeInstances(ctx, client, base, auth)
	if err != nil {
		return "", err
	}
	for _, inst := range instances {
		if inst.PairingCodeID == pairingCodeID {
			return inst.EdgeStatus, nil
		}
	}
	return "", errors.New("edge instance not found")
}

// DeleteEdgeInstance removes a registered VSatellite from TLS Protect Cloud.
func DeleteEdgeInstance(ctx context.Context, client *http.Client, base, apiKey, edgeInstanceID, pairingCodeID, name string) error {
	base = normalizeBase(base)
	apiKey = strings.TrimSpace(apiKey)
	edgeInstanceID = strings.TrimSpace(edgeInstanceID)
	pairingCodeID = strings.TrimSpace(pairingCodeID)
	name = strings.TrimSpace(name)
	if base == "" || apiKey == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if edgeInstanceID == "" {
		instances, err := listEdgeInstances(ctx, client, base, apiKeyAuth(apiKey))
		if err != nil {
			return err
		}
		for _, inst := range instances {
			switch {
			case pairingCodeID != "" && inst.PairingCodeID == pairingCodeID:
				edgeInstanceID = strings.TrimSpace(inst.ID)
			case name != "" && inst.Name == name:
				edgeInstanceID = strings.TrimSpace(inst.ID)
			}
			if edgeInstanceID != "" {
				break
			}
		}
	}
	if edgeInstanceID == "" {
		return nil
	}
	return doJSON(ctx, client, http.MethodDelete, base+"v1/edgeinstances/"+edgeInstanceID, apiKeyAuth(apiKey), nil, nil)
}

// DeleteNGTSEdgeInstance removes a registered VSatellite from NGTS.
func DeleteNGTSEdgeInstance(ctx context.Context, client *http.Client, base, clientID, clientSecret, tsgID, edgeInstanceID, pairingCodeID, name string) error {
	base = normalizeBase(base)
	if base == "" {
		base = defaultNGTSBase
	}
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	tsgID = strings.TrimSpace(tsgID)
	edgeInstanceID = strings.TrimSpace(edgeInstanceID)
	pairingCodeID = strings.TrimSpace(pairingCodeID)
	name = strings.TrimSpace(name)
	if clientID == "" || clientSecret == "" || tsgID == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	auth := &bearerAuth{client: client, clientID: clientID, clientSecret: clientSecret, tsgID: tsgID}
	if edgeInstanceID == "" {
		instances, err := listEdgeInstances(ctx, client, base, auth)
		if err != nil {
			return err
		}
		for _, inst := range instances {
			switch {
			case pairingCodeID != "" && inst.PairingCodeID == pairingCodeID:
				edgeInstanceID = strings.TrimSpace(inst.ID)
			case name != "" && inst.Name == name:
				edgeInstanceID = strings.TrimSpace(inst.ID)
			}
			if edgeInstanceID != "" {
				break
			}
		}
	}
	if edgeInstanceID == "" {
		return nil
	}
	return doJSON(ctx, client, http.MethodDelete, base+"v1/edgeinstances/"+edgeInstanceID, auth, nil, nil)
}

func renameEdgeInstance(ctx context.Context, client *http.Client, base string, auth requestAuth, pairingCodeID, name string) (string, error) {
	instances, err := listEdgeInstances(ctx, client, base, auth)
	if err != nil {
		return "", err
	}
	for _, inst := range instances {
		if inst.PairingCodeID != pairingCodeID || strings.TrimSpace(inst.ID) == "" {
			continue
		}
		id := strings.TrimSpace(inst.ID)
		body := struct {
			Name string `json:"name"`
		}{Name: name}
		return id, doJSON(ctx, client, http.MethodPut, base+"v1/edgeinstances/"+id, auth, body, nil)
	}
	return "", errors.New("edge instance not found")
}

func listEdgeInstances(ctx context.Context, client *http.Client, base string, auth requestAuth) ([]edgeInstance, error) {
	var payload struct {
		EdgeInstances []edgeInstance `json:"edgeInstances"`
	}
	if err := doJSON(ctx, client, http.MethodGet, base+"v1/edgeinstances", auth, nil, &payload); err != nil {
		return nil, err
	}
	return payload.EdgeInstances, nil
}

func doJSON(ctx context.Context, client *http.Client, method, url string, auth requestAuth, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if auth != nil {
		if err := auth.Apply(ctx, req); err != nil {
			return err
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("%s: %s", resp.Status, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("malformed response from server: %w", err)
	}
	return nil
}

func normalizeBase(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return strings.TrimRight(base, "/") + "/"
}

func ngtsInstallAPIURL(tsgID string) string {
	return "https://" + strings.TrimSpace(tsgID) + ngtsInstallSuffix
}

var shellUnsafe = regexp.MustCompile(`[^A-Za-z0-9_@%+=:,./-]`)

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !shellUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
