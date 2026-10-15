package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jag2011/strings/internal/store"
)

// External is an account server that hands out one-time sign-in links, like
// the backend of an app whose users request a link to strings in the app.
//
// The link points to /auth/app?code=…. strings exchanges the code at URL:
//
//	POST URL
//	Authorization: Bearer <Secret>
//	{"code": "…"}
//
// which answers 200 with {"id": "…", "name": "…", "email": "…"} once per
// code, and 404 for unknown, used or expired codes.
type External struct {
	Name   string // shown to users, e.g. "Home Quest"
	URL    string
	Secret string
}

var errBadCode = errors.New("unknown or used sign-in code")

type externalAccount struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

var externalClient = &http.Client{Timeout: 10 * time.Second}

func (x *External) redeem(ctx context.Context, code string) (*externalAccount, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, "POST", x.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+x.Secret)
	res, err := externalClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errBadCode
	default:
		return nil, fmt.Errorf("%s: %s", x.URL, res.Status)
	}
	var a externalAccount
	if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
		return nil, fmt.Errorf("%s: %w", x.URL, err)
	}
	return &a, nil
}

type externalData struct {
	App, Code, Error string
}

// externalPage only shows a button: mail scanners open links, and the code
// works once.
func (srv *Server) externalPage(w http.ResponseWriter, r *http.Request) {
	if srv.external == nil {
		srv.renderError(w, r, nil, notFound())
		return
	}
	srv.renderPublic(w, r, http.StatusOK, "signin", "Sign in", externalData{App: srv.external.Name, Code: r.URL.Query().Get("code")})
}

func (srv *Server) externalSignIn(w http.ResponseWriter, r *http.Request) {
	x := srv.external
	if x == nil {
		srv.renderError(w, r, nil, notFound())
		return
	}
	data := externalData{App: x.Name, Code: r.PostFormValue("code")}
	key := "external|" + srv.clientIP(r)
	if srv.limiter.blocked(key) {
		data.Error = "Too many attempts. Try again in 15 minutes."
		srv.renderPublic(w, r, http.StatusTooManyRequests, "signin", "Sign in", data)
		return
	}
	if data.Code == "" {
		srv.renderPublic(w, r, http.StatusNotFound, "signin", "Link expired", data)
		return
	}
	a, err := x.redeem(r.Context(), data.Code)
	if errors.Is(err, errBadCode) {
		srv.limiter.fail(key)
		data.Code = ""
		srv.renderPublic(w, r, http.StatusNotFound, "signin", "Link expired", data)
		return
	}
	if err != nil {
		log.Printf("external sign-in: %v", err)
		data.Error = "Couldn't reach " + x.Name + ". Try again in a minute."
		srv.renderPublic(w, r, http.StatusBadGateway, "signin", "Sign in", data)
		return
	}
	u, err := srv.store.ExternalSignIn(r.Context(), a.ID, a.Email, a.Name)
	if errors.Is(err, store.ErrDisabled) {
		srv.renderError(w, r, nil, httpError{http.StatusForbidden, "Your account is disabled."})
		return
	}
	if err == nil {
		err = srv.startSession(w, r, u)
	}
	if err != nil {
		srv.renderError(w, r, nil, err)
		return
	}
	redirect(w, r, "/", "Welcome, "+u.Name+"!")
}
