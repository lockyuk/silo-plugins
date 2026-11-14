package main

import (
	"context"
	"strings"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func TestArrProxyAddsKeyAndBlocksOtherPaths(t *testing.T) {
	var gotKey, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath, gotQuery = r.Header.Get("X-Api-Key"), r.URL.Path, r.URL.RawQuery
		w.Write([]byte(`{"version":"5.14"}`))
	}))
	defer srv.Close()
	a := newApp(hclog.NewNullLogger())
	a.apps["radarr"] = Conn{URL: srv.URL, APIKey: "k1"}
	s := &routeServer{app: a}
	q, _ := structpb.NewStruct(map[string]any{"pageSize": "5", "apikey": "evil"})
	r, _ := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/api/radarr/queue", Query: q})
	if r.StatusCode != 200 || gotKey != "k1" || gotPath != "/api/v3/queue" {
		t.Fatalf("status %d key %q path %q", r.StatusCode, gotKey, gotPath)
	}
	if v, _ := url.ParseQuery(gotQuery); v.Get("apikey") != "" || v.Get("pageSize") != "5" {
		t.Fatalf("query %q", gotQuery)
	}
	r, _ = s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/api/radarr/config/host"})
	if r.StatusCode != 400 {
		t.Fatalf("config path allowed: %d", r.StatusCode)
	}
	r, _ = s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/api/radarr/command", Body: []byte(`{"name":"DeleteEverything"}`)})
	if r.StatusCode != 400 {
		t.Fatalf("command allowed: %d", r.StatusCode)
	}
}

func TestUnsetAppIs404(t *testing.T) {
	s := &routeServer{app: newApp(hclog.NewNullLogger())}
	r, _ := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/api/sonarr/system/status"})
	if r.StatusCode != 404 {
		t.Fatalf("got %d", r.StatusCode)
	}
}

func TestSabModes(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	a := newApp(hclog.NewNullLogger())
	a.apps["sabnzbd"] = Conn{URL: srv.URL, APIKey: "s1"}
	s := &routeServer{app: a}
	q, _ := structpb.NewStruct(map[string]any{"value": "50"})
	r, _ := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/api/sabnzbd/speedlimit", Query: q})
	if r.StatusCode != 200 || got.Get("mode") != "config" || got.Get("name") != "speedlimit" || got.Get("value") != "50" || got.Get("apikey") != "s1" {
		t.Fatalf("%d %v", r.StatusCode, got)
	}
	r, _ = s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/api/sabnzbd/delete"})
	if r.StatusCode != 400 {
		t.Fatalf("delete allowed: %d", r.StatusCode)
	}
	r, _ = s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/api/sabnzbd/pause"})
	if r.StatusCode != 400 {
		t.Fatalf("pause via GET allowed: %d", r.StatusCode)
	}
}

func TestConnectionCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v3/system/status" && r.Header.Get("X-Api-Key") == "good":
			w.Write([]byte(`{"appName":"Radarr","version":"5.14.0"}`))
		case r.URL.Path == "/api/v3/system/status", r.URL.Path == "/v3/api":
			w.WriteHeader(401)
		case r.URL.Path == "/api" && r.URL.Query().Get("apikey") == "good":
			w.Write([]byte(`{"queue":{"slots":[]}}`))
		case r.URL.Path == "/api":
			w.Write([]byte(`{"status":false,"error":"API Key Incorrect"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	run := func(app string, c Conn) string {
		a := newApp(hclog.NewNullLogger())
		a.apps[app] = c
		s := &routeServer{app: a}
		q, _ := structpb.NewStruct(map[string]any{"key": app})
		r, _ := s.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/__silo/connection-check", Query: q})
		if r.StatusCode == 200 {
			return "ok"
		}
		return string(r.Body)
	}
	cases := []struct {
		app  string
		c    Conn
		want string
	}{
		{"radarr", Conn{URL: srv.URL, APIKey: "good"}, "ok"},
		{"radarr", Conn{URL: srv.URL, APIKey: "bad"}, "Radarr refused the API key"},
		{"sonarr", Conn{URL: srv.URL, APIKey: "good"}, "that address is Radarr, not Sonarr"},
		{"radarr", Conn{URL: "192.168.1.10:7878", APIKey: "good"}, "must start with http://"},
		{"radarr", Conn{URL: srv.URL}, "enter Radarr's API key"},
		{"radarr", Conn{URL: "http://127.0.0.1:1", APIKey: "good"}, "can't reach Radarr"},
		{"sabnzbd", Conn{URL: srv.URL, APIKey: "good"}, "ok"},
		{"sabnzbd", Conn{URL: srv.URL, APIKey: "bad"}, "SABnzbd refused the API key"},
		{"sabnzbd", Conn{URL: srv.URL + "/nope", APIKey: "good"}, "not at that address"},
		{"sabnzbd", Conn{URL: srv.URL + "/v3", APIKey: "good"}, "isn't SABnzbd"},
	}
	for _, c := range cases {
		if got := run(c.app, c.c); !strings.Contains(got, c.want) {
			t.Errorf("%s %+v: got %s, want %q", c.app, c.c, got, c.want)
		}
	}
}
