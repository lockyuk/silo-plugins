package main

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
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Conn is one app's address and API key.
type Conn struct {
	URL    string
	APIKey string
}

func (c Conn) ok() bool { return c.URL != "" && c.APIKey != "" }

type App struct {
	log  hclog.Logger
	http *http.Client
	mu   sync.RWMutex
	apps map[string]Conn
}

func newApp(log hclog.Logger) *App {
	return &App{log: log, http: &http.Client{Timeout: 20 * time.Second}, apps: map[string]Conn{}}
}

func (a *App) configure(entries []*pluginv1.ConfigEntry) {
	next := map[string]Conn{}
	for _, e := range entries {
		if e == nil || e.GetValue() == nil {
			continue
		}
		v := e.GetValue().AsMap()
		get := func(k string) string { s, _ := v[k].(string); return strings.TrimSpace(s) }
		switch e.GetKey() {
		case "radarr", "sonarr", "sabnzbd", "seerr":
			next[e.GetKey()] = Conn{URL: strings.TrimRight(get("url"), "/"), APIKey: get("api_key")}
		}
	}
	a.mu.Lock()
	a.apps = next
	a.mu.Unlock()
	for _, k := range []string{"radarr", "sonarr", "sabnzbd", "seerr"} {
		c := next[k]
		a.log.Info("configured", "app", k, "url", c.URL, "has_key", c.APIKey != "")
	}
}

// check talks to one app with its settings and says what is wrong in words an
// admin can act on. It is what Silo's "Test" button runs.
func (a *App) check(ctx context.Context, app string) error {
	c := a.conn(app)
	title := map[string]string{"radarr": "Radarr", "sonarr": "Sonarr", "sabnzbd": "SABnzbd", "seerr": "Seerr"}[app]
	if title == "" {
		return fmt.Errorf("unknown app %q", app)
	}
	if c.URL == "" {
		return fmt.Errorf("enter %s's address, for example http://192.168.1.10:%s", title, map[string]string{"radarr": "7878", "sonarr": "8989", "sabnzbd": "8080", "seerr": "5055"}[app])
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s's address must start with http:// or https:// and include the host, for example http://192.168.1.10:7878", title)
	}
	if c.APIKey == "" {
		return fmt.Errorf("enter %s's API key", title)
	}
	var body []byte
	if app == "sabnzbd" {
		// mode=version answers without a key, so ask for the queue, which needs one.
		body, err = a.do(ctx, http.MethodGet, c.URL+"/api?mode=queue&limit=1&output=json&apikey="+url.QueryEscape(c.APIKey), nil, nil)
	} else if app == "seerr" {
		body, err = a.do(ctx, http.MethodGet, c.URL+"/api/v1/settings/about", map[string]string{"X-Api-Key": c.APIKey}, nil)
	} else {
		body, err = a.do(ctx, http.MethodGet, c.URL+"/api/v3/system/status", map[string]string{"X-Api-Key": c.APIKey}, nil)
	}
	if err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) {
			switch {
			case (ue.Status == 401 || ue.Status == 403) && app == "sabnzbd":
				// SABnzbd answers a wrong key with 200 and an error, so a 401 is another app.
				return fmt.Errorf("that address isn't SABnzbd: check the port")
			case ue.Status == 401 || ue.Status == 403:
				return fmt.Errorf("%s refused the API key", title)
			case ue.Status == 404:
				return fmt.Errorf("%s answered, but not at that address: check the port and any URL base", title)
			case ue.Detail == "can't reach the app":
				return fmt.Errorf("can't reach %s at %s from the Silo server", title, c.URL)
			}
			return fmt.Errorf("%s answered HTTP %d", title, ue.Status)
		}
		return err
	}
	if app == "sabnzbd" {
		var v struct {
			Status any    `json:"status"`
			Error  string `json:"error"`
			Queue  any    `json:"queue"`
		}
		if json.Unmarshal(body, &v) != nil {
			return fmt.Errorf("that address answered, but not like SABnzbd")
		}
		if v.Error != "" {
			if strings.Contains(strings.ToLower(v.Error), "api key") {
				return fmt.Errorf("SABnzbd refused the API key (use the API key, not the NZB key)")
			}
			return fmt.Errorf("SABnzbd said: %s", v.Error)
		}
		if v.Queue == nil {
			return fmt.Errorf("that address answered, but not like SABnzbd")
		}
		return nil
	}
	if app == "seerr" {
		var v struct{ Version string `json:"version"` }
		if json.Unmarshal(body, &v) != nil || v.Version == "" {
			return fmt.Errorf("that address answered, but not like Seerr")
		}
		return nil
	}
	var st struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &st) != nil || st.Version == "" {
		return fmt.Errorf("that address answered, but not like %s", title)
	}
	if st.AppName != "" && !strings.EqualFold(st.AppName, title) {
		return fmt.Errorf("that address is %s, not %s", st.AppName, title)
	}
	return nil
}

