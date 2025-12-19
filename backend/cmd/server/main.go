package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	DB  *pgxpool.Pool
	SSE *SSEHub
}

/* =========================
   DTOs
========================= */

type LinkDTO struct {
	Code        string `json:"code"`
	OriginalURL string `json:"originalUrl"`
}

type LinkItem struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	OriginalURL string `json:"originalUrl"`
	CreatedAt   string `json:"createdAt"`
	ShortURL    string `json:"shortUrl"`
	TotalClicks int    `json:"totalClicks"`
}

type SeriesPoint struct {
	T      string `json:"t"`
	Clicks int    `json:"clicks"`
}

type RefRow struct {
	Ref    string `json:"ref"`
	Clicks int    `json:"clicks"`
}

type StatsResponse struct {
	TotalClicks    int           `json:"totalClicks"`
	UniqueVisitors int           `json:"uniqueVisitors"`
	Series         []SeriesPoint `json:"series,omitempty"`
	TopReferrers   []RefRow      `json:"topReferrers,omitempty"`
}

type StreamDelta struct {
	DeltaClicks  int    `json:"deltaClicks"`
	DeltaUniques int    `json:"deltaUniques"`
	Ref          string `json:"ref"`
	Ts           string `json:"ts"`
}


/* =========================
   SSE Hub
========================= */

type SSEClient struct {
	ch chan []byte
}

type SSEHub struct {
	mu      sync.Mutex
	clients map[string]map[*SSEClient]struct{} // code -> set
}

func NewSSEHub() *SSEHub {
	return &SSEHub{
		clients: make(map[string]map[*SSEClient]struct{}),
	}
}

func (h *SSEHub) add(code string, c *SSEClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[code]; !ok {
		h.clients[code] = make(map[*SSEClient]struct{})
	}
	h.clients[code][c] = struct{}{}
}

func (h *SSEHub) remove(code string, c *SSEClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.clients[code]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, code)
		}
	}
}

func (h *SSEHub) Broadcast(code string, event string, payload any) {
	b, _ := json.Marshal(payload)
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(b)))

	h.mu.Lock()
	set := h.clients[code]
	var targets []*SSEClient
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.Unlock()

	for _, c := range targets {
		select {
		case c.ch <- msg:
		default:
			// drop if slow
		}
	}
}

// /api/links/{code}/stats/stream?userId=...
func (h *SSEHub) HandleStream(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		userID := r.URL.Query().Get("userId")
		if userID == "" {
			httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing userId"})
			return
		}

		ok, err := app.linkBelongsToUser(r.Context(), code, userID)
		if err != nil {
			httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if !ok {
			httpJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}

		client := &SSEClient{ch: make(chan []byte, 16)}
		h.add(code, client)
		defer h.remove(code, client)

		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
				flusher.Flush()
			case msg := <-client.ch:
				_, _ = w.Write(msg)
				flusher.Flush()
			}
		}
	}
}

/* =========================
   Handlers
========================= */

// GET /api/links
func (a *App) HandleListLinks(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-Id")
	if userID == "" {
		httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing X-User-Id"})
		return
	}

	rows, err := a.DB.Query(r.Context(), `
		SELECT
			l.id::text,
			l.code,
			l.original_url,
			l.created_at::timestamptz::text,
			COUNT(c.id)::int AS total_clicks
		FROM links l
		LEFT JOIN clicks c ON c.link_id = l.id
		WHERE l.user_id = $1
		GROUP BY l.id
		ORDER BY l.created_at DESC
		LIMIT 200
	`, userID)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}
	defer rows.Close()

	base := strings.TrimRight(os.Getenv("APP_BASE_URL"), "/")
	if base == "" {
		base = "http://localhost:8080"
	}

	out := []LinkItem{}
	for rows.Next() {
		var it LinkItem
		if err := rows.Scan(
			&it.ID,
			&it.Code,
			&it.OriginalURL,
			&it.CreatedAt,
			&it.TotalClicks,
		); err != nil {
			httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}

		it.ShortURL = base + "/r/" + it.Code
		out = append(out, it)
	}

	httpJSON(w, http.StatusOK, out)
}


// POST /api/links (accepts {url} or {originalUrl})
type CreateLinkReq struct {
	URL         string `json:"url"`
	OriginalURL string `json:"originalUrl"`
}

