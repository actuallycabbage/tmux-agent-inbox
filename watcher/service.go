package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type registration struct {
	URL      string  `json:"url"`
	PID      int     `json:"pid"`
	Version  string  `json:"version"`
	Password *string `json:"password"`
}

type serverInfo struct {
	PID     int      `json:"pid"`
	Version string   `json:"version"`
	URLs    []string `json:"urls"`
}

type envelope[T any] struct {
	Data T `json:"data"`
}

type api struct {
	registration registration
	client       *http.Client
}

type apiError struct {
	status int
	path   string
}

func (err *apiError) Error() string {
	return fmt.Sprintf("OpenCode API: %d %s", err.status, err.path)
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		// This is a registered, authenticated service endpoint. A redirect
		// must not forward its credentials or silently select another server.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func discover(ctx context.Context, base string, client *http.Client) (*api, serverInfo, error) {
	var registered registration
	var info serverInfo
	// Read-only adapter for OpenCode V2's service.json, matching the 2.0.11
	// client's Service.discover: optional Basic auth, then PID/version probe.
	// Re-read each poll so restarts and password rotation are picked up.
	if err := readJSON(filepath.Join(base, "opencode", "service.json"), &registered); err != nil {
		return nil, info, fmt.Errorf("OpenCode service discovery: %w", err)
	}
	endpoint, err := url.Parse(registered.URL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || registered.PID <= 0 {
		return nil, info, errors.New("invalid OpenCode service registration")
	}
	service := &api{registration: registered, client: client}
	if err := service.get(ctx, "/api/info", &info); err != nil {
		return nil, info, err
	}
	// Registration can outlive a server. Check identity before using its
	// session data, not just whether something happens to listen on its port.
	if info.PID != registered.PID || (registered.Version != "" && info.Version != registered.Version) || !strings.HasPrefix(info.Version, "2.") || len(info.URLs) == 0 {
		return nil, info, errors.New("OpenCode service identity/version does not match its registration")
	}
	return service, info, nil
}

func (service *api) get(ctx context.Context, path string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(service.registration.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	if password := service.registration.Password; password != nil {
		req.SetBasicAuth("opencode", *password)
	}
	response, err := service.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &apiError{status: response.StatusCode, path: path}
	}
	return json.NewDecoder(response.Body).Decode(value)
}

func (service *api) snapshot(ctx context.Context, bridges []bridge, previous []row) ([]row, error) {
	var active envelope[map[string]json.RawMessage]
	var locations []location
	if err := service.get(ctx, "/api/session/active", &active); err != nil {
		return nil, err
	}
	if err := service.get(ctx, "/api/debug/location", &locations); err != nil {
		return nil, err
	}
	if active.Data == nil || locations == nil {
		return nil, errors.New("OpenCode returned invalid active-session/location data")
	}
	var forms, permissions []request
	var shells []shell
	// Full pending snapshots recover requests created before startup and
	// during disconnections. Missing data must not become a false zero count.
	for _, location := range locations {
		query := url.Values{"location[directory]": {location.Directory}}.Encode()
		var questions, approvals envelope[[]request]
		var commands envelope[[]shell]
		if err := service.get(ctx, "/api/form?"+query, &questions); err != nil {
			return nil, err
		}
		if err := service.get(ctx, "/api/permission/request?"+query, &approvals); err != nil {
			return nil, err
		}
		if err := service.get(ctx, "/api/shell?"+query, &commands); err != nil {
			return nil, err
		}
		if questions.Data == nil || approvals.Data == nil {
			return nil, errors.New("OpenCode returned invalid pending-request data")
		}
		if commands.Data == nil {
			return nil, errors.New("OpenCode returned invalid running-shell data")
		}
		forms = append(forms, questions.Data...)
		permissions = append(permissions, approvals.Data...)
		shells = append(shells, commands.Data...)
	}
	ids := make(map[string]bool)
	for id := range active.Data {
		ids[id] = true
	}
	for _, item := range append(append([]request{}, forms...), permissions...) {
		ids[item.SessionID] = true
	}
	for _, command := range shells {
		// The shared service can run commands from several sessions in one
		// directory. Only explicit session metadata establishes ownership.
		if command.Status == "running" && command.Metadata.SessionID != "" {
			ids[command.Metadata.SessionID] = true
		}
	}
	for _, bridge := range bridges {
		for _, id := range bridge.Sessions {
			ids[id] = true
		}
	}
	for _, item := range previous {
		ids[item.ID] = true
	}
	queue := make([]string, 0, len(ids))
	for id := range ids {
		queue = append(queue, id)
	}
	sort.Strings(queue)
	sessions := make(map[string]session)
	for i := 0; i < len(queue); i++ {
		id := queue[i]
		var result envelope[session]
		if err := service.get(ctx, "/api/session/"+url.PathEscape(id), &result); err != nil {
			var status *apiError
			if errors.As(err, &status) && status.status == http.StatusNotFound {
				continue
			}
			return nil, err
		}
		item := result.Data
		if item.ID != id || item.Location.Directory == "" {
			return nil, fmt.Errorf("OpenCode returned invalid session data for %s", id)
		}
		sessions[id] = item
		if item.ParentID != "" && !ids[item.ParentID] {
			ids[item.ParentID] = true
			queue = append(queue, item.ParentID)
		}
	}
	return makeRows(sessions, active.Data, forms, permissions, shells, bridges, previous, time.Now().UnixMilli()), nil
}
