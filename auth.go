package main
import pkgerr "github.com/pkg/errors"
import (
	"context"
	"net/http"
	"errors"
	"golang.org/x/crypto/bcrypt"
)

type contextKey string

const UserContextKey contextKey = "user"

var allowedUsers = map[string]string{
	"frodo":   "$2a$10$B6O/n6teuCzpuh66jrUAdeaJ3WvXcxRkzpN0x7H.di9G9e/NGb9Me",
	"samwise": "$2a$10$EWZpvYhUJtJcEMmm/IBOsOGIcpxUnGIVMRiDlN/nxl1RRwWGkJtty",
	// frodo: "ofTheNineFingers"
	// samwise: "theStrong"
	"saruman": "invalidFormat",
}

func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			httpError(r.Context(),w,http.StatusUnauthorized,errors.New("unauthorized"),)
			return
		}
		stored, exists := allowedUsers[username]
		if !exists {
			httpError(r.Context(),w,http.StatusUnauthorized,errors.New("unauthorized"),)
			return
		}
		ok, err := s.validatePassword(password, stored)
		if err != nil {
			s.logger.Error("error validating password", "user", username, "error", err)
			httpError(r.Context(),w,http.StatusInternalServerError,	err,)
			return
		}
		if !ok {
			httpError(r.Context(),w,http.StatusUnauthorized,errors.New("unauthorized"),)
			return
		}
		logCtx, ok := r.Context().Value(logContextKey).(*LogContext)
		if ok {
			logCtx.Username = username
		}

		r = r.WithContext(context.WithValue(r.Context(), UserContextKey, username))
		next.ServeHTTP(w, r)
	})
}

func (s *server) validatePassword(password, stored string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return false, nil
	}
	if err != nil {
		return false, pkgerr.WithStack(err)
	}
	return true, nil
}
