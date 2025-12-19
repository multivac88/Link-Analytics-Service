"use client";

import { useEffect, useMemo, useRef, useState } from "react";

const API = process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080";
const USER_ID =
  process.env.NEXT_PUBLIC_USER_ID || "00000000-0000-0000-0000-000000000001";

type LinkRow = {
  code: string;
  originalUrl: string;
  createdAt: string;
  totalClicks: number;
};

export default function LinksPage() {
  const [links, setLinks] = useState<LinkRow[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(false);

  // auto-refresh controls
  const [auto, setAuto] = useState(true);
  const [autoBadge, setAutoBadge] = useState<"on" | "off">("on");
  const pollRef = useRef<any>(null);

  const totalLinks = useMemo(() => links.length, [links]);

  async function load() {
    setError(null);
    setIsLoading(true);

    try {
      const res = await fetch(`${API}/api/links`, {
        headers: { "X-User-Id": USER_ID },
        cache: "no-store"
      });

      const data = await res.json().catch(() => ([]));

      if (!res.ok) {
        setError(data?.error || `Failed to load links (HTTP ${res.status})`);
        return;
      }

      setLinks(Array.isArray(data) ? data : []);
    } catch (e: any) {
      setError(e?.message || "Failed to load links");
    } finally {
      setIsLoading(false);
    }
  }

  useEffect(() => {
    load();
  }, []);

  useEffect(() => {
    if (pollRef.current) clearInterval(pollRef.current);

    if (!auto) {
      setAutoBadge("off");
      return;
    }

    setAutoBadge("on");
    // Poll every 5s so clicks show up without refresh
    pollRef.current = setInterval(() => {
      load();
    }, 5000);

    return () => {
      if (pollRef.current) clearInterval(pollRef.current);
      pollRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [auto]);

  return (
    <div>
      <div className="row" style={{ alignItems: "center" }}>
        <div style={{ display: "flex", alignItems: "baseline", gap: 10 }}>
          <h2 style={{ margin: 0 }}>My Links</h2>

          <span
            className="muted"
            style={{
              fontSize: 12,
              padding: "3px 8px",
              borderRadius: 999,
              border: "1px solid rgba(0,0,0,0.15)"
            }}
            title="Total links"
          >
            Total: <b>{totalLinks}</b>
          </span>

          <span
            className="muted"
            style={{
              fontSize: 12,
              padding: "3px 8px",
              borderRadius: 999,
              border: "1px solid rgba(0,0,0,0.15)"
            }}
            title="Auto refresh status"
          >
            Auto: <b>{autoBadge.toUpperCase()}</b>
          </span>

          {isLoading && (
            <span className="muted" style={{ fontSize: 12 }}>
              Loading…
            </span>
          )}
        </div>

        <div style={{ textAlign: "right", display: "flex", gap: 8 }}>
          <button
            className="secondary"
            onClick={() => setAuto((v) => !v)}
            style={{ opacity: auto ? 1 : 0.7 }}
            title="Toggle auto refresh"
          >
            Auto refresh: {auto ? "On" : "Off"}
          </button>
          <button className="secondary" onClick={load} title="Manual refresh">
            Refresh
          </button>
        </div>
      </div>

      <div className="spacer" />

      {error && (
        <p className="muted" style={{ color: "#b00" }}>
          {error}
        </p>
      )}

      <table className="table">
        <thead>
          <tr>
            <th>Code</th>
            <th>Original URL</th>
            <th>Total Clicks</th>
            <th>Created</th>
          </tr>
        </thead>
        <tbody>
          {links.map((l) => (
            <tr
              key={l.code}
              style={{ cursor: "pointer" }}
              onClick={() => (window.location.href = `/links/${l.code}`)}
              title="Open link statistics"
            >
              <td>
                <code>{l.code}</code>
              </td>
              <td style={{ maxWidth: 520, wordBreak: "break-word" }}>
                {l.originalUrl}
              </td>
              <td>{l.totalClicks}</td>
              <td className="muted">{new Date(l.createdAt).toLocaleString()}</td>
            </tr>
          ))}

          {links.length === 0 && !error && (
            <tr>
              <td colSpan={4} className="muted">
                No links yet. Create one at <a href="/create">/create</a>.
              </td>
            </tr>
          )}
        </tbody>
      </table>

      <div className="spacer" />

      <p className="muted">
        This page auto-refreshes every 5 seconds (toggle it off if you want).
      </p>
    </div>
  );
}
