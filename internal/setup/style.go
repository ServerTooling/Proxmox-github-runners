package setup

// head is the start of every page: one inline stylesheet, since the host may have no internet and
// the binary serves everything itself. Light and dark follow the browser.
const head = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark"><title>pve-runners</title><style>
:root{--bg:#f4f5f7;--card:#fff;--fg:#1c2330;--muted:#5d6878;--line:#e2e5ea;--soft:#f0f2f5;--accent:#2f6fe4;--accent-fg:#fff;
--ok:#16803c;--ok-bg:#e6f4ea;--warn:#a15c07;--warn-bg:#fdf3e1;--bad:#c0262d;--bad-bg:#fdeaea;--hdr:#18202c;--hdr-fg:#f3f5f8;
--shadow:0 1px 2px rgba(16,24,40,.05),0 1px 3px rgba(16,24,40,.06)}
@media (prefers-color-scheme:dark){:root{--bg:#0e1116;--card:#161b22;--fg:#e4e8ee;--muted:#97a1b0;--line:#29303b;--soft:#1d232c;
--accent:#4c8bf5;--ok:#4cc272;--ok-bg:#163222;--warn:#e3a23b;--warn-bg:#352812;--bad:#f0676d;--bad-bg:#3a1a1c;--hdr:#0a0d12;--shadow:none}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-text-size-adjust:100%}
body.narrow{max-width:40rem;margin:0 auto;padding:2rem 1rem}
h1{font-size:1.5rem;margin:0 0 1rem}h2{font-size:1rem;margin:0 0 .75rem;font-weight:600}
a{color:var(--accent)}code{background:var(--soft);padding:.1rem .3rem;border-radius:4px;font-size:.9em}
small,.muted{color:var(--muted)}small{font-size:.85rem}
pre{background:var(--soft);border:1px solid var(--line);border-radius:8px;padding:.75rem;overflow-x:auto;font-size:12.5px;line-height:1.45;margin:.5rem 0 0;white-space:pre}
fieldset,.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:1.1rem 1.25rem;margin:0 0 1rem;box-shadow:var(--shadow);min-width:0}
legend{font-weight:600;padding:0 .4rem;margin-left:-.4rem}
input,select{font:inherit;color:inherit;background:var(--card);border:1px solid var(--line);border-radius:8px;padding:.45rem .65rem;max-width:100%}
input:focus,select:focus,button:focus-visible{outline:2px solid var(--accent);outline-offset:1px;border-color:var(--accent)}
input::placeholder{color:var(--muted);opacity:.8}
button,.btn{font:inherit;font-weight:500;cursor:pointer;border:1px solid var(--line);background:var(--card);color:var(--fg);border-radius:8px;
padding:.45rem .95rem;white-space:nowrap;box-shadow:var(--shadow)}
button:hover{background:var(--soft)}button:disabled{opacity:.55;cursor:not-allowed}
button.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-fg)}button.primary:hover{filter:brightness(1.07)}
button.danger{color:var(--bad)}button.danger:hover{background:var(--bad-bg)}
button.ghost{background:transparent;box-shadow:none;border-color:rgba(255,255,255,.25);color:inherit;padding:.3rem .8rem}
.err{background:var(--bad-bg);color:var(--bad);border:1px solid color-mix(in srgb,var(--bad) 30%,transparent);padding:.7rem .9rem;border-radius:10px;margin:0 0 1rem}
.warn{background:var(--warn-bg);color:var(--warn);border:1px solid color-mix(in srgb,var(--warn) 30%,transparent);padding:.6rem .9rem;border-radius:10px;margin:0 0 .6rem}
.badge{display:inline-block;font-size:.75rem;font-weight:600;padding:.1rem .5rem;border-radius:999px;background:var(--soft);color:var(--muted);white-space:nowrap}
.badge.ok{background:var(--ok-bg);color:var(--ok)}.badge.bad{background:var(--bad-bg);color:var(--bad)}.badge.run{background:color-mix(in srgb,var(--accent) 15%,transparent);color:var(--accent)}
@media (max-width:720px){fieldset,.card{padding:1rem}}
</style></head>`

const page = head + `<body class="narrow">`
