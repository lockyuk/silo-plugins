package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

type routeServer struct {
	pluginv1.UnimplementedHttpRoutesServer
	app *App
}

// Routes (all admin-only, enforced by Silo from the manifest):
//
//	GET  /api/status                      which apps are set up
//	GET  /api/radarr/<path>?<query>       Radarr /api/v3/<path> (read-only list)
//	GET  /api/sonarr/<path>?<query>       Sonarr /api/v3/<path>
//	POST /api/radarr/command  {"name"}    start an allowed command
//	POST /api/sonarr/command  {"name"}
//	GET  /api/sabnzbd/<mode>?limit=       queue, history, status, version, server_stats
//	GET  /api/seerr/requests?filter=&take=&skip=   requests, with titles and posters
//	GET  /api/seerr/notifications                  which Seerr notification channels are on
//	POST /api/seerr/request/<id>/<approve|decline|delete>
//	POST /api/sabnzbd/<pause|resume|speedlimit>?value=
func (s *routeServer) Handle(ctx context.Context, in *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	post := strings.EqualFold(in.GetMethod(), "POST")
	q := queryValues(in)
	a := s.app
	// Silo's "Test" button: only Silo can call this path; it is not one of the
	// manifest's routes, so viewers can't reach it.
	if in.GetPath() == "/__silo/connection-check" && post {
		if err := a.check(ctx, q.Get("key")); err != nil {
			return errResp(422, err.Error()), nil
		}
		return jsonResp(200, map[string]bool{"ok": true}), nil
	}
	path := strings.TrimPrefix(in.GetPath(), "/api/")

	if path == "status" && !post {
		return jsonResp(200, a.status()), nil
	}
	app, rest, _ := strings.Cut(path, "/")
	switch app {
	case "radarr", "sonarr":
		if post {
			if rest != "command" {
				return errResp(404, "not found"), nil
			}
			var body struct{ Name string `json:"name"` }
			_ = json.Unmarshal(in.GetBody(), &body)
			if body.Name == "" {
				body.Name = q.Get("name")
			}
			out, err := a.arrCommand(ctx, app, body.Name)
			return passthrough(out, err), nil
		}
		out, err := a.arrGet(ctx, app, rest, q)
		return passthrough(out, err), nil
	case "seerr":
		out, err := a.seerr(ctx, rest, q, post, in.GetBody())
		return passthrough(out, err), nil
	case "sabnzbd":
		out, err := a.sab(ctx, rest, q, post)
		return passthrough(out, err), nil
	}
	return errResp(404, "not found"), nil
}

func passthrough(body []byte, err error) *pluginv1.HandleHTTPResponse {
	if err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) {
			status := ue.Status
			if status == 401 || status == 403 {
				return errResp(502, "the app refused the API key (check the plugin settings)")
			}
			return errResp(status, ue.Detail)
		}
		return errResp(502, err.Error())
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       body,
	}
}

func queryValues(in *pluginv1.HandleHTTPRequest) url.Values {
	out := url.Values{}
	if in.GetQuery() == nil {
		return out
	}
	for k, v := range in.GetQuery().AsMap() {
		switch t := v.(type) {
		case string:
			out.Set(k, t)
		case []any:
			for _, x := range t {
				if s, ok := x.(string); ok {
					out.Add(k, s)
				}
			}
		}
	}
	return out
}

func jsonResp(status int, v any) *pluginv1.HandleHTTPResponse {
	body, _ := json.Marshal(v)
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       body,
	}
}

func errResp(status int, msg string) *pluginv1.HandleHTTPResponse {
	return jsonResp(status, map[string]string{"error": msg})
}
