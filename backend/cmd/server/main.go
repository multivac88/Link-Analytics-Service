package main // This file is the entrypoint of the Go program (executable, not a library)

import (
	"context"      // Lets us pass cancellation + timeouts into DB calls, etc.
	"crypto/rand"  // Cryptographically-secure random bytes/ints (for short code generation)
	"encoding/json"// JSON encode/decode for request/response bodies
	"fmt"          // String formatting (used for SSE message formatting)
	"log"          // Basic logging and log.Fatal
	"math/big"     // Needed because crypto/rand.Int returns big.Int
	"net"          // IP parsing and splitting host:port
	"net/http"     // HTTP server + handlers
	"net/url"      // URL parsing/validation
	"os"           // Read environment variables (DATABASE_URL, APP_BASE_URL)
	"regexp"       // Regex utilities (used for stripping scheme from referrer)
	"strings"      // String helpers (trim, prefix checks, etc.)
	"sync"         // Mutex for concurrent-safe SSE client set
	"time"         // Timeouts, timestamps, ticker for SSE keep-alives

	"github.com/go-chi/chi/v5"        // Router (URL params, route matching)
	"github.com/jackc/pgx/v5"         // Postgres driver errors (pgx.ErrNoRows)
	"github.com/jackc/pgx/v5/pgxpool" // Connection pool for Postgres
)

// App holds shared dependencies used by handlers (DB pool + SSE hub)
type App struct {
	DB  *pgxpool.Pool // Shared Postgres connection pool (thread-safe)
	SSE *SSEHub       // Shared SSE hub (manages connected clients by link code)
}

/* =========================
   DTOs (Data Transfer Objects)
   These structs define the JSON shape you send/receive.
========================= */

// LinkDTO is a minimal response object for GET /api/links/{code}
type LinkDTO struct {
	Code        string `json:"code"`        // Short code
	OriginalURL string `json:"originalUrl"` // Full original URL
}

// LinkItem is the response object for listing links and for create response
type LinkItem struct {
	ID          string `json:"id"`          // Link UUID (as string)
	Code        string `json:"code"`        // Short code
	OriginalURL string `json:"originalUrl"` // Original URL
	CreatedAt   string `json:"createdAt"`   // Created time (text for simplicity)
	ShortURL    string `json:"shortUrl"`    // Fully-qualified short URL (base + /r/{code})
	TotalClicks int    `json:"totalClicks"` // Aggregated clicks for this link
}

// SeriesPoint is one bucket in the time-series chart (hourly or daily)
type SeriesPoint struct {
	T      string `json:"t"`      // Bucket timestamp (text)
	Clicks int    `json:"clicks"` // Click count in that bucket
}

// RefRow is one row in the "Top referrers" table
type RefRow struct {
	Ref    string `json:"ref"`    // Domain name or "direct"
	Clicks int    `json:"clicks"` // Count for that referrer
}

// StatsResponse is the JSON response from GET /api/links/{code}/stats
type StatsResponse struct {
	TotalClicks    int           `json:"totalClicks"`          // Total clicks across all time
	UniqueVisitors int           `json:"uniqueVisitors"`       // Distinct IPs (MVP)
	Series         []SeriesPoint `json:"series,omitempty"`     // Time series buckets for chart
	TopReferrers   []RefRow      `json:"topReferrers,omitempty"`// Top 10 referrer domains
}

// StreamDelta is the JSON payload you push over SSE on every click
type StreamDelta struct {
	DeltaClicks  int    `json:"deltaClicks"`  // Usually 1
	DeltaUniques int    `json:"deltaUniques"` // Usually 0 in this MVP (uniques not computed live)
	Ref          string `json:"ref"`          // Normalized referrer domain or "direct"
	Ts           string `json:"ts"`           // ISO timestamp for event time
}

/* =========================
   SSE Hub
   This manages multiple EventSource connections.
   Clients are grouped by link code, so only viewers of that code get updates.
========================= */

// SSEClient represents one connected browser/client
type SSEClient struct {
	ch chan []byte // Channel used to push SSE messages to this client
}

// SSEHub holds all active clients grouped by short code
type SSEHub struct {
	mu      sync.Mutex                          // Mutex to protect clients map from concurrent access
	clients map[string]map[*SSEClient]struct{}  // code -> set of clients (struct{} saves memory)
}

// NewSSEHub creates and initializes an SSEHub
func NewSSEHub() *SSEHub {
	return &SSEHub{
		clients: make(map[string]map[*SSEClient]struct{}), // initialize the map so we can add entries
	}
}

