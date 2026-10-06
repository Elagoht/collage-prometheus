package prometheus_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	prometheus "github.com/Elagoht/collage-prometheus"
	"github.com/Elagoht/collage/pkg/collage"
	prom "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func site(t *testing.T, m *prometheus.Plugin, pluginConfig string) *collage.App {
	t.Helper()
	cfg := &collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html":    {Data: []byte(`<p>page</p>`)},
			"t/fail.html": {Data: []byte(`<p>fails</p>`)},
		}, Root: "t"},
		Cache:         collage.CacheConfig{Enabled: true, Type: "memory", DefaultTTL: time.Hour},
		Observability: collage.ObservabilityConfig{Metrics: m},
		Plugins:       []collage.Plugin{m},
	}
	if pluginConfig != "" {
		cfg.PluginConfig = map[string]json.RawMessage{prometheus.Name: json.RawMessage(pluginConfig)}
	}
	app, err := collage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pages := []*collage.Page{
		collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Static().WithDependency("home").Build(),
		collage.NewPage("post").WithContent(collage.NewFragment("post", "p.html").Build()).WithPath("en", "/blog/{slug}").Static().Build(),
		collage.NewPage("docs").WithContent(collage.NewFragment("docs", "p.html").Build()).WithPath("en", "/docs/{rest...}").Static().Build(),
		collage.NewPage("broken").WithContent(collage.NewFragment("broken", "fail.html").WithData(collage.Load(
			func(context.Context, *collage.RenderContext) (string, error) {
				return "", errors.New("backend down")
			})).Build()).WithPath("en", "/broken").Build(),
	}
	for _, p := range pages {
		if err := app.RegisterPage(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.RegisterDocument(collage.NewDocument("robots", "text/plain").AtRoot("/robots.txt").WithBody([]byte("User-agent: *\n")).Build()); err != nil {
		t.Fatal(err)
	}
	if err := app.Mount("/static/", fstest.MapFS{"site.css": {Data: []byte("p{}")}}); err != nil {
		t.Fatal(err)
	}
	if err := app.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})); err != nil {
		t.Fatal(err)
	}
	return app
}

func get(h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// samples returns, for every series of the named metric, its labels and its count:
// a counter's value, or a histogram's number of observations.
func samples(t *testing.T, reg *prom.Registry, name string) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			out[labels(m)] = value(m)
		}
	}
	return out
}

func labels(m *dto.Metric) string {
	var parts []string
	for _, l := range m.GetLabel() {
		parts = append(parts, l.GetName()+"="+l.GetValue())
	}
	return strings.Join(parts, ",")
}

func value(m *dto.Metric) float64 {
	switch {
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	}
	return 0
}

func TestRenderAndCacheMetrics(t *testing.T) {
	reg := prom.NewRegistry()
	m := prometheus.NewMetrics(prometheus.Options{Registry: reg})
	h := site(t, m, "").Handler()
	get(h, "/")
	get(h, "/")
	get(h, "/broken")

	render := samples(t, reg, "collage_render_duration_seconds")
	if render["cache_hit=false,page=home"] != 1 || render["cache_hit=true,page=home"] != 1 {
		t.Errorf("render = %v", render)
	}
	events := samples(t, reg, "collage_cache_events_total")
	if events["event=hit"] != 1 || events["event=miss"] != 1 || events["event=set"] != 1 {
		t.Errorf("cache events = %v", events)
	}
	fragments := samples(t, reg, "collage_fragment_duration_seconds")
	if fragments["fragment=broken,outcome=error,page=broken"] != 1 {
		t.Errorf("fragments = %v", fragments)
	}
}

// A request is labelled by the route collage resolved it to — never by its path,
// which would be a new series for every URL a crawler invents.
func TestRouteLabelsAreBounded(t *testing.T) {
	reg := prom.NewRegistry()
	m := prometheus.NewMetrics(prometheus.Options{Registry: reg})
	h := site(t, m, "").Handler()
	for _, path := range []string{"/", "/blog/a", "/blog/b", "/blog/b/", "/docs/a/b/c", "/robots.txt", "/robots.txt",
		"/nope", "/blog/a/b", "/api/users/7", "/api/users/8", "/static/site.css", "/static/missing.css", "/metrics"} {
		get(h, path)
	}
	got := samples(t, reg, "collage_http_request_duration_seconds")
	want := map[string]float64{
		"route=page:home,status=2xx":        1,
		"route=page:post,status=2xx":        2,
		"route=other,status=3xx":            1, // the trailing slash redirects before routing
		"route=page:docs,status=2xx":        1,
		"route=document:robots,status=2xx":  2,
		"route=other,status=4xx":            2,
		"route=handler:/api/,status=2xx":    2,
		"route=mount:/static/,status=2xx":   1,
		"route=mount:/static/,status=4xx":   1,
		"route=handler:/metrics,status=2xx": 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("series = %v, want exactly %v", got, want)
	}
}

