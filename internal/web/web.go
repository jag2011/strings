// Package web serves the HTML pages and the HTTP API.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jag2011/strings/internal/ai"
	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	// TrustProxy takes the client IP from the last X-Forwarded-For entry,
	// which the load balancer appends. Only set it behind a proxy that does.
	TrustProxy bool
	// Base is the path the app is served under, like "/strings", or "" for
	// the root. See underBase.
	Base string

	// AI runs AI translation jobs; nil without an OpenRouter key.
	AI *ai.Runner
	// Models lists the models for AI users; nil offers a built-in list.
	Models *ai.Catalog

	store    *store.Store
	external *External // nil: no sign-in through an account server
	pages    map[string]*template.Template
	icons    *template.Template // for the favicon
	limiter  *limiter
	static   string // cache-busting suffix for static URLs
}

// New returns the server. external may be nil.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// tokens shortens a token count: 950, 12.3k, 4.1M.
func tokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}

func New(s *store.Store, external *External) (*Server, error) {
	srv := &Server{store: s, external: external, pages: map[string]*template.Template{}, limiter: newLimiter(10, 15*time.Minute),
		static: fmt.Sprint(time.Now().Unix())}
	funcs := template.FuncMap{
		"plural": plural,
		"query": func(kv ...string) template.URL {
			v := url.Values{}
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i+1] != "" {
					v.Set(kv[i], kv[i+1])
				}
			}
			if len(v) == 0 {
				return ""
			}
			return template.URL("?" + v.Encode())
		},
		"esc":      url.PathEscape,
		"static":   func(name string) string { return srv.Base + "/static/" + name + "?v=" + srv.static },
		"base":     func() string { return srv.Base },
		"date":     func(s string) string { return s[:min(len(s), 16)] + " UTC" },
		"langName": langName,
		"flag":     flag,
		"add":      func(a, b int) int { return a + b },
		"usd":      func(v float64) string { return fmt.Sprintf("$%.2f", v) },
		"tokens":   tokens,
		"diff":     diffText,
		// For the textarea: lang enables the browser's spell checker in the
		// right language, dir handles right-to-left scripts.
		"bcp47": func(code string) string { return strings.ReplaceAll(code, "_", "-") },
		"dir": func(code string) string {
			switch strings.Split(code, "_")[0] {
			case "ar", "he", "fa", "ur", "ps", "yi", "dv", "ckb":
				return "rtl"
			}
			return "ltr"
		},
	}
	srv.icons = template.Must(template.ParseFS(templateFS, "templates/_icons.html"))
	// Files starting with "_" hold templates shared by several pages.
	layout := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/_*.html"))
	names, _ := fs.Glob(templateFS, "templates/*.html")
	for _, n := range names {
		name := path.Base(n)
		if name == "layout.html" || strings.HasPrefix(name, "_") {
			continue
		}
		srv.pages[strings.TrimSuffix(name, ".html")] = template.Must(template.Must(layout.Clone()).ParseFS(templateFS, n))
	}
	return srv, nil
}

