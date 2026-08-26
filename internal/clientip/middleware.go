package clientip

import (
	"context"
	"net/http"
)

type contextKey string

const clientIPKey contextKey = "client_ip"

// Middleware returns chi-compatible middleware that resolves the client IP
// and stores it in the request context. It also overwrites r.RemoteAddr so
// that downstream middleware (e.g. chi's Logger) and any code reading
// RemoteAddr directly sees the real client IP instead of the proxy address.
func Middleware(resolver *Resolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := resolver.ClientIP(r)
			r.RemoteAddr = ip
			ctx := context.WithValue(r.Context(), clientIPKey, ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext retrieves the resolved client IP from the request context.
// Returns empty string if not set.
func FromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}

// SetContext stores a client IP in the context. Useful for testing handlers
// that depend on the clientip middleware without going through the full chain.
func SetContext(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}
