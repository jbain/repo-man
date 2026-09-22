// Package web serves the dashboard: a server-rendered repository tree plus a
// small JSON API for the three write actions and for live refresh.
//
// The tree renders fully server-side, so the page is useful with JavaScript
// disabled. The script layer only re-fetches the tree fragment and drives the
// action forms; it is an enhancement, not a requirement.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jbain/repo-man/internal/actions"
	"github.com/jbain/repo-man/internal/auth"
	"github.com/jbain/repo-man/internal/config"
	"github.com/jbain/repo-man/internal/index"
	"github.com/jbain/repo-man/internal/jobs"
	"github.com/jbain/repo-man/internal/model"
	"github.com/jbain/repo-man/internal/safepath"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// maxBodyBytes caps request bodies. Every request this service accepts is a
// handful of short strings, so anything larger is a mistake or an attack.
const maxBodyBytes = 64 << 10

// Server wires the index, the action service and the authenticator into an
// http.Handler.
type Server struct {
	cfg  *config.Config
	ix   *index.Index
	act  *actions.Service
	reg  *jobs.Registry
	auth *auth.Authenticator
	log  *slog.Logger
	tpl  *template.Template
}

// New returns a Server. It fails if the embedded templates do not parse, which
// can only happen if the binary was built with a broken template.
func New(cfg *config.Config, ix *index.Index, act *actions.Service, reg *jobs.Registry, a *auth.Authenticator, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	tpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing templates: %w", err)
	}
	return &Server{cfg: cfg, ix: ix, act: act, reg: reg, auth: a, log: log, tpl: tpl}, nil
}

// Handler returns the fully routed handler, with authentication applied to
// everything except the login endpoints and the static assets the login page
// itself needs.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /fragment/tree", s.handleTreeFragment)
	mux.HandleFunc("GET /api/tree", s.handleAPITree)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/fetch", s.handleFetchRepo)
	mux.HandleFunc("POST /api/pull", s.handlePullRepo)
	mux.HandleFunc("POST /api/init", s.handleInit)
	mux.HandleFunc("POST /api/clone", s.handleClone)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJob)

	// Everything above requires a session.
	protected := s.auth.Require(mux)

	// The login page and the assets it references must stay reachable while
	// unauthenticated, so they are routed outside the protected mux.
	// /logout belongs here too, not behind Require: its whole job is to drop
	// a session that might already be invalid, expired, or absent (e.g. after
	// a process restart, which logs everyone out), and auth.Logout already
	// tolerates a missing/invalid cookie gracefully. Gating it would mean a
	// browser holding a stale cookie gets redirected to /login instead of a
	// clean "signed out" response, and never has the stale cookie cleared.
	public := http.NewServeMux()
	public.HandleFunc("GET /login", s.handleLoginPage)
	public.HandleFunc("POST /login", s.auth.Login)
	public.HandleFunc("POST /logout", s.auth.Logout)
	public.Handle("GET /static/", s.staticHandler())
	public.Handle("/", protected)

	return securityHeaders(public)
}

// securityHeaders sets the handful of headers that matter for a single-tenant
// app serving only its own assets. The CSP is strict because the page loads
// nothing from anywhere else: no CDN, no inline event handlers, no eval.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) staticHandler() http.Handler {
	fsys := http.FileServerFS(staticFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The assets are embedded in the binary and change only when the
		// binary does, but they are also tiny; a short cache keeps a reload
		// from being stale after an upgrade.
		w.Header().Set("Cache-Control", "public, max-age=300")
		fsys.ServeHTTP(w, r)
	})
}

// pageData is the template contract for the full page.
type pageData struct {
	Title       string
	Root        *model.Node
	Snapshot    *index.Snapshot
	Jobs        []jobs.Job
	Config      *config.Config
	AuthEnabled bool
	Now         time.Time
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	snap := s.ix.Snapshot()
	s.render(w, "page.html", pageData{
		Title:       "repo-man",
		Root:        snap.Root,
		Snapshot:    snap,
		Jobs:        s.reg.List(),
		Config:      s.cfg,
		AuthEnabled: s.auth.Enabled(),
		Now:         time.Now(),
	})
}