// add registers a client to receive events for a specific code
func (h *SSEHub) add(code string, c *SSEClient) {
	h.mu.Lock()         // lock because we are modifying shared state
	defer h.mu.Unlock() // ensure we unlock even if something panics/returns early

	// If this is the first client for this code, create the inner map (the "set")
	if _, ok := h.clients[code]; !ok {
		h.clients[code] = make(map[*SSEClient]struct{})
	}

	// Add the client to the set
	h.clients[code][c] = struct{}{}
}

// remove unregisters a client from a specific code
func (h *SSEHub) remove(code string, c *SSEClient) {
	h.mu.Lock()         // lock shared map
	defer h.mu.Unlock() // unlock at end

	// Only do work if that code exists
	if set, ok := h.clients[code]; ok {
		delete(set, c) // remove client from set

		// If no clients remain for this code, remove the code entry entirely
		if len(set) == 0 {
			delete(h.clients, code)
		}
	}
}

// Broadcast sends a server-sent event message to all clients watching a given code
func (h *SSEHub) Broadcast(code string, event string, payload any) {
	b, _ := json.Marshal(payload) // marshal payload as JSON; ignore errors for MVP
	// SSE format: "event: <name>\ndata: <json>\n\n"
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(b)))

	h.mu.Lock() // lock while we copy targets safely
	set := h.clients[code] // get set of clients for this code (may be nil)
	var targets []*SSEClient // copy clients into a slice so we can unlock before sending
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.Unlock() // unlock early so we don’t block other requests while pushing bytes

	// Try to send to each client; if they’re slow, drop the message (best-effort)
	for _, c := range targets {
		select {
		case c.ch <- msg: // send message into client's channel
		default:
			// If channel is full, client is too slow; drop message to avoid blocking
		}
	}
}

// HandleStream is the HTTP handler for:
// GET /api/links/{code}/stats/stream?userId=...
// This keeps a connection open and pushes SSE events down it.
func (h *SSEHub) HandleStream(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")      // read {code} from route
		userID := r.URL.Query().Get("userId")// MVP auth: userId passed via query param (EventSource can't set headers)

		// If no userId, deny
		if userID == "" {
			httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing userId"})
			return
		}

		// Verify the link belongs to that user, so they can't subscribe to others' stats
		ok, err := app.linkBelongsToUser(r.Context(), code, userID)
		if err != nil {
			httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
			return
		}
		if !ok {
			httpJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}

		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream") // tells browser this is SSE
		w.Header().Set("Cache-Control", "no-cache")         // prevent buffering/caching
		w.Header().Set("Connection", "keep-alive")          // keep TCP connection open

		// Need http.Flusher to force the response to be sent immediately
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}

		// Create a new SSE client with a buffered channel
		client := &SSEClient{ch: make(chan []byte, 16)} // buffer 16 messages
		h.add(code, client)                              // register client for this code
		defer h.remove(code, client)                     // ensure cleanup when handler exits

		// Send ping events periodically so proxies/browsers keep connection alive
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()

		ctx := r.Context() // request context cancels when client disconnects
		for {
			select {
			case <-ctx.Done(): // client disconnected or server shutting down
				return

			case <-ticker.C: // every 25s send ping
				_, _ = w.Write([]byte("event: ping\ndata: {}\n\n")) // ping event with empty JSON
				flusher.Flush() // force send

			case msg := <-client.ch: // receive a broadcast message
				_, _ = w.Write(msg) // write raw SSE bytes
				flusher.Flush()     // force send
			}
		}
	}
}

/* =========================
   Handlers
   These are HTTP handlers for your API and redirect endpoint.
========================= */

// HandleListLinks implements: GET /api/links
// It returns all links for the user + total clicks for each link.
func (a *App) HandleListLinks(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-Id") // MVP auth: user id is passed via header
	if userID == "" {
		httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing X-User-Id"})
		return
	}

	// Query links and join clicks to count total clicks per link.
	// LEFT JOIN ensures links with 0 clicks still appear (count becomes 0).
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
	defer rows.Close() // always close rows when done

	// Determine base URL for building shortUrl (e.g., http://localhost:8080)
	base := strings.TrimRight(os.Getenv("APP_BASE_URL"), "/") // remove trailing slash
	if base == "" {
		base = "http://localhost:8080" // fallback for local dev
	}

	out := []LinkItem{} // results to return as JSON array
	for rows.Next() {   // iterate through DB rows
		var it LinkItem // one item

		// Scan DB columns into struct fields (in the same order as SELECT)
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

		// Build short URL (base + /r/{code})
		it.ShortURL = base + "/r/" + it.Code

		// Append to output slice
		out = append(out, it)
	}

	// Return JSON list
	httpJSON(w, http.StatusOK, out)
}

