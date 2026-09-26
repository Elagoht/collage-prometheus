// Package prometheus is a collage plugin that exports the framework's metrics to
// Prometheus, and serves them at /metrics.
//
//	m := prometheus.NewMetrics(prometheus.Options{Namespace: "collage"})
//	app, err := collage.New(&collage.Config{
//		Observability: collage.ObservabilityConfig{Metrics: m},
//		Plugins:       []collage.Plugin{m},
//	})
//
// The one value is both halves, and it needs both lines. A plugin cannot set the
// application's Config, so only the application can hand collage its Metrics; and
// only a plugin can serve a route and learn the site's pages, which is what keeps
// the HTTP metric's route label bounded.
//
// # Labels
//
// Every label takes its values from a set the application's code fixes: page and
// fragment names, cache event kinds, status classes. None comes from a request. A
// raw path as a label is a new time series for every /blog/whatever a crawler
// invents, and a Prometheus server holding a million series for one histogram is
// one that has stopped answering. So the HTTP metric labels a request with the
// name of the page or document whose pattern its path matches, and "other" when
// none does.
package prometheus

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/prometheus"

// OtherRoute is the route label of a request whose path matches no page, no
// document the plugin has learned and no prefix in Options.Routes: a 404, a
// handler mounted by the application, a static asset.
const OtherRoute = "other"

// Options configures the plugin.
type Options struct {
	// Namespace prefixes every metric name: "collage" gives
	// collage_render_duration_seconds. Default "collage". It can only be set
	// from Go: the metrics are registered when NewMetrics returns, before any
	// configuration is read.
	Namespace string `json:"-"`
	// Registry is where the metrics are registered and what /metrics serves.
	// Unset, it is Prometheus's default registry, so metrics the application
	// registers with promauto and the Go runtime's own are served alongside.
	// It can only be set from Go.
	Registry *prom.Registry `json:"-"`
	// Buckets are the histograms' upper bounds, in seconds. Default
	// prometheus.DefBuckets, 5ms to 10s. It can only be set from Go.
	Buckets []float64 `json:"-"`
	// Path is where the metrics are served. Default "/metrics"; "-" serves
	// them nowhere, for an application that serves its registry itself.
	Path string `json:"path"`
	// Token, when set, is required as "Authorization: Bearer <token>" on the
	// metrics path. Without it anyone who can reach the site can read its
	// metrics — page names, error rates, traffic.
	Token string `json:"token"`
	// Routes are path prefixes labelled as themselves: "/api/" labels every
	// request under it "/api/". For what the plugin cannot learn from the
	// application's pages — a handler of the application's own, a mount.
	Routes []string `json:"routes"`
}

// Plugin is the application's collage.Metrics and the plugin that serves them.
type Plugin struct {
	opts     Options
	gatherer prom.Gatherer
	// regErr is a registration that failed in NewMetrics, which cannot return
	// it; Init does, so the application does not start without its metrics.
	regErr error

	render        *prom.HistogramVec
	fragment      *prom.HistogramVec
	cacheEvents   *prom.CounterVec
	http          *prom.HistogramVec
	invalidations prom.Counter
	invalidated   prom.Counter

	host   collage.Host
	routes atomic.Pointer[matcher]
	// learnMu serialises learning a document's path, and tried remembers every
	// name already looked up, found or not: the names are registered ones, so
	// the set is bounded, and each is asked about once.
	learnMu sync.Mutex
	tried   sync.Map // name → struct{}
}

// New returns the plugin, with its metrics registered. See NewMetrics.
func New(opts Options) *Plugin { return NewMetrics(opts) }

// NewMetrics returns the plugin with its metrics registered: hand the same value
// to Config.Observability.Metrics and to Config.Plugins.
func NewMetrics(opts Options) *Plugin {
	if opts.Namespace == "" {
		opts.Namespace = "collage"
	}
	if len(opts.Buckets) == 0 {
		opts.Buckets = prom.DefBuckets
	}
	var reg prom.Registerer = prom.DefaultRegisterer
	var gatherer prom.Gatherer = prom.DefaultGatherer
	if opts.Registry != nil {
		reg, gatherer = opts.Registry, opts.Registry
	}
	ns := opts.Namespace
	p := &Plugin{
		opts:     opts,
		gatherer: gatherer,
		render: prom.NewHistogramVec(prom.HistogramOpts{
			Namespace: ns, Name: "render_duration_seconds",
			Help:    "How long a page or document took to render, or to be read from the cache when cache_hit is true.",
			Buckets: opts.Buckets,
		}, []string{"page", "cache_hit"}),
		fragment: prom.NewHistogramVec(prom.HistogramOpts{
			Namespace: ns, Name: "fragment_duration_seconds",
			Help:    "How long a fragment's data handler took, by page, fragment and outcome.",
			Buckets: opts.Buckets,
		}, []string{"page", "fragment", "outcome"}),
		cacheEvents: prom.NewCounterVec(prom.CounterOpts{
			Namespace: ns, Name: "cache_events_total",
			Help: "Cache operations, by event: hit, miss, set, evict, invalidate, coalesced.",
		}, []string{"event"}),
		http: prom.NewHistogramVec(prom.HistogramOpts{
			Namespace: ns, Name: "http_request_duration_seconds",
			Help:    "How long a response took, by route (a page or document name, a configured prefix, or \"other\") and status class.",
			Buckets: opts.Buckets,
		}, []string{"route", "status"}),
		invalidations: prom.NewCounter(prom.CounterOpts{
			Namespace: ns, Name: "invalidations_total",
			Help: "Calls to invalidate cache entries by tag.",
		}),
		invalidated: prom.NewCounter(prom.CounterOpts{
			Namespace: ns, Name: "invalidated_keys_total",
			Help: "Cache keys invalidations issued a removal for; an upper bound on live entries removed.",
		}),
	}
	for _, c := range []prom.Collector{p.render, p.fragment, p.cacheEvents, p.http, p.invalidations, p.invalidated} {
		if err := reg.Register(c); err != nil {
			p.regErr = errors.Join(p.regErr, err)
		}
	}
	p.routes.Store(&matcher{})
	return p
}

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.1.0" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var (
	_ collage.Plugin  = (*Plugin)(nil)
	_ collage.Metrics = (*Plugin)(nil)
)