func (a *App) conn(name string) Conn {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.apps[name]
}

// Read paths the TV may ask Radarr and Sonarr for (under /api/v3/).
var arrReadable = regexp.MustCompile(`^(system/status|health|movie|queue|history|calendar|wanted/missing|wanted/cutoff|diskspace)$`)

// Commands the TV may start.
var arrCommands = map[string]map[string]bool{
	"radarr": {"MissingMoviesSearch": true, "RefreshMovie": true, "CutOffUnmetMoviesSearch": true},
	"sonarr": {"MissingEpisodeSearch": true, "RefreshSeries": true, "CutoffUnmetEpisodeSearch": true},
}

var sabReadable = map[string]bool{"queue": true, "history": true, "status": true, "version": true, "server_stats": true}

type upstreamError struct {
	Status int
	Detail string
}

func (e *upstreamError) Error() string { return fmt.Sprintf("%d %s", e.Status, e.Detail) }

func (a *App) do(ctx context.Context, method, target string, header map[string]string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, &upstreamError{Status: 502, Detail: "can't reach the app"}
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &upstreamError{Status: resp.StatusCode, Detail: http.StatusText(resp.StatusCode)}
	}
	return out, nil
}

// arrGet reads /api/v3/<path> from Radarr or Sonarr with the query passed through.
func (a *App) arrGet(ctx context.Context, app, path string, q url.Values) ([]byte, error) {
	c := a.conn(app)
	if !c.ok() {
		return nil, &upstreamError{Status: 404, Detail: app + " is not set up"}
	}
	if !arrReadable.MatchString(path) {
		return nil, &upstreamError{Status: 400, Detail: "not allowed"}
	}
	q.Del("apikey")
	target := c.URL + "/api/v3/" + path
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	return a.do(ctx, http.MethodGet, target, map[string]string{"X-Api-Key": c.APIKey}, nil)
}

func (a *App) arrCommand(ctx context.Context, app, name string) ([]byte, error) {
	c := a.conn(app)
	if !c.ok() {
		return nil, &upstreamError{Status: 404, Detail: app + " is not set up"}
	}
	if !arrCommands[app][name] {
		return nil, &upstreamError{Status: 400, Detail: "not allowed"}
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	return a.do(ctx, http.MethodPost, c.URL+"/api/v3/command", map[string]string{"X-Api-Key": c.APIKey}, body)
}

// sab calls SABnzbd's API with a fixed set of modes.
func (a *App) sab(ctx context.Context, mode string, q url.Values, write bool) ([]byte, error) {
	c := a.conn("sabnzbd")
	if !c.ok() {
		return nil, &upstreamError{Status: 404, Detail: "sabnzbd is not set up"}
	}
	p := url.Values{}
	switch {
	case !write && sabReadable[mode]:
		for _, k := range []string{"limit", "start", "skip_dashboard"} {
			if v := q.Get(k); v != "" {
				p.Set(k, v)
			}
		}
	case write && (mode == "pause" || mode == "resume"):
	case write && mode == "speedlimit":
		v := q.Get("value")
		if !regexp.MustCompile(`^[0-9]{1,3}$`).MatchString(v) {
			return nil, &upstreamError{Status: 400, Detail: "bad value"}
		}
		mode = "config"
		p.Set("name", "speedlimit")
		p.Set("value", v)
	default:
		return nil, &upstreamError{Status: 400, Detail: "not allowed"}
	}
	p.Set("mode", mode)
	p.Set("output", "json")
	p.Set("apikey", c.APIKey)
	return a.do(ctx, http.MethodGet, c.URL+"/api?"+p.Encode(), nil, nil)
}

func (a *App) status() map[string]bool {
	out := map[string]bool{}
	for _, k := range []string{"radarr", "sonarr", "sabnzbd", "seerr"} {
		out[k] = a.conn(k).ok()
	}
	return out
}
