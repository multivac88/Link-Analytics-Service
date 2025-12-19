import "./globals.css";

export const metadata = {
  title: "Link Analytics MVP",
  description: "URL shortener with analytics"
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <div className="container">
          <header className="header">
            <div className="brand">Link Analytics</div>
            <nav className="nav">
              <a href="/create">Create</a>
              <a href="/links">My Links</a>
            </nav>
          </header>
          <main className="main">{children}</main>
        </div>
      </body>
    </html>
  );
}
