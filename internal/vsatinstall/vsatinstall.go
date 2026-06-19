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

// Runner executes a shell script inside a container.
type Runner interface {
	Exec(ctx context.Context, name, script string) ([]byte, error)
}

// InstallOpts configures a VSatellite install.
type InstallOpts struct {
	Container       string
	Protocol        string
	APIEndpointURL  string
	APIKey          string
	TenantID        string
	SecretSignature string
	Runner          Runner
	HTTPClient      *http.Client
	RegionBases     []string
	PollInterval    time.Duration
	PollTimeout     time.Duration
}

// InstallResult is returned to the web UI after a successful install.
type InstallResult struct {
	PairingCode    string `json:"pairingCode,omitempty"`
	EdgeStatus     string `json:"edgeStatus,omitempty"`
	TenantURL      string `json:"tenantUrl,omitempty"`
	CompanyID      string `json:"companyId,omitempty"`
	OrganizationID string `json:"organizationId,omitempty"`
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
	if err := deleteStaleEdgeInstances(ctx, client, base, apiKey, environmentID); err != nil {
		return nil, err
	}
	pairingCode, pairingCodeID, err := createPairingCode(ctx, client, base, apiKey, environmentID)
	if err != nil {
		return nil, err
	}
	if err := ensureVSatctl(ctx, opts.Runner, container); err != nil {
		return nil, err
	}
	if err := installVSatellite(ctx, opts.Runner, container, strings.TrimRight(base, "/"), pairingCode); err != nil {
		return nil, err
	}
	status, err := pollActive(ctx, client, base, apiKey, pairingCodeID, opts.PollInterval, opts.PollTimeout)
	if err != nil {
		return nil, err
	}
	if err := renameEdgeInstance(ctx, client, base, apiKey, pairingCodeID, container); err != nil {
		return nil, err
	}
	return &InstallResult{
		PairingCode:    pairingCode,
		EdgeStatus:     status,
		TenantURL:      tenant.TenantURL,
		CompanyID:      tenant.CompanyID,
		OrganizationID: tenant.OrganizationID,
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
	if err := doJSON(ctx, client, http.MethodGet, base+"v1/useraccounts", apiKey, nil, &payload); err != nil {
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
		envID, err := decodeEnvironmentID(ctx, client, base, apiKey)
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

func decodeEnvironmentID(ctx context.Context, client *http.Client, base, apiKey string) (string, error) {
	var payload struct {
		Environments []struct {
			ID string `json:"id"`
		} `json:"environments"`
	}
	if err := doJSON(ctx, client, http.MethodGet, base+"v1/environments", apiKey, nil, &payload); err != nil {
		return "", err
	}
	if len(payload.Environments) == 0 || payload.Environments[0].ID == "" {
		return "", errors.New("no environments returned")
	}
	return payload.Environments[0].ID, nil
}

func createPairingCode(ctx context.Context, client *http.Client, base, apiKey, environmentID string) (string, string, error) {
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
	if err := doJSON(ctx, client, http.MethodPost, base+"v1/pairingcodes/satellite", apiKey, body, &payload); err != nil {
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
	cmd := `if [ ! -x /tmp/vsatctl ]; then
  curl -fsSLo /tmp/vsatctl https://dl.venafi.cloud/vsatctl
  chmod +x /tmp/vsatctl
fi
/tmp/vsatctl version >/dev/null`
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

func deleteStaleEdgeInstances(ctx context.Context, client *http.Client, base, apiKey, environmentID string) error {
	instances, err := listEdgeInstances(ctx, client, base, apiKey)
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
		if err := doJSON(ctx, client, http.MethodDelete, base+"v1/edgeinstances/"+id, apiKey, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func pollActive(ctx context.Context, client *http.Client, base, apiKey, pairingCodeID string, interval, timeout time.Duration) (string, error) {
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
		status, err := fetchEdgeStatus(ctx, client, base, apiKey, pairingCodeID)
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

func fetchEdgeStatus(ctx context.Context, client *http.Client, base, apiKey, pairingCodeID string) (string, error) {
	instances, err := listEdgeInstances(ctx, client, base, apiKey)
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

func renameEdgeInstance(ctx context.Context, client *http.Client, base, apiKey, pairingCodeID, name string) error {
	instances, err := listEdgeInstances(ctx, client, base, apiKey)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if inst.PairingCodeID != pairingCodeID || strings.TrimSpace(inst.ID) == "" {
			continue
		}
		body := struct {
			Name string `json:"name"`
		}{Name: name}
		return doJSON(ctx, client, http.MethodPut, base+"v1/edgeinstances/"+inst.ID, apiKey, body, nil)
	}
	return errors.New("edge instance not found")
}

func listEdgeInstances(ctx context.Context, client *http.Client, base, apiKey string) ([]edgeInstance, error) {
	var payload struct {
		EdgeInstances []edgeInstance `json:"edgeInstances"`
	}
	if err := doJSON(ctx, client, http.MethodGet, base+"v1/edgeinstances", apiKey, nil, &payload); err != nil {
		return nil, err
	}
	return payload.EdgeInstances, nil
}

func doJSON(ctx context.Context, client *http.Client, method, url, apiKey string, body any, out any) error {
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
	req.Header.Set("tppl-api-key", apiKey)
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