// CreateLinkReq supports both request shapes:
// { "url": "..." } or { "originalUrl": "..." }
type CreateLinkReq struct {
	URL         string `json:"url"`         // alternative field name
	OriginalURL string `json:"originalUrl"` // frontend often uses this
}

// HandleCreateLink implements: POST /api/links
// It creates a short link and returns link info including shortUrl.
func (a *App) HandleCreateLink(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-Id") // get user id from header
	if userID == "" {
		httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing X-User-Id"})
		return
	}

	var req CreateLinkReq
	// Decode JSON request body into req struct
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}

	// Prefer req.URL, else fallback to req.OriginalURL
	raw := strings.TrimSpace(req.URL) // remove surrounding whitespace
	if raw == "" {
		raw = strings.TrimSpace(req.OriginalURL)
	}
	if raw == "" {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": "url is required"})
		return
	}

	// If user typed "example.com", add https://
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}

	// Validate it parses like a URL and has scheme+host
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid url"})
		return
	}

	// Base URL used to build shortUrl for response
	base := strings.TrimRight(os.Getenv("APP_BASE_URL"), "/")
	if base == "" {
		base = "http://localhost:8080"
	}

	// Attempt multiple times in case of rare code collisions
	const maxTries = 12
	var code string
	for i := 0; i < maxTries; i++ {
		code = randomCode(7) // generate a 7-char code

		// Insert row into links table
		_, err := a.DB.Exec(r.Context(),
			`INSERT INTO links (user_id, code, original_url) VALUES ($1, $2, $3)`,
			userID, code, raw,
		)

		// If insert succeeded, return created response
		if err == nil {
			var id string
			var createdAt string

			// Fetch id + createdAt for the inserted row (simple way for MVP)
			if err2 := a.DB.QueryRow(r.Context(),
				`SELECT id::text, created_at::timestamptz::text FROM links WHERE user_id=$1 AND code=$2`,
				userID, code,
			).Scan(&id, &createdAt); err2 != nil {
				httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
				return
			}

			// Respond with LinkItem (same shape used by frontend)
			httpJSON(w, http.StatusCreated, LinkItem{
				ID:          id,
				Code:        code,
				OriginalURL: raw,
				CreatedAt:   createdAt,
				ShortURL:    base + "/r/" + code,
				TotalClicks: 0, // just created, no clicks yet
			})
			return
		}

		// If unique violation occurred (code collision), retry with a new code
		if isUniqueViolation(err) {
			continue
		}

		// Any other DB error -> return failure
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// If we exhausted retries, something is wrong (very unlikely)
	httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to allocate code"})
}