// handleTreeFragment renders just the parts of the page that change, so the
// script layer can swap them in without a full reload and without rebuilding
// the DOM in JavaScript.
func (s *Server) handleTreeFragment(w http.ResponseWriter, r *http.Request) {
	snap := s.ix.Snapshot()
	s.render(w, "fragment.html", pageData{
		Root:     snap.Root,
		Snapshot: snap,
		Jobs:     s.reg.List(),
		Config:   s.cfg,
		Now:      time.Now(),
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	// Render to a buffer first: a template error halfway through would
	// otherwise emit a 200 with a truncated page and no way to signal failure.
	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("template render failed", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, buf.String())
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Enabled() || s.auth.Authenticated(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var msg string
	switch r.URL.Query().Get("error") {
	case "invalid":
		msg = "Incorrect passphrase."
	case "locked":
		msg = "Too many failed attempts. Try again later."
	}
	data := struct {
		Title string
		Error string
		Next  string
	}{Title: "repo-man · sign in", Error: msg, Next: r.URL.Query().Get("next")}

	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "login.html", data); err != nil {
		s.log.Error("template render failed", "template", "login.html", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, buf.String())
}

// --- JSON API ---------------------------------------------------------------

func (s *Server) handleAPITree(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ix.Snapshot())
}

// handleRefresh triggers a rescan, or a full fetch round with ?fetch=1. Both
// are asynchronous: the request returns as soon as the work is queued, and the
// UI picks up the result on its next poll.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("fetch") == "1" {
		s.ix.FetchNow()
	} else {
		s.ix.ScanNow()
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

type fetchRequest struct {
	Path string `json:"path"` // root-relative path of the checkout
}

// handleFetchRepo fetches a single checkout and waits for the result, so the
// UI can report success or a concrete failure (bad credentials, no network)
// rather than leaving the user guessing.
func (s *Server) handleFetchRepo(w http.ResponseWriter, r *http.Request) {
	var req fetchRequest
	if !decode(w, r, &req) {
		return
	}
	abs, _, err := safepath.ResolveExisting(s.cfg.Root, req.Path, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.FetchTimeout)
	defer cancel()
	if err := s.ix.FetchRepo(ctx, abs); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "fetched"})
}

type pullRequest struct {
	Path string `json:"path"` // root-relative path of the checkout
}

// handlePullRepo fetches and fast-forwards a single checkout's checked-out
// branch and waits for the result, mirroring handleFetchRepo.
func (s *Server) handlePullRepo(w http.ResponseWriter, r *http.Request) {
	var req pullRequest
	if !decode(w, r, &req) {
		return
	}
	abs, _, err := safepath.ResolveExisting(s.cfg.Root, req.Path, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.FetchTimeout)
	defer cancel()
	if err := s.ix.PullRepo(ctx, abs); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "pulled"})
}

type initRequest struct {
	Parent string `json:"parent"` // root-relative directory to create the repo in
	Name   string `json:"name"`
	Branch string `json:"branch"`
}

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	if !decode(w, r, &req) {
		return
	}
	rel, err := s.act.InitRepo(r.Context(), req.Parent, strings.TrimSpace(req.Name), strings.TrimSpace(req.Branch))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": rel})
}

type cloneRequest struct {
	URL  string `json:"url"`
	Dest string `json:"dest"`
}

func (s *Server) handleClone(w http.ResponseWriter, r *http.Request) {
	var req cloneRequest
	if !decode(w, r, &req) {
		return
	}
	job, err := s.act.Clone(actions.CloneRequest{URL: strings.TrimSpace(req.URL), Dest: strings.TrimSpace(req.Dest)})
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reg.List())
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.reg.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no such job"))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// statusFor maps the action layer's sentinel errors onto status codes. An
// unrecognized error is a client error here because every write action is
// driven entirely by request parameters.
func statusFor(err error) int {
	switch {
	case errors.Is(err, actions.ErrExists):
		return http.StatusConflict
	case errors.Is(err, jobs.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, safepath.ErrOutsideRoot):
		return http.StatusForbidden
	default:
		return http.StatusBadRequest
	}
}

// decode reads a JSON or form-encoded body into dst. It writes the error
// response itself and reports whether decoding succeeded, so handlers read as
// a straight line.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(ct) == "application/x-www-form-urlencoded" {
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return false
		}
		// Reuse the json tags as form field names by round-tripping through a
		// map; the request shapes are three or four short strings, so this
		// costs nothing and keeps one source of truth for field names.
		m := make(map[string]string, len(r.PostForm))
		for k := range r.PostForm {
			m[k] = r.PostForm.Get(k)
		}
		b, _ := json.Marshal(m)
		if err := json.Unmarshal(b, dst); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return false
		}
		return true
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
