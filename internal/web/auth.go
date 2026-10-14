package web

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/jag2011/strings/internal/store"
)

const sessionCookie = "strings_session"

func (srv *Server) currentUser(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := srv.store.SessionUser(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return u
}

func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (srv *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	token, err := srv.store.CreateSession(r.Context(), u.ID)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: 30 * 24 * 3600,
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// safeNext only allows local paths as redirect targets.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

type loginData struct {
	Email, Next, Error string
}

func (srv *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if srv.currentUser(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	srv.renderPublic(w, r, http.StatusOK, "login", "Log in", loginData{Next: r.URL.Query().Get("next")})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (srv *Server) login(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	data := loginData{Email: email, Next: r.PostFormValue("next")}
	key := clientIP(r) + "|" + email
	if srv.limiter.blocked(key) {
		data.Error = "Too many attempts. Try again in 15 minutes."
		srv.renderPublic(w, r, http.StatusTooManyRequests, "login", "Log in", data)
		return
	}
	u, err := srv.store.Login(r.Context(), email, r.PostFormValue("password"))
	if errors.Is(err, store.ErrNotFound) {
		srv.limiter.fail(key)
		data.Error = "Wrong email or password."
		srv.renderPublic(w, r, http.StatusUnauthorized, "login", "Log in", data)
		return
	}
	if err == nil {
		err = srv.startSession(w, r, u)
	}
	if err != nil {
		srv.renderError(w, r, nil, err)
		return
	}
	http.Redirect(w, r, safeNext(data.Next), http.StatusSeeOther)
}

func (srv *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		srv.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type inviteData struct {
	User  *store.User
	Error string
}

func (srv *Server) invitePage(w http.ResponseWriter, r *http.Request) {
	u, err := srv.store.UserTokenUser(r.Context(), r.PathValue("token"))
	if err != nil {
		srv.renderPublic(w, r, http.StatusNotFound, "invite", "Link expired", inviteData{})
		return
	}
	srv.renderPublic(w, r, http.StatusOK, "invite", "Set your password", inviteData{User: u})
}

func (srv *Server) invite(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	pw := r.PostFormValue("password")
	if pw != r.PostFormValue("password2") {
		u, _ := srv.store.UserTokenUser(r.Context(), token)
		srv.renderPublic(w, r, http.StatusUnprocessableEntity, "invite", "Set your password", inviteData{User: u, Error: "The passwords don't match."})
		return
	}
	u, err := srv.store.UseUserToken(r.Context(), token, pw)
	if errors.Is(err, store.ErrNotFound) {
		srv.renderPublic(w, r, http.StatusNotFound, "invite", "Link expired", inviteData{})
		return
	}
	if err != nil {
		u, _ := srv.store.UserTokenUser(r.Context(), token)
		srv.renderPublic(w, r, http.StatusUnprocessableEntity, "invite", "Set your password", inviteData{User: u, Error: err.Error()})
		return
	}
	if err := srv.startSession(w, r, u); err != nil {
		srv.renderError(w, r, nil, err)
		return
	}
	redirect(w, r, "/", "Welcome, "+u.Name+"!")
}

func (srv *Server) renderPublic(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	if err := srv.render(w, r, status, name, nil, title, data); err != nil {
		srv.renderError(w, r, nil, err)
	}
}

// apiAuth requires "Authorization: Bearer st_…".
func (srv *Server) apiAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !srv.store.CheckAPIToken(r.Context(), token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="strings"`)
			jsonError(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		h.ServeHTTP(w, r)
	})
}