// HandleGetLink implements: GET /api/links/{code}
// Returns the link for the authenticated user
func (a *App) HandleGetLink(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")     // get {code} from route
	userID := r.Header.Get("X-User-Id") // auth header
	if userID == "" {
		httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing X-User-Id"})
		return
	}

	var dto LinkDTO
	// Query link by code + user_id
	err := a.DB.QueryRow(r.Context(),
		`SELECT code, original_url FROM links WHERE code=$1 AND user_id=$2`,
		code, userID,
	).Scan(&dto.Code, &dto.OriginalURL)

	// Handle "not found" vs generic error
	if err != nil {
		if err == pgx.ErrNoRows {
			httpJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// Return JSON
	httpJSON(w, http.StatusOK, dto)
}

// HandleGetStats implements: GET /api/links/{code}/stats?range=24h|7d
func (a *App) HandleGetStats(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")     // get {code} from route
	userID := r.Header.Get("X-User-Id") // auth header
	if userID == "" {
		httpJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing X-User-Id"})
		return
	}

	// Read query param: range
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h" // default range
	}
	if rng != "24h" && rng != "7d" {
		httpJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid range"})
		return
	}

	// Get link ID for this user+code (used for clicks table queries)
	linkID, err := a.getLinkID(r.Context(), code, userID)
	if err != nil {
		if err == pgx.ErrNoRows {
			httpJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// Totals: total clicks + unique visitors (by distinct IP)
	totalClicks, uniqueVisitors, err := a.queryTotals(r.Context(), linkID)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// Series: hourly (24h) or daily (7d) buckets for chart
	series, err := a.querySeries(r.Context(), linkID, rng)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// Top referrers for range window
	topRefs, err := a.queryTopReferrers(r.Context(), linkID, rng)
	if err != nil {
		httpJSON(w, http.StatusInternalServerError, map[string]any{"error": "db error"})
		return
	}

	// Return combined stats
	httpJSON(w, http.StatusOK, StatsResponse{
		TotalClicks:    totalClicks,
		UniqueVisitors: uniqueVisitors,
		Series:         series,
		TopReferrers:   topRefs,
	})
}

// HandleRedirect implements: GET/HEAD /r/{code}
// It records a click, broadcasts SSE delta, then returns 302 redirect.
func (a *App) HandleRedirect(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code") // get {code}

	var linkID string   // DB link UUID
	var original string // destination URL

	// Look up the link by code (no user auth because public redirect)
	err := a.DB.QueryRow(r.Context(),
		`SELECT id, original_url FROM links WHERE code=$1`,
		code,
	).Scan(&linkID, &original)

	// If not found, return 404
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Collect metadata about the request
	ip := clientIP(r)          // visitor IP
	ua := r.UserAgent()        // user agent string
	refFull := r.Referer()     // full referer header
	refDomain := normalizeRefDomain(refFull) // normalize to domain or "direct"

	// Insert a click record into DB (synchronous in this MVP)
	_, _ = a.DB.Exec(r.Context(),
		`INSERT INTO clicks (link_id, ts, ip, ua, referer) VALUES ($1, now(), $2, $3, $4)`,
		linkID, ip, ua, refFull,
	)

	// Broadcast real-time update to stats viewers via SSE
	a.SSE.Broadcast(code, "stats", StreamDelta{
		DeltaClicks:  1,
		DeltaUniques: 0, // MVP: not computing unique delta live
		Ref:          refDomain,
		Ts:           time.Now().UTC().Format(time.RFC3339),
	})

	// Return HTTP 302 redirect to original URL
	http.Redirect(w, r, original, http.StatusFound)
}

/* =========================
   Queries (DB helper methods)
========================= */

// getLinkID returns the link UUID (as text) for a given user+code
func (a *App) getLinkID(ctx context.Context, code string, userID string) (string, error) {
	var id string
	err := a.DB.QueryRow(ctx,
		`SELECT id FROM links WHERE code=$1 AND user_id=$2`,
		code, userID,
	).Scan(&id)
	return id, err
}

// linkBelongsToUser checks if a given code belongs to a user (used for SSE auth)
func (a *App) linkBelongsToUser(ctx context.Context, code string, userID string) (bool, error) {
	var one int
	err := a.DB.QueryRow(ctx,
		`SELECT 1 FROM links WHERE code=$1 AND user_id=$2`,
		code, userID,
	).Scan(&one)

	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil // not found is not a "real" error here
		}
		return false, err // real DB error
	}
	return true, nil
}

// queryTotals returns total clicks + unique visitors (distinct IPs)
func (a *App) queryTotals(ctx context.Context, linkID string) (total int, uniques int, err error) {
	// Total clicks count
	if err = a.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM clicks WHERE link_id=$1`,
		linkID,
	).Scan(&total); err != nil {
		return
	}

	// Unique visitors by distinct IP (MVP approach)
	if err = a.DB.QueryRow(ctx,
		`SELECT COUNT(DISTINCT ip) FROM clicks WHERE link_id=$1 AND ip IS NOT NULL`,
		linkID,
	).Scan(&uniques); err != nil {
		return
	}

	return
}

// querySeries chooses hourly vs daily series based on range
func (a *App) querySeries(ctx context.Context, linkID string, rng string) ([]SeriesPoint, error) {
	if rng == "7d" {
		return a.queryDailySeries(ctx, linkID) // 7d -> 7 daily buckets
	}
	return a.queryHourlySeries(ctx, linkID) // 24h -> 24 hourly buckets
}

// queryHourlySeries returns 24 hourly buckets (including zeros)
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

	out := []SeriesPoint{} // output slice
	for rows.Next() {      // iterate each bucket row
		var t string
		var clicks int
		if err := rows.Scan(&t, &clicks); err != nil {
			return nil, err
		}
		out = append(out, SeriesPoint{T: t, Clicks: clicks}) // add to series
	}
	return out, nil
}

// queryDailySeries returns 7 daily buckets (including zeros)
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

// Regex used by normalizeRefDomain to strip http:// or https:// if needed
var reStripScheme = regexp.MustCompile(`^https?://`)

// queryTopReferrers returns top 10 referrers in the last 24h or 7d
func (a *App) queryTopReferrers(ctx context.Context, linkID string, rng string) ([]RefRow, error) {
	interval := "24 hour" // default
	if rng == "7d" {
		interval = "7 day" // for 7d range
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
   Helpers (small utility functions)
========================= */

// httpJSON writes JSON responses with a status code
func httpJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json") // tell client response is JSON
	w.WriteHeader(status)                              // set HTTP status code
	_ = json.NewEncoder(w).Encode(v)                   // write JSON (ignore error for MVP)
}

// normalizeRefDomain takes a full referrer URL and returns just a domain (or "direct")
func normalizeRefDomain(raw string) string {
	if raw == "" {
		return "direct" // no Referer header
	}

	u, err := url.Parse(raw) // parse as URL
	if err != nil {
		return "direct" // if it doesn't parse, treat as direct
	}

	h := strings.ToLower(u.Hostname())   // extract hostname only (no port)
	h = strings.TrimPrefix(h, "www.")    // normalize away "www."
	if h == "" {
		// fallback: strip scheme manually and take the part before the first "/"
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

// clientIP returns the parsed client IP from RemoteAddr (MVP; not proxy-aware)
func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr) // RemoteAddr is usually "IP:PORT"
	if err != nil {
		return nil // if it isn't in IP:PORT format
	}
	return net.ParseIP(host) // parse the IP string into net.IP
}

// randomCode generates an n-character short code using crypto random
func randomCode(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" // allowed chars
	b := make([]byte, n) // allocate bytes for code
	for i := 0; i < n; i++ {
		b[i] = alphabet[mustRandInt(len(alphabet))] // pick random index into alphabet
	}
	return string(b) // convert bytes to string code
}

// mustRandInt returns a secure random integer in [0, max)
func mustRandInt(max int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(max))) // secure random int
	if err != nil {
		// fallback if crypto fails (rare)
		return int(time.Now().UnixNano() % int64(max))
	}
	return int(v.Int64()) // convert big.Int to int
}

// isUniqueViolation checks error text to see if a unique constraint failed (code collision)
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error() // string form of error
	return strings.Contains(s, "duplicate key value") || // common postgres error text
		strings.Contains(s, "unique constraint")
}