func (a *App) HandleCreateLink(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-Id")
	if userID == "" {
		httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing X-User-Id"})
		return
	}

	var req CreateLinkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}

	raw := strings.TrimSpace(req.URL)
	if raw == "" {
		raw = strings.TrimSpace(req.OriginalURL)
	}
	if raw == "" {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": "url is required"})
		return
	}

	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid url"})
		return
	}

	base := strings.TrimRight(os.Getenv("APP_BASE_URL"), "/")
	if base == "" {
		base = "http://localhost:8080"
	}

	const maxTries = 12
	var code string
	for i := 0; i < maxTries; i++ {
		code = randomCode(7)

		_, err := a.DB.Exec(r.Context(),
			`INSERT INTO links (user_id, code, original_url) VALUES ($1, $2, $3)`,
			userID, code, raw,
		)
		if err == nil {
			var id string
			var createdAt string
			if err2 := a.DB.QueryRow(r.Context(),
				`SELECT id::text, created_at::timestamptz::text FROM links WHERE user_id=$1 AND code=$2`,
				userID, code,
			).Scan(&id, &createdAt); err2 != nil {
				httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
				return
			}

			httpJSON(w, http.StatusCreated, LinkItem{
				ID:          id,
				Code:        code,
				OriginalURL: raw,
				CreatedAt:   createdAt,
				ShortURL:    base + "/r/" + code,
			})
			return
		}

		if isUniqueViolation(err) {
			continue
		}
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to allocate code"})
}

