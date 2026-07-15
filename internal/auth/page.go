package auth

import (
	"html"
	"io"
	"net/http"
	"strings"
)

// xshellzLogoSVG is the xShellz "constellation" mark (inlined so the callback
// page is fully self-contained — no network or asset dependency).
const xshellzLogoSVG = `<svg viewBox="0 0 60 60" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">` +
	`<g stroke="#c3d2f5" stroke-width="3" opacity="0.45">` +
	`<line x1="30" y1="30" x2="30" y2="8"/><line x1="30" y1="30" x2="50.9" y2="23.2"/>` +
	`<line x1="30" y1="30" x2="42.9" y2="47.8"/><line x1="30" y1="30" x2="17.1" y2="47.8"/>` +
	`<line x1="30" y1="30" x2="9.1" y2="23.2"/></g>` +
	`<circle cx="30" cy="8" r="6" fill="#7c5cff"/><circle cx="50.9" cy="23.2" r="6" fill="#14c3a2"/>` +
	`<circle cx="42.9" cy="47.8" r="6" fill="#ffb020"/><circle cx="17.1" cy="47.8" r="6" fill="#ff5d8f"/>` +
	`<circle cx="9.1" cy="23.2" r="6" fill="#38bdf8"/><circle cx="30" cy="30" r="10.5" fill="#2d5cf3"/></svg>`

const checkIconSVG = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"/></svg>`

const crossIconSVG = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 6 18M6 6l12 12"/></svg>`

// browserPageHTML is the template for the post-redirect page. Placeholders are
// substituted via strings.Replacer (avoids fmt verb clashes with the CSS `%`).
const browserPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>xShellz · borg</title>
<style>
  :root{--bg:#0d111d;--card:#141a28;--text:#dbdee1;--muted:#94a3b8;--border:rgba(255,255,255,.09);--blue:#2d5cf3;--green:#14c3a2;--pink:#ff5d8f}
  *{box-sizing:border-box}html,body{height:100%;margin:0}
  body{background:radial-gradient(1200px 600px at 50% -10%,rgba(45,92,243,.12),transparent),var(--bg);color:var(--text);font-family:system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;display:flex;align-items:center;justify-content:center;padding:24px}
  .card{width:100%;max-width:400px;background:var(--card);border:1px solid var(--border);border-radius:20px;padding:40px 32px;text-align:center;box-shadow:0 24px 70px rgba(0,0,0,.5);animation:rise .5s cubic-bezier(.2,.8,.2,1) both}
  @keyframes rise{from{opacity:0;transform:translateY(12px)}}
  .brand{display:flex;align-items:center;justify-content:center;gap:10px;margin-bottom:28px}
  .brand svg{width:34px;height:34px}
  .brand .word{font-weight:700;font-size:18px;letter-spacing:.2px}
  .badge{width:66px;height:66px;border-radius:50%;display:flex;align-items:center;justify-content:center;margin:0 auto 20px}
  .badge svg{width:30px;height:30px}
  .badge.ok{background:rgba(20,195,162,.12);color:var(--green);box-shadow:0 0 0 1px rgba(20,195,162,.32),0 0 28px rgba(20,195,162,.18)}
  .badge.err{background:rgba(255,93,143,.12);color:var(--pink);box-shadow:0 0 0 1px rgba(255,93,143,.32)}
  h1{font-size:20px;margin:0 0 8px;font-weight:700}
  p{margin:0;color:var(--muted);font-size:14px;line-height:1.55}
  .hint{margin-top:24px;font-size:12px;color:var(--muted);opacity:.8}
</style>
</head>
<body>
  <main class="card">
    <div class="brand">{{LOGO}}<span class="word">xShellz</span></div>
    <div class="badge {{BADGE}}">{{ICON}}</div>
    <h1>{{HEADING}}</h1>
    <p>{{DETAIL}}</p>
    <div class="hint">You can close this tab and return to your terminal.</div>
  </main>
</body>
</html>`

// writeBrowserPage renders the on-brand page the user lands on after the OAuth
// redirect, matching the xShellz app look.
func writeBrowserPage(w http.ResponseWriter, ok bool, heading, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	badge, icon := "err", crossIconSVG
	if ok {
		badge, icon = "ok", checkIconSVG
	}
	page := strings.NewReplacer(
		"{{LOGO}}", xshellzLogoSVG,
		"{{BADGE}}", badge,
		"{{ICON}}", icon,
		"{{HEADING}}", html.EscapeString(heading),
		"{{DETAIL}}", html.EscapeString(detail),
	).Replace(browserPageHTML)
	_, _ = io.WriteString(w, page)
}
