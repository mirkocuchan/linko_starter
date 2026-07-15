package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"io"
	"syscall"
	"strings"
    "slices"
	"time"
	"net/http"
	"net"
	"fmt"
	"errors"
	"crypto/rand"
	"net/url"
	pkgerr "github.com/pkg/errors"
	"boot.dev/linko/internal/store"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/build"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, closeLogger, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	if err != nil {
		slog.Error("failed to initialize logger", "error", err)
		return 1
	}
	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error("failed to create store", "error", err)
		return 1
	}

	s := newServer(*st, httpPort, cancel, logger)

	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logger.Debug("Linko is shutting down")

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}

	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}

	return 0
}
type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	// Si nunca llamaron a WriteHeader, Go asume 200.
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n

	return n, err
}
const logContextKey contextKey = "log_context"

func httpError(ctx context.Context, w http.ResponseWriter, statusCode int, err error) {
	logCtx, ok := ctx.Value(logContextKey).(*LogContext)
	if ok {
		logCtx.Error = err
	}

	msg := err.Error()

	switch statusCode {
	case http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusInternalServerError:
		msg = http.StatusText(statusCode)
	}

	http.Error(w, msg, statusCode)
}

type LogContext struct {
	Username string
	Error    error
}
func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}
func redactIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return addr
	}

	ipv4 := ip.To4()
	if ipv4 == nil {
		return addr
	}

	return fmt.Sprintf("%d.%d.%d.x", ipv4[0], ipv4[1], ipv4[2])
}
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			spyReader := &spyReadCloser{
				ReadCloser: r.Body,
			}
			r.Body = spyReader

			spyWriter := &spyResponseWriter{
				ResponseWriter: w,
			}

			logCtx := &LogContext{}
			r = r.WithContext(
				context.WithValue(r.Context(), logContextKey, logCtx),
			)

			next.ServeHTTP(spyWriter, r)

			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("request_id", r.Header.Get("X-Request-ID")),
				slog.String("client_ip", redactIP(r.RemoteAddr)),
				slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
			}

			if logCtx.Username != "" {
				attrs = append(attrs,
					slog.String("user", logCtx.Username),
				)
			}
			if logCtx.Error != nil {
				attrs = append(attrs,
					slog.Any("error", logCtx.Error),
				)
			}
			logger.Info("Served request", attrs...)
		})
	}
}
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")

		if requestID == "" {
			requestID = rand.Text()
		}

		w.Header().Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r)
	})
}

type closeFunc func() error
type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}
type multiError interface {
	error
	Unwrap() []error
}
func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{slog.String("message", err.Error()),}

	attrs = append(attrs, linkoerr.Attrs(err)...)

	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.String("stack_trace", fmt.Sprintf("%+v", stackErr.StackTrace()),),)
	}
	return attrs
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	sensitiveKeys := []string{
        "password",
        "key",
        "apikey",
        "secret",
        "pin",
        "creditcardno",
        "user",
    }
    if slices.Contains(sensitiveKeys, strings.ToLower(a.Key)) {
        return slog.String(a.Key, "[REDACTED]")
    }
	if a.Value.Kind() == slog.KindString {
		str := a.Value.String()

		u, err := url.Parse(str)
		if err == nil && u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				u.User = url.UserPassword(u.User.Username(), "[REDACTED]")
				return slog.String(a.Key, u.String())
			}
		}
	}
    if a.Key != "error" {
        return a
    }
	err, ok := a.Value.Any().(error)
	if !ok {
		return a
	}

	if multiErr, ok := errors.AsType[multiError](err); ok {
		var errs []slog.Attr

		for i, err := range multiErr.Unwrap() {
			errs = append(errs,	slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errorAttrs(err)...,),)
		}
		return slog.GroupAttrs("errors", errs...)
	}
	return slog.GroupAttrs("error", errorAttrs(err)...)
}

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error){
	fd := os.Stderr.Fd()
	color := isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)

	handlers := []slog.Handler{
	tint.NewHandler(os.Stderr, &tint.Options{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
		NoColor:     !color,}),
	}

	closers := []closeFunc{}

	if logFile != "" {
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		close := func() error {
        	if err := logger.Close(); err != nil {
            	return fmt.Errorf("failed to close log file: %w", err)
        	}	
        	return nil
    	}
		handlers = append(handlers,
			slog.NewJSONHandler(logger, &slog.HandlerOptions{
				Level:       slog.LevelInfo,
				ReplaceAttr: replaceAttr,
			}),
		)
		closers = append(closers, close)
	}
	closer := func() error {
		var errs []error

		for _, close := range closers {
			if err := close(); err != nil {
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	}

	return slog.New(slog.NewMultiHandler(handlers...)), closer, nil
}

