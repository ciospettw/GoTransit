// Package api is the HTTP interface: plain JSON over GET — the format every
// React Native (and web, and curl) client consumes natively, no SDK needed.
// It also ships the map debug UI (on by default, debug_ui = false to disable).
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gotransit/internal/engine"
)

//go:embed debug.html
var debugHTML []byte

// TrackSink is the tracker's event sink (mirrors track.Sink structurally).
type TrackSink interface {
	Send(event any) error
}

// TrackFunc runs one journey-tracking session (wired to track.Tracker.Run).
type TrackFunc func(ctx context.Context, itineraryID string, sink TrackSink) error

// Server wraps the engine with HTTP handlers.
type Server struct {
	E       *engine.Engine
	Log     *slog.Logger
	DebugUI bool
	Track   TrackFunc // nil until realtime is wired
}

// Handler builds the HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/plan", s.cors(s.handlePlan))
	mux.HandleFunc("GET /v1/health", s.cors(s.handleHealth))
	mux.HandleFunc("GET /v1/status", s.cors(s.handleStatus))
	mux.HandleFunc("GET /v1/track", s.handleTrack)
	mux.HandleFunc("OPTIONS /", func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		w.WriteHeader(http.StatusNoContent)
	})
	if s.DebugUI {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(debugHTML)
		})
	}
	return mux
}

// handleTrack upgrades to WebSocket and streams journey events.
func (s *Server) handleTrack(w http.ResponseWriter, r *http.Request) {
	if s.Track == nil {
		s.jsonErr(w, 501, "tracking unavailable: engine started without it")
		return
	}
	id := r.URL.Query().Get("itinerary")
	if id == "" {
		s.jsonErr(w, 400, "missing ?itinerary=<id> (from a /v1/plan response)")
		return
	}
	ws, err := UpgradeWS(w, r)
	if err != nil {
		s.jsonErr(w, 400, "websocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// reader: consume client frames (pings handled inside), detect disconnect
	go func() {
		defer cancel()
		for {
			if _, err := ws.ReadMessage(90 * time.Second); err != nil {
				return
			}
		}
	}()
	// keepalive pings
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ws.Ping() != nil {
					cancel()
					return
				}
			}
		}
	}()

	if err := s.Track(ctx, id, wsSink{ws}); err != nil && ctx.Err() == nil {
		s.Log.Debug("track session ended", "err", err)
	}
}

type wsSink struct{ c *WSConn }

func (s wsSink) Send(ev any) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return s.c.SendText(b)
}

func corsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func (s *Server) cors(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		fn(w, r)
	}
}

func (s *Server) jsonErr(w http.ResponseWriter, code int, format string, a ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, a...)})
}

// handlePlan: /v1/plan?from=41.9,12.5&to=41.8,12.6&mode=transit&depart=now&num=3
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	q := r.URL.Query()

	fromLat, fromLon, err := parseLatLon(q.Get("from"))
	if err != nil {
		s.jsonErr(w, 400, "from: %v", err)
		return
	}
	toLat, toLon, err := parseLatLon(q.Get("to"))
	if err != nil {
		s.jsonErr(w, 400, "to: %v", err)
		return
	}
	req := engine.Request{
		FromLat: fromLat, FromLon: fromLon,
		ToLat: toLat, ToLon: toLon,
		Mode: strings.ToLower(q.Get("mode")),
	}
	if req.Mode == "" {
		req.Mode = "transit"
	}
	if n := q.Get("num"); n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 && v <= 10 {
			req.Num = v
		}
	}
	req.Live = q.Get("live") == "true" || q.Get("live") == "1"

	// naive datetimes are interpreted in the transit network's timezone,
	// never the client's or the host's
	tz := s.E.Timezone()
	depart, arrive := q.Get("depart"), q.Get("arrive")
	switch {
	case arrive != "":
		t, err := parseWhen(arrive, tz)
		if err != nil {
			s.jsonErr(w, 400, "arrive: %v", err)
			return
		}
		req.When, req.ArriveBy = t, true
	case depart == "" || depart == "now":
		req.When = time.Now()
	default:
		t, err := parseWhen(depart, tz)
		if err != nil {
			s.jsonErr(w, 400, "depart: %v", err)
			return
		}
		req.When = t
	}

	resp, err := s.E.Plan(req)
	if err != nil {
		s.jsonErr(w, 422, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Query-Ms", strconv.FormatInt(time.Since(t0).Milliseconds(), 10))
	json.NewEncoder(w).Encode(resp)
	s.Log.Debug("plan", "mode", req.Mode, "ms", time.Since(t0).Milliseconds(), "itineraries", len(resp.Itineraries))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !s.E.Ready() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "starting": true})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(s.E.Status())
}

func parseLatLon(v string) (float64, float64, error) {
	if v == "" {
		return 0, 0, fmt.Errorf("missing (want \"lat,lon\")")
	}
	parts := strings.SplitN(v, ",", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("want \"lat,lon\", got %q", v)
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, fmt.Errorf("invalid coordinates %q", v)
	}
	return lat, lon, nil
}

// parseWhen accepts RFC3339 (explicit offset wins) or naive forms like
// "2026-07-10 08:30", which are read in the network's timezone tz.
func parseWhen(v string, tz *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, v, tz); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q (RFC3339 or \"YYYY-MM-DD HH:MM\")", v)
}
