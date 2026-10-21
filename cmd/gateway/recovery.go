package main

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// recoverMiddleware: one bad handler must not take the gateway down. Log the
// stack, return JSON 500, move on.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("http_panic_recovered",
					"panic", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"remote", r.RemoteAddr,
					"stack", string(debug.Stack()))
				// Best effort: headers may already be committed.
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
