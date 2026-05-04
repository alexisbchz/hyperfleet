package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// HTTPMiddleware returns a chi-compatible middleware that enforces API key auth.
// Accepts either `X-API-Key: <key>` or `Authorization: Bearer <key>`.
// Bypasses OpenAPI/docs endpoints so the spec is publicly fetchable.
func HTTPMiddleware(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if !Check(extractKey(r), key) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hyperfleet"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Check compares the candidate to the expected API key in constant time.
func Check(candidate, expected string) bool {
	if expected == "" || candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

func extractKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}

func isPublicPath(p string) bool {
	switch p {
	case "/openapi.json", "/openapi.yaml", "/docs":
		return true
	}
	return strings.HasPrefix(p, "/docs")
}