var tokenChars = regexp.MustCompile(`^[\x21-\x7e]+$`)

// Init reads the configuration, learns the pages' patterns for the route label and
// serves the metrics.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if p.regErr != nil {
		return fmt.Errorf("prometheus: registering the metrics: %w", p.regErr)
	}
	if err := host.Config(&p.opts); err != nil {
		return err
	}
	if p.opts.Path == "" {
		p.opts.Path = "/metrics"
	}
	if p.opts.Path != "-" && !strings.HasPrefix(p.opts.Path, "/") {
		return fmt.Errorf("prometheus: path %q must begin with /", p.opts.Path)
	}
	if p.opts.Token != "" && !tokenChars.MatchString(p.opts.Token) {
		return errors.New("prometheus: the token must be printable ASCII without spaces, as a header carries it")
	}
	for _, prefix := range p.opts.Routes {
		if !strings.HasPrefix(prefix, "/") {
			return fmt.Errorf("prometheus: route %q must begin with /", prefix)
		}
	}
	p.host = host

	_, locales := host.Locales()
	m := &matcher{locales: locales, prefixes: slices.Clone(p.opts.Routes)}
	for _, page := range host.Pages() {
		for _, pattern := range page.Paths {
			m.add(pattern, page.Name)
		}
	}
	if p.opts.Path != "-" {
		m.exact = p.opts.Path
	}
	m.sort()
	p.routes.Store(m)

	if p.opts.Path == "-" {
		return nil
	}
	return host.Use(p.middleware)
}

// middleware answers the metrics path and passes everything else on. It is
// middleware rather than Host.Handle because Handle serves a prefix ending in
// "/", and Prometheus scrapes /metrics, without one, unless told otherwise.
func (p *Plugin) middleware(next http.Handler) http.Handler {
	metrics := promhttp.HandlerFor(p.gatherer, promhttp.HandlerOpts{})
	want := []byte("Bearer " + p.opts.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != p.opts.Path {
			next.ServeHTTP(w, r)
			return
		}
		if p.opts.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// A scrape is never a page anyone else should be served from a cache.
		w.Header().Set("Cache-Control", "no-store")
		metrics.ServeHTTP(w, r)
	})
}

// RenderDuration observes a render, or a cache read when cacheHit is true.
func (p *Plugin) RenderDuration(_ context.Context, page string, d time.Duration, cacheHit bool) {
	p.learn(page)
	p.render.WithLabelValues(page, strconv.FormatBool(cacheHit)).Observe(d.Seconds())
}

// FragmentDuration observes a fragment's data fetch.
func (p *Plugin) FragmentDuration(_ context.Context, page, fragment string, d time.Duration, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	p.fragment.WithLabelValues(page, fragment, outcome).Observe(d.Seconds())
}

// CacheEvent counts a cache operation. The key is not a label: it is one per
// cached URL.
func (p *Plugin) CacheEvent(_ context.Context, event collage.CacheEvent, _ string) {
	p.cacheEvents.WithLabelValues(string(event)).Inc()
}

// HTTPResponse observes a response, labelled by the route its path matched.
func (p *Plugin) HTTPResponse(_ context.Context, status int, path string, d time.Duration) {
	p.http.WithLabelValues(p.routes.Load().match(path), statusClass(status)).Observe(d.Seconds())
}

// Invalidation counts an invalidation and the keys it reached. The tags are not a
// label: an application may tag by record ID.
func (p *Plugin) Invalidation(_ context.Context, _ []string, keys int) {
	p.invalidations.Inc()
	p.invalidated.Add(float64(keys))
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return strconv.Itoa(status/100) + "xx"
}

// learn adds a document's path to the route label's patterns the first time the
// document is rendered. Host has no Documents method, so the plugin cannot list
// them at Init; but RenderDuration names every document that renders, before its
// response is reported, and Host.URL turns a name into its path. A document whose
// path has parameters cannot be built without them and stays "other".
func (p *Plugin) learn(name string) {
	if p.host == nil {
		return
	}
	if _, seen := p.tried.Load(name); seen {
		return
	}
	p.learnMu.Lock()
	defer p.learnMu.Unlock()
	if _, seen := p.tried.LoadOrStore(name, struct{}{}); seen {
		return
	}
	current := p.routes.Load()
	if current.names[name] {
		return
	}
	def, _ := p.host.Locales()
	path, err := p.host.URL(name, def, nil)
	if err != nil {
		return
	}
	next := current.clone()
	next.add(path, name)
	next.sort()
	p.routes.Store(next)
}