/* =========================
   main()
   Creates DB pool, router, and starts HTTP server.
========================= */

func main() {
	dsn := os.Getenv("DATABASE_URL") // read DB connection string from env
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (e.g. postgres://app:app@localhost:5432/link_analytics?sslmode=disable)")
	}

	// Create a context with timeout so DB pool creation doesn't hang forever
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := pgxpool.New(ctx, dsn) // create a connection pool
	if err != nil {
		log.Fatal(err) // crash fast if DB config is invalid
	}

	// Create application container holding shared dependencies
	app := &App{
		DB:  db,
		SSE: NewSSEHub(),
	}

	r := chi.NewRouter() // create router

	// CORS middleware for dev (so frontend can call backend across ports)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")              // allow all origins (dev only)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-User-Id") // allow these headers
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")     // allow these methods
			if req.Method == "OPTIONS" { // browser preflight request
				w.WriteHeader(http.StatusNoContent) // return 204 quickly
				return
			}
			next.ServeHTTP(w, req) // continue to actual handler
		})
	})

	// API routes
	r.Post("/api/links", app.HandleCreateLink)                  // create a link
	r.Get("/api/links", app.HandleListLinks)                    // list links
	r.Get("/api/links/{code}", app.HandleGetLink)               // get one link by code
	r.Get("/api/links/{code}/stats", app.HandleGetStats)        // stats for one link
	r.Get("/api/links/{code}/stats/stream", app.SSE.HandleStream(app)) // SSE stream for live stats

	// Redirect route (GET and HEAD) to support your load tests (HEAD expects 302 too)
	r.Get("/r/{code}", app.HandleRedirect)  // redirect
	r.Head("/r/{code}", app.HandleRedirect) // HEAD redirect

	addr := ":8080"                          // listen on port 8080
	log.Println("listening on", addr)        // log for visibility
	log.Fatal(http.ListenAndServe(addr, r))  // start server (blocks forever or returns error)
}
