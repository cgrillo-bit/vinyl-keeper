package router

import (
	"context"
	"log"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/ninesl/vinyl-keeper/app/auth"
	"github.com/ninesl/vinyl-keeper/app/router/values"
	"github.com/ninesl/vinyl-keeper/app/vinyl"
)

type responseWriter struct {
	http.ResponseWriter
	statusCode int
	wroteHdr   bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHdr {
		rw.wroteHdr = true
		rw.statusCode = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHdr {
		rw.wroteHdr = true
		rw.statusCode = http.StatusOK
	}
	return rw.ResponseWriter.Write(b)
}

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		log.Printf("[%s] %s %d %s", r.Method, r.URL.Path, wrapped.statusCode, duration)
	})
}

func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf(
					"[Panic] method=%s path=%s remote=%s panic=%v\n%s",
					r.Method,
					r.URL.Path,
					r.RemoteAddr,
					recovered,
					debug.Stack(),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func SetHXTrigger(w http.ResponseWriter, event string) {
	h := w.Header()
	if existing := h.Get(values.HeaderHXTrigger); existing != "" {
		h.Set(values.HeaderHXTrigger, existing+", "+event)
		return
	}
	h.Set(values.HeaderHXTrigger, event)
}

// contextKey is a private type for context keys to avoid collisions
type contextKey int

const (
	// userContextKey is used to store the authenticated user in request context
	userContextKey contextKey = iota
)

// AuthMiddleware extracts the user session and adds it to the request context
// This allows all downstream handlers to access the authenticated user via GetUserFromContext
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionUser, err := auth.GetSessionUser(r)
		if err != nil {
			// Log error but continue as anonymous
			log.Printf("[Auth] Error reading session: %v", err)
		}

		if sessionUser != nil {
			user := &vinyl.User{
				UserID:   sessionUser.UserID,
				UserName: sessionUser.Username,
			}
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
		}

		next.ServeHTTP(w, r)
	})
}

// GetUserFromContext extracts the authenticated user from the request context
// Returns nil if no user is authenticated
func GetUserFromContext(ctx context.Context) *vinyl.User {
	if user, ok := ctx.Value(userContextKey).(*vinyl.User); ok {
		return user
	}
	return nil
}
