"use client";

import React, { useMemo, useState } from "react";

const API = process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080";
const USER_ID =
  process.env.NEXT_PUBLIC_USER_ID || "00000000-0000-0000-0000-000000000001";

// Accepts:
// - https://example.com
// - http://example.com
// Also supports users typing "example.com" (we treat it as https://example.com)
function normalizeUrl(input: string) {
  const s = input.trim();
  if (!s) return "";
  if (s.startsWith("http://") || s.startsWith("https://")) return s;
  return `https://${s}`;
}

function validateUrl(input: string): { ok: boolean; message?: string; normalized?: string } {
  const normalized = normalizeUrl(input);
  if (!normalized) return { ok: false, message: "URL is required." };

  try {
    const u = new URL(normalized);

    if (u.protocol !== "http:" && u.protocol !== "https:") {
      return { ok: false, message: "URL must start with http:// or https://." };
    }

    // Must have a hostname like example.com (not just "https://")
    if (!u.hostname || u.hostname.includes(" ")) {
      return { ok: false, message: "Please enter a valid domain (e.g. example.com)." };
    }

    // Quick sanity: block obviously malformed hostnames
    if (!u.hostname.includes(".") && u.hostname !== "localhost") {
      return { ok: false, message: "Domain looks invalid (missing a dot), e.g. example.com." };
    }

    return { ok: true, normalized };
  } catch {
    return { ok: false, message: "Please enter a valid http(s) URL." };
  }
}

export default function CreatePage() {
  const [originalUrl, setOriginalUrl] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<any | null>(null);
  const [touched, setTouched] = useState(false);
  const [isSubmitting, setIsSubmitting] = useState(false);

  const validation = useMemo(() => validateUrl(originalUrl), [originalUrl]);
  const canSubmit = validation.ok && !isSubmitting;

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setTouched(true);
    setError(null);
    setResult(null);

    const v = validateUrl(originalUrl);
    if (!v.ok) {
      setError(v.message || "Invalid URL");
      return;
    }

    setIsSubmitting(true);
    try {
      const res = await fetch(`${API}/api/links`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-User-Id": USER_ID
        },
        body: JSON.stringify({ originalUrl: v.normalized })
      });

      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setError(data?.error || `Failed to create link (HTTP ${res.status})`);
        return;
      }
      setResult(data);
    } catch (e: any) {
      setError(e?.message || "Failed to create link");
    } finally {
      setIsSubmitting(false);
    }
  }

  async function copy() {
    if (!result?.shortUrl) return;
    await navigator.clipboard.writeText(result.shortUrl);
    alert("Copied!");
  }

  return (
    <div>
      <h2>Create Link</h2>

      <form onSubmit={onSubmit} className="card">
        <label className="muted">Original URL</label>
        <div className="spacer" />

        <input
          value={originalUrl}
          onChange={(e) => {
            setOriginalUrl(e.target.value);
            setResult(null);
            setError(null);
          }}
          onBlur={() => setTouched(true)}
          placeholder="https://example.com/..."
          autoComplete="off"
          inputMode="url"
        />

        {/* Inline validation message (client-side) */}
        {touched && originalUrl.trim().length > 0 && !validation.ok && (
          <>
            <div className="spacer" />
            <p className="muted" style={{ color: "#b00", margin: 0 }}>
              {validation.message}
            </p>
          </>
        )}

        <div className="spacer" />

        <div className="row">
          <button type="submit" disabled={!canSubmit}>
            {isSubmitting ? "Creating..." : "Create"}
          </button>
          <a className="muted" href="/links" style={{ alignSelf: "center" }}>
            Go to My Links →
          </a>
        </div>

        {/* Server/API error */}
        {error && (
          <>
            <div className="spacer" />
            <p className="muted" style={{ color: "#b00", margin: 0 }}>
              {error}
            </p>
          </>
        )}

        {/* Helpful hint about auto-normalization */}
        {originalUrl.trim().length > 0 && validation.ok && (
          <>
            <div className="spacer" />
            <p className="muted" style={{ fontSize: 12, margin: 0 }}>
              Will create: <code>{validation.normalized}</code>
            </p>
          </>
        )}
      </form>

      {result && (
        <div style={{ marginTop: 16 }} className="card">
          <div className="muted">Created short link</div>
          <div style={{ fontSize: 18, marginTop: 6 }}>
            <a href={result.shortUrl} target="_blank" rel="noreferrer">
              {result.shortUrl}
            </a>
          </div>
          <div className="spacer" />
          <button className="secondary" onClick={copy}>
            Copy
          </button>
        </div>
      )}
    </div>
  );
}