func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	page := func(pattern string, h func(http.ResponseWriter, *http.Request, *store.User) error) {
		mux.Handle(pattern, srv.page(h, false))
	}
	admin := func(pattern string, h func(http.ResponseWriter, *http.Request, *store.User) error) {
		mux.Handle(pattern, srv.page(h, true))
	}

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheForever(http.FileServerFS(static))))
	mux.HandleFunc("GET /health", srv.health)
	mux.HandleFunc("GET /favicon.svg", srv.favicon)
	mux.HandleFunc("GET /login", srv.loginPage)
	mux.HandleFunc("POST /login", srv.login)
	mux.HandleFunc("POST /logout", srv.logout)
	mux.HandleFunc("GET /invite/{token}", srv.invitePage)
	mux.HandleFunc("POST /invite/{token}", srv.invite)
	mux.HandleFunc("GET /auth/app", srv.externalPage)
	mux.HandleFunc("POST /auth/app", srv.externalSignIn)

	page("GET /{$}", srv.home)
	page("GET /account", srv.account)
	page("GET /u/{id}", srv.profile)
	page("GET /activity", srv.activity)
	page("GET /t/{lang}", srv.list)
	page("GET /t/{lang}/{key}", srv.editor)
	page("POST /t/{lang}/{key}", srv.save)
	page("POST /t/{lang}/{key}/revert/{id}", srv.revert)
	page("POST /t/{lang}/{key}/delete/{id}", srv.deleteVersion)
	page("POST /t/{lang}/{key}/suggestions/{id}/use", srv.useSuggestion)
	page("POST /t/{lang}/{key}/suggestions/{id}/dismiss", srv.dismissSuggestion)

	admin("GET /admin", srv.adminPage)
	admin("POST /admin/settings", srv.adminSettings)
	admin("POST /admin/languages", srv.adminAddLanguage)
	admin("POST /admin/languages/{lang}/remove", srv.adminRemoveLanguage)
	admin("POST /admin/import", srv.adminImport)
	admin("GET /admin/import/{id}", srv.adminReview)
	admin("POST /admin/import/{id}", srv.adminConfirmImport)
	admin("POST /admin/import/{id}/cancel", srv.adminCancelImport)
	admin("POST /admin/tokens", srv.adminCreateToken)
	admin("POST /admin/tokens/{id}/delete", srv.adminDeleteToken)
	admin("GET /admin/users", srv.adminUsers)
	admin("POST /admin/users", srv.adminCreateUser)
	admin("POST /admin/users/ai", srv.adminCreateAIUser)
	admin("GET /admin/ai", srv.aiPage)
	admin("POST /admin/ai", srv.aiTranslate)
	admin("GET /admin/ai/runs/{id}", srv.aiRun)
	admin("POST /admin/ai/runs/{id}/cancel", srv.aiCancel)
	admin("POST /admin/ai/runs/{id}/restart", srv.aiRestart)
	admin("POST /admin/users/{id}", srv.adminUpdateUser)
	admin("POST /admin/users/{id}/link", srv.adminUserLink)
	admin("GET /admin/users/{id}/merge", srv.adminMergePage)
	admin("POST /admin/users/{id}/merge", srv.adminMerge)
	page("GET /export/{file}", srv.exportFile)
	page("GET /export.zip", srv.exportZip)

	api := http.NewServeMux()
	api.HandleFunc("PUT /api/v1/source", srv.apiSource)
	api.HandleFunc("PUT /api/v1/translations/{lang}", srv.apiTranslations)
	api.HandleFunc("GET /api/v1/arb/{file}", srv.apiExport)
	api.HandleFunc("GET /api/v1/arb.zip", srv.apiExportZip)
	api.HandleFunc("GET /api/v1/stats", srv.apiStats)
	api.HandleFunc("GET /api/v1/contributors", srv.apiContributors)
	mux.Handle("/api/", srv.apiAuth(api))

	// Rejects cross-site form posts (Sec-Fetch-Site / Origin), so forms need
	// no CSRF tokens. Requests without those headers, like curl, pass.
	h := securityHeaders(http.NewCrossOriginProtection().Handler(mux))
	if srv.Base == "" {
		return h
	}
	return srv.underBase(h)
}

