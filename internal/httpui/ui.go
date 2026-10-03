// Package httpui serves Gyemoim's embedded management UI and runtime status API.
package httpui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"runtime"
	"time"

	"github.com/gosuda/gyemoim/internal/history"
)

//go:embed assets/*
var embedded embed.FS

// Status contains runtime information that is safe to expose on the local UI.
type Status struct {
	State                  string    `json:"state"`
	DataDirectory          string    `json:"dataDirectory"`
	Port                   int       `json:"port"`
	StartedAt              time.Time `json:"startedAt"`
	GoVersion              string    `json:"goVersion"`
	OperatingSystem        string    `json:"operatingSystem"`
	Architecture           string    `json:"architecture"`
	SQLiteState            string    `json:"sqliteState"`
	HistoryState           string    `json:"historyState"`
	HistoryPotentiallyLost uint64    `json:"historyPotentiallyLostRecords"`
	HistoryRecoveredBytes  uint64    `json:"historyRecoveredBytes"`
	HistoryActive          int       `json:"historyActiveRequests"`
	HistoryBytesWritten    int64     `json:"historyBytesWritten"`
}

// DatabaseReadiness is the minimal status dependency needed from the config store.
type DatabaseReadiness interface {
	Ping(context.Context) error
}

// HistoryReadiness is the safe recorder health view needed by the local status API.
type HistoryReadiness interface {
	Status() history.RecorderStatus
}

// New constructs the embedded UI handler.
func New(dataDirectory string, port int, startedAt time.Time, csrfToken string, database DatabaseReadiness, historyStatus HistoryReadiness) (http.Handler, error) {
	assets, err := fs.Sub(embedded, "assets")
	if err != nil {
		return nil, fmt.Errorf("open embedded UI assets: %w", err)
	}
	page, err := template.ParseFS(assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("parse embedded UI page: %w", err)
	}
	status := Status{
		State:           "ready",
		DataDirectory:   dataDirectory,
		Port:            port,
		StartedAt:       startedAt,
		GoVersion:       runtime.Version(),
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
		SQLiteState:     "ready",
		HistoryState:    "unavailable",
	}
	fileServer := http.FileServer(http.FS(assets))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.ExecuteTemplate(w, "index.html", struct{ CSRFToken string }{CSRFToken: csrfToken}); err != nil {
			// The response may already be committed; logging belongs at the server boundary later.
			return
		}
	})
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", fileServer))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		currentStatus := status
		if database == nil {
			currentStatus.SQLiteState = "unavailable"
			currentStatus.State = "degraded"
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			if err := database.Ping(ctx); err != nil {
				currentStatus.SQLiteState = "unavailable"
				currentStatus.State = "degraded"
			}
			cancel()
		}
		if historyStatus == nil {
			currentStatus.State = "degraded"
		} else {
			history := historyStatus.Status()
			currentStatus.HistoryState = history.State
			currentStatus.HistoryPotentiallyLost = history.PotentiallyLostRecords
			currentStatus.HistoryRecoveredBytes = history.RecoveredBytes
			currentStatus.HistoryActive = history.ActiveRequests
			currentStatus.HistoryBytesWritten = history.BytesWritten
			if history.State != "ready" {
				currentStatus.State = "degraded"
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(currentStatus); err != nil {
			return
		}
	})
	return mux, nil
}
