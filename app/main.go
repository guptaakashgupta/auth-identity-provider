// Command app is a minimal OIDC relying party. Run it twice (foo, bar) to see SSO.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type session struct {
	claims  map[string]any
	idToken string        // kept for id_token_hint on logout
	tok     *oauth2.Token // access + refresh token, used to call the API
}

// ponytail: in-memory sessions, lost on restart; use Redis/DB if it matters.
var (
	mu       sync.Mutex
	sessions = map[string]session{}
)

func main() {
	name := flag.String("name", "foo", "client id and app name")
	port := flag.String("port", "3001", "listen port")
	secret := flag.String("secret", "foo-secret", "client secret")
	peer := flag.String("peer", "http://bar.localhost:3002", "the other app, for the demo link")
	issuer := flag.String("issuer", "http://localhost:8080/realms/lab", "OIDC issuer URL")
	scopes := flag.String("scopes", "", "extra space-separated scopes to request, e.g. orders:read")
	api := flag.String("api", "http://localhost:4000", "resource server base URL")
	flag.Parse()

	base := fmt.Sprintf("http://%s.localhost:%s", *name, *port)
	ctx := context.Background()

	// Discovery: reads <issuer>/.well-known/openid-configuration.
	provider, err := oidc.NewProvider(ctx, *issuer)
	if err != nil {
		log.Fatalf("discover issuer: %v", err)
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&meta); err != nil {
		log.Fatalf("read provider metadata: %v", err)
	}

	conf := &oauth2.Config{
		ClientID:     *name,
		ClientSecret: *secret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  base + "/callback",
		Scopes:       append([]string{oidc.ScopeOpenID, "profile", "email"}, strings.Fields(*scopes)...),
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: *name})

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := current(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<h1>%s</h1>", html.EscapeString(*name))
		if !ok {
			fmt.Fprint(w, `<p>Not logged in.</p><a href="/login">Login with SSO</a>`)
			return
		}
		claims, _ := json.MarshalIndent(s.claims, "", "  ")
		fmt.Fprintf(w, `<p>Logged in as <b>%s</b></p><pre>%s</pre>
<p><a href="%s">Go to the other app</a> | <a href="/logout">Logout</a></p>
<h2>Access token (for the API, not for %s)</h2><p>Expires %s</p><pre>%s</pre>
<form method="post" action="/call-api">Call the API: <button name="method" value="GET">GET /orders</button>
<button name="method" value="POST">POST /orders</button></form>`,
			html.EscapeString(fmt.Sprint(s.claims["preferred_username"])),
			html.EscapeString(string(claims)), html.EscapeString(*peer),
			html.EscapeString(*name), s.tok.Expiry.Format("15:04:05"), html.EscapeString(peek(s.tok.AccessToken)))
	})

	// Step 1: send the browser to the IdP with state (CSRF) and a PKCE challenge.
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		state, pkce := randomID(), oauth2.GenerateVerifier()
		setTemp(w, "state", state)
		setTemp(w, "pkce", pkce)
		http.Redirect(w, r, conf.AuthCodeURL(state, oauth2.S256ChallengeOption(pkce)), http.StatusFound)
	})

	// Step 2: the IdP redirects back with ?code; swap it for tokens over the back channel.
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		state, err1 := r.Cookie("state")
		pkce, err2 := r.Cookie("pkce")
		if err1 != nil || err2 != nil {
			http.Error(w, "invalid state: login cookies missing (expired after 5 min, or login started on another host)", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("state") != state.Value {
			http.Error(w, "invalid state: mismatch (another login started in a different tab?) - retry from /login", http.StatusBadRequest)
			return
		}
		setTemp(w, "state", "")
		setTemp(w, "pkce", "")
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "IdP returned "+e+": "+r.URL.Query().Get("error_description"), http.StatusBadRequest)
			return
		}
		tok, err := conf.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(pkce.Value))
		if err != nil {
			http.Error(w, "code exchange failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		raw, ok := tok.Extra("id_token").(string)
		if !ok {
			http.Error(w, "no id_token in response", http.StatusBadGateway)
			return
		}
		// Checks signature (JWKS), issuer, audience and expiry.
		idt, err := verifier.Verify(r.Context(), raw)
		if err != nil {
			http.Error(w, "invalid id_token: "+err.Error(), http.StatusUnauthorized)
			return
		}
		claims := map[string]any{}
		if err := idt.Claims(&claims); err != nil {
			http.Error(w, "read claims: "+err.Error(), http.StatusInternalServerError)
			return
		}

		sid := randomID()
		mu.Lock()
		sessions[sid] = session{claims: claims, idToken: raw, tok: tok}
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/", http.StatusFound)
	})

	// Calls the API as the logged-in user by sending the access token as a bearer token.
	mux.HandleFunc("POST /call-api", func(w http.ResponseWriter, r *http.Request) {
		s, ok := current(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		method := http.MethodGet
		if r.FormValue("method") == http.MethodPost {
			method = http.MethodPost
		}
		// TokenSource returns the stored token, or uses the refresh token to get a
		// new one once the access token is within 10s of expiry.
		t, err := conf.TokenSource(r.Context(), s.tok).Token()
		if err != nil {
			http.Error(w, "refresh failed (IdP session ended?), log in again: "+err.Error(), http.StatusUnauthorized)
			return
		}
		refreshed := t.AccessToken != s.tok.AccessToken
		if c, err := r.Cookie("sid"); err == nil {
			mu.Lock()
			s.tok = t
			sessions[c.Value] = s
			mu.Unlock()
		}

		req, err := http.NewRequestWithContext(r.Context(), method, *api+"/orders", nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		req.Header.Set("Authorization", "Bearer "+t.AccessToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "api unreachable (is make api running?): "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<h1>%s calls the API</h1><p>Access token refreshed for this call: <b>%v</b></p>
<p>%s %s/orders &rarr; <b>%s</b></p><pre>WWW-Authenticate: %s</pre><pre>%s</pre><a href="/">Back</a>`,
			html.EscapeString(*name), refreshed, method, html.EscapeString(*api), html.EscapeString(resp.Status),
			html.EscapeString(resp.Header.Get("WWW-Authenticate")), html.EscapeString(string(body)))
	})

	// Clears the local session and ends the IdP session (RP-initiated logout).
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		s, _ := current(r)
		if c, err := r.Cookie("sid"); err == nil {
			mu.Lock()
			delete(sessions, c.Value)
			mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Path: "/", MaxAge: -1})
		q := url.Values{"post_logout_redirect_uri": {base + "/"}, "client_id": {*name}}
		if s.idToken != "" {
			q.Set("id_token_hint", s.idToken)
		}
		http.Redirect(w, r, meta.EndSession+"?"+q.Encode(), http.StatusFound)
	})

	// Cookies are per hostname, so the whole flow must stay on the same host
	// as RedirectURL; send localhost:3001 / 127.0.0.1:3001 to foo.localhost:3001.
	host := strings.TrimPrefix(base, "http://")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Redirect(w, r, base+r.URL.RequestURI(), http.StatusFound)
			return
		}
		mux.ServeHTTP(w, r)
	})

	log.Printf("%s listening on %s", *name, base)
	log.Fatal(http.ListenAndServe(":"+*port, handler))
}

func current(r *http.Request) (session, bool) {
	c, err := r.Cookie("sid")
	if err != nil {
		return session{}, false
	}
	mu.Lock()
	defer mu.Unlock()
	s, ok := sessions[c.Value]
	return s, ok
}

// setTemp sets a short-lived login cookie; an empty value deletes it.
func setTemp(w http.ResponseWriter, name, value string) {
	maxAge := 300
	if value == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// peek decodes a JWT payload WITHOUT verifying it, for display only. A client
// should treat the access token as opaque; the API is the one that verifies it.
func peek(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "(not a JWT)"
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "(undecodable payload)"
	}
	var out bytes.Buffer
	if json.Indent(&out, b, "", "  ") != nil {
		return string(b)
	}
	return out.String()
}

func randomID() string {
	b := make([]byte, 32)
	rand.Read(b) // crypto/rand.Read never returns an error since Go 1.24
	return hex.EncodeToString(b)
}
