// Command api is an OAuth2 resource server. It accepts only access tokens that
// Keycloak issued for audience "api", and enforces one scope per route.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

type claims struct {
	Subject  string `json:"sub"`
	Username string `json:"preferred_username,omitempty"`
	Client   string `json:"azp"` // which client the token was issued to
	Scope    string `json:"scope"`
}

func main() {
	port := flag.String("port", "4000", "listen port")
	issuer := flag.String("issuer", "http://localhost:8080/realms/lab", "token issuer")
	audience := flag.String("audience", "api", "required aud claim")
	flag.Parse()

	provider, err := oidc.NewProvider(context.Background(), *issuer)
	if err != nil {
		log.Fatalf("discover issuer: %v", err)
	}
	// Same checks as for an ID token (JWKS signature, iss, exp), but aud must
	// name this API. That alone rejects ID tokens (aud=foo) and tokens meant
	// for other APIs.
	verifier := provider.Verifier(&oidc.Config{ClientID: *audience})

	mux := http.NewServeMux()
	mux.Handle("GET /orders", require(verifier, "orders:read", func(w http.ResponseWriter, c claims) {
		writeJSON(w, http.StatusOK, map[string]any{
			"caller": c,
			"orders": []string{"#1001 milk", "#1002 bread"},
		})
	}))
	mux.Handle("POST /orders", require(verifier, "orders:write", func(w http.ResponseWriter, c claims) {
		writeJSON(w, http.StatusCreated, map[string]any{"caller": c, "created": "#1003"})
	}))

	log.Printf("api listening on http://localhost:%s (audience %q)", *port, *audience)
	log.Fatal(http.ListenAndServe(":"+*port, mux))
}

// require validates the bearer token and checks it was granted scope.
// Errors follow RFC 6750: 401 for a missing or bad token, 403 for a valid
// token that lacks the scope.
func require(v *oidc.IDTokenVerifier, scope string, next func(http.ResponseWriter, claims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			deny(w, http.StatusUnauthorized, `Bearer realm="api"`, "missing bearer token")
			return
		}
		tok, err := v.Verify(r.Context(), raw)
		if err != nil {
			deny(w, http.StatusUnauthorized, `Bearer error="invalid_token"`, err.Error())
			return
		}
		var c claims
		if err := tok.Claims(&c); err != nil {
			deny(w, http.StatusUnauthorized, `Bearer error="invalid_token"`, err.Error())
			return
		}
		if !slices.Contains(strings.Fields(c.Scope), scope) {
			deny(w, http.StatusForbidden, fmt.Sprintf(`Bearer error="insufficient_scope", scope=%q`, scope),
				fmt.Sprintf("token has scope %q, route needs %q", c.Scope, scope))
			return
		}
		next(w, c)
	}
}

func deny(w http.ResponseWriter, status int, challenge, reason string) {
	w.Header().Set("WWW-Authenticate", challenge)
	writeJSON(w, status, map[string]string{"error": reason})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