// GET /api/links/{code}
func (a *App) HandleGetLink(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	userID := r.Header.Get("X-User-Id")
	if userID == "" {
		httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing X-User-Id"})
		return
	}

	var dto LinkDTO
	err := a.DB.QueryRow(r.Context(),
		`SELECT code, original_url FROM links WHERE code=$1 AND user_id=$2`,
		code, userID,
	).Scan(&dto.Code, &dto.OriginalURL)

	if err != nil {
		if err == pgx.ErrNoRows {
			httpJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	httpJSON(w, http.StatusOK, dto)
}

// GET /api/links/{code}/stats?range=24h|7d
func (a *App) HandleGetStats(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	userID := r.Header.Get("X-User-Id")
	if userID == "" {
		httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing X-User-Id"})
		return
	}

	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	if rng != "24h" && rng != "7d" {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid range"})
		return
	}

	linkID, err := a.getLinkID(r.Context(), code, userID)
	if err != nil {
		if err == pgx.ErrNoRows {
			httpJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	totalClicks, uniqueVisitors, err := a.queryTotals(r.Context(), linkID)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	series, err := a.querySeries(r.Context(), linkID, rng)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	topRefs, err := a.queryTopReferrers(r.Context(), linkID, rng)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	httpJSON(w, http.StatusOK, StatsResponse{
		TotalClicks:    totalClicks,
		UniqueVisitors: uniqueVisitors,
		Series:         series,
		TopReferrers:   topRefs,
	})
}

// GET /r/{code} -> insert click + broadcast SSE + redirect
func (a *App) HandleRedirect(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	var linkID string
	var original string
	err := a.DB.QueryRow(r.Context(),
		`SELECT id, original_url FROM links WHERE code=$1`,
		code,
	).Scan(&linkID, &original)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ip := clientIP(r)
	ua := r.UserAgent()
	refFull := r.Referer()
	refDomain := normalizeRefDomain(refFull)

	_, _ = a.DB.Exec(r.Context(),
		`INSERT INTO clicks (link_id, ts, ip, ua, referer) VALUES ($1, now(), $2, $3, $4)`,
		linkID, ip, ua, refFull,
	)

	a.SSE.Broadcast(code, "stats", StreamDelta{
		DeltaClicks:  1,
		DeltaUniques: 0,
		Ref:          refDomain,
		Ts:           time.Now().UTC().Format(time.RFC3339),
	})

	http.Redirect(w, r, original, http.StatusFound)
}

/* =========================
   Queries
========================= */

func (a *App) getLinkID(ctx context.Context, code string, userID string) (string, error) {
	var id string
	err := a.DB.QueryRow(ctx,
		`SELECT id FROM links WHERE code=$1 AND user_id=$2`,
		code, userID,
	).Scan(&id)
	return id, err
}

func (a *App) linkBelongsToUser(ctx context.Context, code string, userID string) (bool, error) {
	var one int
	err := a.DB.QueryRow(ctx,
		`SELECT 1 FROM links WHERE code=$1 AND user_id=$2`,
		code, userID,
	).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (a *App) queryTotals(ctx context.Context, linkID string) (total int, uniques int, err error) {
	if err = a.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM clicks WHERE link_id=$1`,
		linkID,
	).Scan(&total); err != nil {
		return
	}
	if err = a.DB.QueryRow(ctx,
		`SELECT COUNT(DISTINCT ip) FROM clicks WHERE link_id=$1 AND ip IS NOT NULL`,
		linkID,
	).Scan(&uniques); err != nil {
		return
	}
	return
}

func (a *App) querySeries(ctx context.Context, linkID string, rng string) ([]SeriesPoint, error) {
	if rng == "7d" {
		return a.queryDailySeries(ctx, linkID)
	}
	return a.queryHourlySeries(ctx, linkID)
}

func (a *App) queryHourlySeries(ctx context.Context, linkID string) ([]SeriesPoint, error) {
	rows, err := a.DB.Query(ctx, `
WITH buckets AS (
  SELECT generate_series(
    date_trunc('hour', now() - interval '23 hour'),
    date_trunc('hour', now()),
    interval '1 hour'
  ) AS bucket
),
agg AS (
  SELECT date_trunc('hour', ts) AS bucket, COUNT(*)::int AS clicks
  FROM clicks
  WHERE link_id = $1
    AND ts >= now() - interval '24 hour'
  GROUP BY 1
)
SELECT buckets.bucket::timestamptz::text, COALESCE(agg.clicks, 0)::int
FROM buckets
LEFT JOIN agg USING (bucket)
ORDER BY buckets.bucket ASC
`, linkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SeriesPoint{}
	for rows.Next() {
		var t string
		var clicks int
		if err := rows.Scan(&t, &clicks); err != nil {
			return nil, err
		}
		out = append(out, SeriesPoint{T: t, Clicks: clicks})
	}
	return out, nil
}

func (a *App) queryDailySeries(ctx context.Context, linkID string) ([]SeriesPoint, error) {
	rows, err := a.DB.Query(ctx, `
WITH buckets AS (
  SELECT generate_series(
    date_trunc('day', now() - interval '6 day'),
    date_trunc('day', now()),
    interval '1 day'
  ) AS bucket
),
agg AS (
  SELECT date_trunc('day', ts) AS bucket, COUNT(*)::int AS clicks
  FROM clicks
  WHERE link_id = $1
    AND ts >= now() - interval '7 day'
  GROUP BY 1
)
SELECT buckets.bucket::timestamptz::text, COALESCE(agg.clicks, 0)::int
FROM buckets
LEFT JOIN agg USING (bucket)
ORDER BY buckets.bucket ASC
`, linkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SeriesPoint{}
	for rows.Next() {
		var t string
		var clicks int
		if err := rows.Scan(&t, &clicks); err != nil {
			return nil, err
		}
		out = append(out, SeriesPoint{T: t, Clicks: clicks})
	}
	return out, nil
}

var reStripScheme = regexp.MustCompile(`^https?://`)

func (a *App) queryTopReferrers(ctx context.Context, linkID string, rng string) ([]RefRow, error) {
	interval := "24 hour"
	if rng == "7d" {
		interval = "7 day"
	}

	rows, err := a.DB.Query(ctx, `
WITH norm AS (
  SELECT
    CASE
      WHEN referer IS NULL OR referer = '' THEN 'direct'
      ELSE regexp_replace(
        split_part(regexp_replace(referer, '^https?://', ''), '/', 1),
        '^www\.', ''
      )
    END AS ref
  FROM clicks
  WHERE link_id = $1
    AND ts >= now() - ($2::text)::interval
)
SELECT ref, COUNT(*)::int AS clicks
FROM norm
GROUP BY ref
ORDER BY clicks DESC
LIMIT 10
`, linkID, interval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RefRow{}
	for rows.Next() {
		var ref string
		var clicks int
		if err := rows.Scan(&ref, &clicks); err != nil {
			return nil, err
		}
		if ref == "" {
			ref = "direct"
		}
		out = append(out, RefRow{Ref: ref, Clicks: clicks})
	}
	return out, nil
}

/* =========================
   Helpers
========================= */

func httpJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func normalizeRefDomain(raw string) string {
	if raw == "" {
		return "direct"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "direct"
	}
	h := strings.ToLower(u.Hostname())
	h = strings.TrimPrefix(h, "www.")
	if h == "" {
		s := reStripScheme.ReplaceAllString(raw, "")
		s = strings.SplitN(s, "/", 2)[0]
		s = strings.TrimPrefix(strings.ToLower(s), "www.")
		if s == "" {
			return "direct"
		}
		return s
	}
	return h
}

func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func randomCode(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = alphabet[mustRandInt(len(alphabet))]
	}
	return string(b)
}

func mustRandInt(max int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(max))
	}
	return int(v.Int64())
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "duplicate key value") ||
		strings.Contains(s, "unique constraint")
}

/* =========================
   main()
========================= */

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (e.g. postgres://app:app@localhost:5432/link_analytics?sslmode=disable)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}

	app := &App{
		DB:  db,
		SSE: NewSSEHub(),
	}

	r := chi.NewRouter()

	// Dev CORS
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-User-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if req.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, req)
		})
	})

	// API routes
	r.Post("/api/links", app.HandleCreateLink)
	r.Get("/api/links", app.HandleListLinks)
	r.Get("/api/links/{code}", app.HandleGetLink)
	r.Get("/api/links/{code}/stats", app.HandleGetStats)
	r.Get("/api/links/{code}/stats/stream", app.SSE.HandleStream(app))

	// redirect route
	r.Get("/r/{code}", app.HandleRedirect)
	r.Head("/r/{code}", app.HandleRedirect)


	addr := ":8080"
	log.Println("listening on", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}
