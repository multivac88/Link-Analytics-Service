"use client";

import { useEffect, useMemo, useRef, useState } from "react";

const API = process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080";
const USER_ID =
  process.env.NEXT_PUBLIC_USER_ID || "00000000-0000-0000-0000-000000000001";

type SeriesPoint = { t: string; clicks: number };
type RefRow = { ref: string; clicks: number };

type Stats = {
  totalClicks: number;
  uniqueVisitors: number;
  series?: SeriesPoint[];
  topReferrers?: RefRow[];
};

type StreamEvent = {
  deltaClicks?: number;
  deltaUniques?: number;
  ref?: string;
  ts?: string;
};

function formatShortTime(iso: string) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit"
  });
}

function Sparkline({
  series,
  height = 140
}: {
  series: SeriesPoint[];
  height?: number;
}) {
  const width = 700;
  const padding = 16;

  const values = series.map((p) => p.clicks);
  const allZero = values.length > 0 && values.every((v) => v === 0);

  const maxV = Math.max(1, ...values);
  const minV = 0;

  const pts = series.map((p, i) => {
    const x =
      padding +
      (i * (width - padding * 2)) / Math.max(1, series.length - 1);

    // If all zero, keep it visible
    const effectiveMax = allZero ? 1 : maxV;

    const y =
      padding +
      ((effectiveMax - p.clicks) * (height - padding * 2)) /
        Math.max(1, effectiveMax - minV);

    return { x, y };
  });

  const d = pts
    .map((p, i) => `${i === 0 ? "M" : "L"} ${p.x.toFixed(2)} ${p.y.toFixed(2)}`)
    .join(" ");

  return (
    <div
      className="card"
      style={{
        overflowX: "auto",
        minHeight: height + 70,
        background: "rgba(0,0,0,0.02)"
      }}
    >
      <div className="muted" style={{ marginBottom: 8 }}>
        Clicks over time
      </div>

      {allZero && (
        <div className="muted" style={{ fontSize: 12, marginBottom: 8 }}>
          No clicks yet — chart is flat at 0.
        </div>
      )}

      <svg width={width} height={height} style={{ display: "block" }}>
        <line
          x1={padding}
          y1={height - padding}
          x2={width - padding}
          y2={height - padding}
          stroke="rgba(0,0,0,0.25)"
          strokeWidth="2"
        />

        <path d={d} fill="none" stroke="rgba(0,0,0,0.95)" strokeWidth="4" />

        {pts.map((p, idx) => (
          <circle
            key={idx}
            cx={p.x}
            cy={p.y}
            r={allZero ? 3.5 : 2.5}
            fill="rgba(0,0,0,0.7)"
          />
        ))}
      </svg>

      <div className="muted" style={{ marginTop: 8, fontSize: 12 }}>
        Latest: <b>{series[series.length - 1]?.clicks ?? 0}</b> clicks •{" "}
        {series[0]?.t ? formatShortTime(series[0].t) : ""} →{" "}
        {series[series.length - 1]?.t
          ? formatShortTime(series[series.length - 1].t)
          : ""}
      </div>
    </div>
  );
}