// underBase serves h under srv.Base, for sharing a host with other apps
// behind a proxy. Handlers keep using root paths: the prefix is stripped from
// requests and added to redirects and cookie paths here, and to links in
// templates with {{base}}. /health stays at the root for the load balancer.
func (srv *Server) underBase(h http.Handler) http.Handler {
	strip := http.StripPrefix(srv.Base, h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			h.ServeHTTP(w, r)
		case r.URL.Path == srv.Base:
			http.Redirect(w, r, srv.Base+"/", http.StatusMovedPermanently)
		case strings.HasPrefix(r.URL.Path, srv.Base+"/"):
			strip.ServeHTTP(&baseWriter{ResponseWriter: w, base: srv.Base}, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// baseWriter adds the base path to the Location header and to cookie paths.
type baseWriter struct {
	http.ResponseWriter
	base  string
	wrote bool
}

func (w *baseWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		h := w.Header()
		if loc := h.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
			h.Set("Location", w.base+loc)
		}
		for i, v := range h["Set-Cookie"] {
			if c, err := http.ParseSetCookie(v); err == nil && c.Path == "/" {
				c.Path = w.base
				h["Set-Cookie"][i] = c.String()
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *baseWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *baseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// health is the load balancer's health check: 200 while the database answers.
func (srv *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := srv.store.Ping(ctx); err != nil {
		log.Printf("health: %v", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintln(w, "ok")
}

// favicon serves the logo in the accent color.
func (srv *Server) favicon(w http.ResponseWriter, r *http.Request) {
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "max-age=3600")
	srv.icons.ExecuteTemplate(w, "logo", set["accent"])
}

// pageCtx is passed to every template.
type pageCtx struct {
	Title    string
	User     *store.User
	Settings map[string]string
	Flash    string
	Nav      string
	Data     any
}

func (srv *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, u *store.User, title string, data any) error {
	set, err := srv.store.Settings(r.Context())
	if err != nil {
		return err
	}
	ctx := pageCtx{Title: title, User: u, Settings: set, Flash: takeFlash(w, r), Data: data, Nav: name}
	var buf strings.Builder
	if err := srv.pages[name].ExecuteTemplate(&buf, "layout.html", ctx); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, err = w.Write([]byte(buf.String()))
	return err
}

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func notFound() error             { return httpError{http.StatusNotFound, "Not found."} }
func badRequest(msg string) error { return httpError{http.StatusBadRequest, msg} }

// page wraps handlers that need a logged-in user, and renders errors.
func (srv *Server) page(h func(http.ResponseWriter, *http.Request, *store.User) error, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := srv.currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if adminOnly && !u.IsAdmin() {
			srv.renderError(w, r, u, httpError{http.StatusForbidden, "Only admins can open this page."})
			return
		}
		if err := h(w, r, u); err != nil {
			srv.renderError(w, r, u, err)
		}
	})
}

func (srv *Server) renderError(w http.ResponseWriter, r *http.Request, u *store.User, err error) {
	var he httpError
	if errors.Is(err, store.ErrNotFound) {
		he = httpError{http.StatusNotFound, "Not found."}
	} else if errors.Is(err, store.ErrApproved) {
		he = httpError{http.StatusConflict, "This translation is approved. Remove the approval first."}
	} else if errors.Is(err, store.ErrForbidden) {
		he = httpError{http.StatusForbidden, "You can't do this."}
	} else if !errors.As(err, &he) {
		log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
		he = httpError{http.StatusInternalServerError, "Something went wrong."}
	}
	if rerr := srv.render(w, r, he.status, "error", u, "Error", he.msg); rerr != nil {
		http.Error(w, he.msg, he.status)
	}
}

const flashCookie = "strings_flash"

func setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(msg), Path: "/", MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1})
	msg, _ := url.QueryUnescape(c.Value)
	return msg
}

func redirect(w http.ResponseWriter, r *http.Request, to, flash string) error {
	if flash != "" {
		setFlash(w, flash)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
	return nil
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}

// limiter counts failed attempts per key in a sliding window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-l.window)
	h := l.hits[key]
	for len(h) > 0 && h[0].Before(cut) {
		h = h[1:]
	}
	if len(h) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = h
	}
	return len(h) >= l.max
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[key] = append(l.hits[key], time.Now())
}

func (srv *Server) requireLang(ctx context.Context, lang string) error {
	if !arb.ValidLocale(lang) {
		return notFound()
	}
	ok, err := srv.store.HasLanguage(ctx, lang)
	if err != nil {
		return err
	}
	if !ok {
		return notFound()
	}
	return nil
}
