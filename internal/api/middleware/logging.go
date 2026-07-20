package middleware

import (
	"fmt"
	"net/http"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func Logging(log logger.LoggerService, enabled bool, next http.Handler) http.Handler {
	if !enabled || log == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		duration := time.Since(start).Truncate(time.Millisecond)
		msg := fmt.Sprintf("%s %s %d %s", r.Method, r.URL.Path, status, duration)
		switch {
		case status >= http.StatusInternalServerError:
			log.Error(msg, nil)
		case status >= http.StatusBadRequest:
			log.Warn(msg)
		default:
			log.Info(msg)
		}
	})
}