func TestInvalidations(t *testing.T) {
	reg := prom.NewRegistry()
	m := prometheus.NewMetrics(prometheus.Options{Registry: reg, Namespace: "site"})
	app := site(t, m, "")
	get(app.Handler(), "/")
	if err := app.InvalidateTags(context.Background(), "home"); err != nil {
		t.Fatal(err)
	}
	if got := samples(t, reg, "site_invalidations_total")[""]; got != 1 {
		t.Errorf("invalidations = %v", got)
	}
	if got := samples(t, reg, "site_invalidated_keys_total")[""]; got != 1 {
		t.Errorf("invalidated keys = %v", got)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	reg := prom.NewRegistry()
	h := site(t, prometheus.NewMetrics(prometheus.Options{Registry: reg}), "").Handler()
	get(h, "/")
	rec := get(h, "/metrics")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `collage_render_duration_seconds_count{cache_hit="false",page="home"} 1`) {
		t.Errorf("/metrics = %d\n%s", rec.Code, rec.Body.String())
	}
	if got := get(h, "/metrics/more").Code; got != http.StatusNotFound { // only the exact path
		t.Errorf("/metrics/more = %d", got)
	}
}

func TestTokenAndPath(t *testing.T) {
	reg := prom.NewRegistry()
	h := site(t, prometheus.NewMetrics(prometheus.Options{Registry: reg}), `{"path": "/_metrics", "token": "s3cret"}`).Handler()
	if rec := get(h, "/_metrics"); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("without a token: %d %v", rec.Code, rec.Header())
	}
	if got := get(h, "/_metrics", "Authorization", "Bearer wrong").Code; got != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", got)
	}
	if got := get(h, "/_metrics", "Authorization", "Bearer s3cret").Code; got != http.StatusOK {
		t.Errorf("right token: %d", got)
	}
	if got := get(h, "/metrics").Code; got != http.StatusNotFound {
		t.Errorf("/metrics after moving it: %d", got)
	}
}

func TestPathDashServesNothing(t *testing.T) {
	h := site(t, prometheus.NewMetrics(prometheus.Options{Registry: prom.NewRegistry()}), `{"path": "-"}`).Handler()
	if got := get(h, "/metrics").Code; got != http.StatusNotFound {
		t.Errorf("/metrics = %d", got)
	}
}

// A misconfigured plugin, or metrics that could not be registered, stop the
// application from starting.
func TestMisconfigurationStopsStartup(t *testing.T) {
	for name, tc := range map[string]struct {
		config string
		reg    func() *prom.Registry
	}{
		"relative path": {config: `{"path": "metrics"}`},
		"prefix path":   {config: `{"path": "/metrics/"}`},
		// Served with Host.Handle, the path cannot quietly hide a page.
		"a page's path": {config: `{"path": "/broken"}`},
		"bad token":     {config: `{"token": "has space"}`},
		"registered twice": {reg: func() *prom.Registry {
			reg := prom.NewRegistry()
			prometheus.NewMetrics(prometheus.Options{Registry: reg})
			return reg
		}},
	} {
		t.Run(name, func(t *testing.T) {
			reg := prom.NewRegistry()
			if tc.reg != nil {
				reg = tc.reg()
			}
			if got := get(site(t, prometheus.NewMetrics(prometheus.Options{Registry: reg}), tc.config).Handler(), "/").Code; got != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", got)
			}
		})
	}
}

// A page is one route in every locale.
func TestLocalesShareARoute(t *testing.T) {
	reg := prom.NewRegistry()
	m := prometheus.NewMetrics(prometheus.Options{Registry: reg})
	app, err := collage.New(&collage.Config{
		Server:        collage.ServerConfig{Host: "localhost", Port: 3000},
		Template:      collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>x</p>`)}}, Root: "t"},
		Locale:        collage.LocaleConfig{Default: "en", Supported: []string{"en", "tr"}},
		Observability: collage.ObservabilityConfig{Metrics: m},
		Plugins:       []collage.Plugin{m},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.RegisterPage(collage.NewPage("about").WithContent(collage.NewFragment("about", "p.html").Build()).
		WithPath("en", "/about").WithPath("tr", "/hakkinda").Build()); err != nil {
		t.Fatal(err)
	}
	get(app.Handler(), "/tr/hakkinda")
	get(app.Handler(), "/about")
	if got := samples(t, reg, "collage_http_request_duration_seconds")["route=page:about,status=2xx"]; got != 2 {
		t.Errorf("series = %v", samples(t, reg, "collage_http_request_duration_seconds"))
	}
}
