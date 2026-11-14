package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
)

var seerrChannels = []string{"email", "webpush", "discord", "telegram", "slack", "pushover", "gotify", "ntfy", "webhook"}

var seerrActionRe = regexp.MustCompile(`^request/([0-9]{1,9})/(approve|decline|delete)$`)

type seerrTitle struct {
	Title  string `json:"title"`
	Year   string `json:"year"`
	Poster string `json:"poster"`
}

// seerr serves the Requests tab: Seerr's list with titles filled in, its
// notification channels, and approve / decline / delete.
func (a *App) seerr(ctx context.Context, rest string, q url.Values, write bool, body []byte) ([]byte, error) {
	c := a.conn("seerr")
	if !c.ok() {
		return nil, &upstreamError{Status: 404, Detail: "seerr is not set up"}
	}
	hdr := map[string]string{"X-Api-Key": c.APIKey}
	switch {
	case !write && rest == "requests":
		p := url.Values{}
		take := "30"
		if v := q.Get("take"); regexp.MustCompile(`^[0-9]{1,3}$`).MatchString(v) {
			take = v
		}
		p.Set("take", take)
		p.Set("sort", "added")
		if v := q.Get("skip"); regexp.MustCompile(`^[0-9]{1,5}$`).MatchString(v) {
			p.Set("skip", v)
		}
		if v := q.Get("filter"); regexp.MustCompile(`^(all|pending|approved|processing|available|unavailable|failed|declined)$`).MatchString(v) {
			p.Set("filter", v)
		}
		raw, err := a.do(ctx, http.MethodGet, c.URL+"/api/v1/request?"+p.Encode(), hdr, nil)
		if err != nil {
			return nil, err
		}
		return a.seerrEnrich(ctx, c, raw), nil
	case !write && rest == "notifications":
		out := map[string]bool{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, ch := range seerrChannels {
			wg.Add(1)
			go func(ch string) {
				defer wg.Done()
				b, err := a.do(ctx, http.MethodGet, c.URL+"/api/v1/settings/notifications/"+ch, hdr, nil)
				var v struct{ Enabled bool `json:"enabled"` }
				on := err == nil && json.Unmarshal(b, &v) == nil && v.Enabled
				mu.Lock()
				out[ch] = on
				mu.Unlock()
			}(ch)
		}
		wg.Wait()
		return json.Marshal(out)
	case write:
		m := seerrActionRe.FindStringSubmatch(rest)
		if m == nil {
			return nil, &upstreamError{Status: 404, Detail: "not found"}
		}
		target := c.URL + "/api/v1/request/" + m[1]
		switch m[2] {
		case "delete":
			if _, err := a.do(ctx, http.MethodDelete, target, hdr, nil); err != nil {
				return nil, err
			}
			return []byte(`{"ok":true}`), nil
		case "approve":
			return a.do(ctx, http.MethodPost, target+"/approve", hdr, []byte("{}"))
		default:
			return a.do(ctx, http.MethodPost, target+"/decline", hdr, []byte("{}"))
		}
	}
	return nil, &upstreamError{Status: 400, Detail: "not allowed"}
}

// seerrEnrich adds title, year and poster to each request (Seerr's list only
// carries TMDB ids). On any failure the list is returned unchanged.
func (a *App) seerrEnrich(ctx context.Context, c Conn, raw []byte) []byte {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	var results []map[string]any
	if json.Unmarshal(doc["results"], &results) != nil {
		return raw
	}
	titles := make([]seerrTitle, len(results))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, r := range results {
		media, _ := r["media"].(map[string]any)
		tmdb, _ := media["tmdbId"].(float64)
		kind, _ := r["type"].(string)
		if tmdb == 0 || (kind != "movie" && kind != "tv") {
			continue
		}
		wg.Add(1)
		go func(i int, kind string, id int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			b, err := a.do(ctx, http.MethodGet, c.URL+"/api/v1/"+kind+"/"+strconv.Itoa(id), map[string]string{"X-Api-Key": c.APIKey}, nil)
			if err != nil {
				return
			}
			var v struct {
				Title        string `json:"title"`
				Name         string `json:"name"`
				ReleaseDate  string `json:"releaseDate"`
				FirstAirDate string `json:"firstAirDate"`
				PosterPath   string `json:"posterPath"`
			}
			if json.Unmarshal(b, &v) != nil {
				return
			}
			t := v.Title
			if t == "" {
				t = v.Name
			}
			d := v.ReleaseDate
			if d == "" {
				d = v.FirstAirDate
			}
			if len(d) >= 4 {
				d = d[:4]
			}
			titles[i] = seerrTitle{Title: t, Year: d, Poster: v.PosterPath}
		}(i, kind, int(tmdb))
	}
	wg.Wait()
	for i := range results {
		results[i]["title"] = titles[i].Title
		results[i]["year"] = titles[i].Year
		results[i]["poster"] = titles[i].Poster
	}
	b, err := json.Marshal(results)
	if err != nil {
		return raw
	}
	doc["results"] = b
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}