export default function LinkStatsPage({ params }: { params: { code: string } }) {
  const code = params.code;

  const [stats, setStats] = useState<Stats | null>(null);
  const [link, setLink] = useState<{ code: string; originalUrl: string } | null>(
    null
  );
  const [error, setError] = useState<string | null>(null);

  const [range, setRange] = useState<"24h" | "7d">("24h");

  const [live, setLive] = useState(true);
  const [liveBadge, setLiveBadge] = useState<"live" | "reconnecting" | "off">(
    "off"
  );

  const esRef = useRef<EventSource | null>(null);
  const pollRef = useRef<any>(null);

  const shortUrl = useMemo(() => `${API}/r/${code}`, [code]);

  function buildStatsUrl(rng: "24h" | "7d") {
    const u = new URL(`${API}/api/links/${code}/stats`);
    u.searchParams.set("range", rng);
    return u.toString();
  }

  async function loadAll() {
    setError(null);

    const [lRes, sRes] = await Promise.all([
      fetch(`${API}/api/links/${code}`, {
        headers: { "X-User-Id": USER_ID },
        cache: "no-store"
      }),
      fetch(buildStatsUrl(range), {
        headers: { "X-User-Id": USER_ID },
        cache: "no-store"
      })
    ]);

    const lData = await lRes.json().catch(() => ({}));
    const sData = await sRes.json().catch(() => ({}));

    if (!lRes.ok) {
      setError(lData?.error || `Failed to load link (HTTP ${lRes.status})`);
      return;
    }
    if (!sRes.ok) {
      setError(sData?.error || `Failed to load stats (HTTP ${sRes.status})`);
      return;
    }

    setLink(lData);
    setStats(sData);
  }

  async function refreshSeriesAndRefs() {
    try {
      const res = await fetch(buildStatsUrl(range), {
        headers: { "X-User-Id": USER_ID },
        cache: "no-store"
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) return;

      setStats((prev) => {
        if (!prev) return data;
        return {
          ...prev,
          // keep totals (they update in realtime via SSE), but refresh buckets/refs for consistency
          series: data.series ?? prev.series,
          topReferrers: data.topReferrers ?? prev.topReferrers,
          uniqueVisitors: data.uniqueVisitors ?? prev.uniqueVisitors,
          totalClicks: data.totalClicks ?? prev.totalClicks
        };
      });
    } catch {
      // ignore
    }
  }

  function applyStreamEvent(ev: StreamEvent) {
    const dClicks = ev.deltaClicks ?? 0;
    const dUniques = ev.deltaUniques ?? 0;
    const ref = (ev.ref || "direct").trim() || "direct";

    setStats((prev) => {
      if (!prev) return prev;

      const next: Stats = {
        ...prev,
        totalClicks: prev.totalClicks + dClicks,
        uniqueVisitors: prev.uniqueVisitors + dUniques
      };

      // update topReferrers locally for instant UI feedback
      const prevRefs = prev.topReferrers ?? [];
      const idx = prevRefs.findIndex((r) => r.ref === ref);

      let nextRefs = [...prevRefs];
      if (idx === -1) nextRefs.push({ ref, clicks: dClicks || 1 });
      else
        nextRefs[idx] = {
          ...nextRefs[idx],
          clicks: nextRefs[idx].clicks + (dClicks || 1)
        };

      nextRefs.sort((a, b) => b.clicks - a.clicks);
      nextRefs = nextRefs.slice(0, 10);

      next.topReferrers = nextRefs;
      return next;
    });
  }

  function startSSE() {
    stopSSE();
    setLiveBadge("reconnecting");

    // EventSource can’t send X-User-Id header → use query param (your Go handler supports it)
    const url = `${API}/api/links/${code}/stats/stream?userId=${encodeURIComponent(
      USER_ID
    )}`;

    const es = new EventSource(url);
    esRef.current = es;

    es.addEventListener("open", () => setLiveBadge("live"));

    es.addEventListener("stats", (e: MessageEvent) => {
      try {
        applyStreamEvent(JSON.parse(e.data));
      } catch {
        // ignore
      }
    });

    es.addEventListener("ping", () => {});

    es.onerror = () => {
      setLiveBadge(live ? "reconnecting" : "off");
      // browser auto-reconnects
    };
  }

  function stopSSE() {
    if (esRef.current) {
      esRef.current.close();
      esRef.current = null;
    }
    setLiveBadge("off");
  }

  useEffect(() => {
    loadAll();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [code, range]);

  useEffect(() => {
    if (!live) {
      stopSSE();
      return;
    }
    startSSE();
    return () => stopSSE();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [code, live]);

  useEffect(() => {
    // Keep chart buckets accurate even if SSE misses anything.
    if (pollRef.current) clearInterval(pollRef.current);
    pollRef.current = setInterval(() => {
      refreshSeriesAndRefs();
    }, 15000);

    return () => {
      if (pollRef.current) clearInterval(pollRef.current);
      pollRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [code, range]);

  const badgeText =
    liveBadge === "live"
      ? "LIVE"
      : liveBadge === "reconnecting"
        ? "RECONNECTING…"
        : "OFF";

  return (
    <div>
      <div style={{ display: "flex", alignItems: "baseline", gap: 10 }}>
        <h2 style={{ marginTop: 0, marginBottom: 0 }}>Link Statistics</h2>
        <span
          className="muted"
          style={{
            fontSize: 12,
            padding: "3px 8px",
            borderRadius: 999,
            border: "1px solid rgba(0,0,0,0.15)"
          }}
        >
          {badgeText}
        </span>
      </div>

      {link && (
        <div className="card">
          <div className="muted">
            <div>
              <b>Code:</b> <code>{link.code}</code>
            </div>
            <div style={{ wordBreak: "break-word" }}>
              <b>Original:</b> {link.originalUrl}
            </div>
            <div>
              <b>Short:</b>{" "}
              <a href={shortUrl} target="_blank">
                {shortUrl}
              </a>
            </div>
          </div>
        </div>
      )}

      <div className="spacer" />

      {/* Controls */}
      <div className="row" style={{ alignItems: "center" }}>
        <div className="card" style={{ flex: 1 }}>
          <div className="muted" style={{ marginBottom: 8 }}>
            Range
          </div>

          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            <button
              className="btn"
              onClick={() => setRange("24h")}
              style={{ opacity: range === "24h" ? 1 : 0.6 }}
            >
              Last 24 hours
            </button>
            <button
              className="btn"
              onClick={() => setRange("7d")}
              style={{ opacity: range === "7d" ? 1 : 0.6 }}
            >
              Last 7 days
            </button>
            <button
              className="btn"
              onClick={() => setLive((v) => !v)}
              style={{
                opacity: live ? 1 : 0.6,
                marginLeft: "auto"
              }}
            >
              Real-time: {live ? "On" : "Off"}
            </button>
            <button className="btn" onClick={() => loadAll()}>
              Refresh
            </button>
          </div>
        </div>
      </div>

      <div className="spacer" />

      {error && (
        <p className="muted" style={{ color: "#b00" }}>
          {error}
        </p>
      )}

      {stats && (
        <>
          <div className="row">
            <div className="card">
              <div className="muted">Total clicks</div>
              <div style={{ fontSize: 28, fontWeight: 700 }}>
                {stats.totalClicks}
              </div>
            </div>

            <div className="card">
              <div className="muted">Unique visitors (by IP)</div>
              <div style={{ fontSize: 28, fontWeight: 700 }}>
                {stats.uniqueVisitors}
              </div>
            </div>
          </div>

          <div className="spacer" />

          {/* Chart */}
          {stats.series && stats.series.length > 1 ? (
            <Sparkline series={stats.series} />
          ) : (
            <div className="card">
              <div className="muted">Clicks over time</div>
              <div className="muted" style={{ marginTop: 6 }}>
                No series data yet.
              </div>
            </div>
          )}

          <div className="spacer" />

          {/* Top Referrers */}
          <div className="card">
            <div style={{ fontWeight: 700 }}>Top 10 referrer sources</div>
            <div className="muted" style={{ marginTop: 4 }}>
              Domain-level referrers, with “direct” for missing referrer.
            </div>

            <div className="spacer" />

            {stats.topReferrers && stats.topReferrers.length > 0 ? (
              <table style={{ width: "100%", borderCollapse: "collapse" }}>
                <thead>
                  <tr>
                    <th
                      style={{
                        textAlign: "left",
                        padding: "8px 6px",
                        borderBottom: "1px solid rgba(0,0,0,0.15)"
                      }}
                      className="muted"
                    >
                      Referrer
                    </th>
                    <th
                      style={{
                        textAlign: "right",
                        padding: "8px 6px",
                        borderBottom: "1px solid rgba(0,0,0,0.15)"
                      }}
                      className="muted"
                    >
                      Clicks
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {stats.topReferrers.map((r) => (
                    <tr key={r.ref}>
                      <td style={{ padding: "8px 6px" }}>{r.ref}</td>
                      <td style={{ padding: "8px 6px", textAlign: "right" }}>
                        {r.clicks}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <div className="muted">No referrer data yet.</div>
            )}
          </div>

          <div className="spacer" />

          <p className="muted">
            Live totals/referrers update via SSE. The chart refreshes every 15s
            to stay consistent with hourly/daily buckets.
          </p>
        </>
      )}
    </div>
  );
}
