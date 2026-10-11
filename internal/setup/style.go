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
/* Settings page */
.top{background:var(--hdr);color:var(--hdr-fg)}
.top .in{max-width:72rem;margin:0 auto;padding:.8rem 1rem;display:flex;align-items:center;gap:.75rem}
.brand{display:flex;align-items:center;gap:.6rem;font-weight:650;font-size:1.05rem;margin-right:auto;min-width:0}
.brand svg{flex:none}.brand small{color:#a9b3c2;font-weight:400;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.tabs{position:sticky;top:0;z-index:5;background:var(--card);border-bottom:1px solid var(--line)}
.tabs .in{max-width:72rem;margin:0 auto;padding:0 .5rem;display:flex;overflow-x:auto;scrollbar-width:none}
.tabs a{padding:.8rem .9rem;color:var(--muted);text-decoration:none;font-weight:500;border-bottom:2px solid transparent;white-space:nowrap}
.tabs a:hover{color:var(--fg)}.tabs a.on{color:var(--accent);border-bottom-color:var(--accent)}
.wrap{max-width:72rem;margin:0 auto;padding:1.25rem 1rem 2rem}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(9.5rem,1fr));gap:.75rem;margin-bottom:1rem}
.stat{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:.85rem 1rem;box-shadow:var(--shadow);min-width:0}
.stat b{display:block;font-size:1.45rem;font-weight:650;line-height:1.2;overflow-wrap:anywhere}.stat span{color:var(--muted);font-size:.82rem}
.meter{height:6px;background:var(--soft);border-radius:3px;margin-top:.5rem;overflow:hidden}.meter i{display:block;height:100%;background:var(--accent)}
table{width:100%;border-collapse:collapse;font-size:.9rem}
th{text-align:left;font-size:.72rem;letter-spacing:.04em;text-transform:uppercase;color:var(--muted);font-weight:600;padding:.4rem .6rem}
td{padding:.55rem .6rem;border-top:1px solid var(--line);white-space:nowrap}td.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.85rem}
.scroll{overflow-x:auto;margin:0 -.6rem}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(20rem,1fr));gap:1rem;align-items:start}.grid>.card{margin:0}
.cols{columns:20rem 3;column-gap:1rem}.cols>.card{break-inside:avoid}
.list{margin:0;padding:0;list-style:none}.list li{display:flex;justify-content:space-between;gap:1rem;padding:.5rem 0;border-top:1px solid var(--line)}
.list li:first-child{border-top:0;padding-top:0}
.act{padding:.85rem 0;border-top:1px solid var(--line)}.act:first-of-type{border-top:0;padding-top:0}.act:last-child{padding-bottom:0}
.act .row{display:flex;align-items:center;justify-content:space-between;gap:.75rem}
.act .t{font-weight:550}.act .args{display:flex;flex-wrap:wrap;gap:.5rem;margin-top:.6rem}
.act .args label{display:flex;flex-direction:column;font-size:.8rem;color:var(--muted);flex:1 1 6rem}.act .args input{width:100%}
.act .last{margin-top:.5rem;font-size:.82rem;color:var(--muted);display:flex;gap:.5rem;align-items:center}
.layout{display:grid;grid-template-columns:13rem 1fr;gap:1.25rem;align-items:start}
.side{position:sticky;top:3.6rem;display:flex;flex-direction:column;gap:.15rem}
.side a{padding:.45rem .75rem;border-radius:8px;color:var(--muted);text-decoration:none;font-weight:500}
.side a:hover{background:var(--soft);color:var(--fg)}.side a.on{background:color-mix(in srgb,var(--accent) 12%,transparent);color:var(--accent)}
.f{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,20rem);gap:.35rem 1.5rem;align-items:center;padding:.8rem 0;border-top:1px solid var(--line)}
.card h2+.f,.card p+.f,.f:first-child{border-top:0}
.f .l{display:flex;flex-direction:column}.f label{font-weight:500}.f .c input:not([type=checkbox]),.f .c select{width:100%}
.check{display:flex;align-items:center;gap:.5rem;font-size:.9rem;color:var(--muted);margin-top:.5rem}
.inline{display:flex;gap:.5rem;flex-wrap:wrap;margin:.5rem 0}.inline input{flex:1 1 10rem}
input.switch{appearance:none;-webkit-appearance:none;width:2.6rem;height:1.5rem;border-radius:1rem;background:var(--line);position:relative;
cursor:pointer;border:0;padding:0;transition:background .15s;flex:none}
input.switch::before{content:"";position:absolute;top:.2rem;left:.2rem;width:1.1rem;height:1.1rem;border-radius:50%;background:#fff;
box-shadow:0 1px 2px rgba(0,0,0,.3);transition:left .15s}
input.switch:checked{background:var(--accent)}input.switch:checked::before{left:1.3rem}
.savebar{position:sticky;bottom:0;z-index:4;display:flex;align-items:center;justify-content:space-between;gap:1rem;margin-top:1rem;
padding:.75rem 1rem;background:var(--card);border:1px solid var(--line);border-radius:12px;box-shadow:0 -4px 16px rgba(16,24,40,.08)}
.empty{color:var(--muted);text-align:center;padding:1.25rem 0}
@media (max-width:720px){
.wrap{padding:1rem .75rem 1.5rem}.layout{grid-template-columns:1fr;gap:.75rem}
.side{position:sticky;top:2.9rem;z-index:3;flex-direction:row;overflow-x:auto;scrollbar-width:none;background:var(--bg);
margin:0 -.75rem;padding:.5rem .75rem;gap:.4rem}
.side a{border:1px solid var(--line);background:var(--card);border-radius:999px;padding:.3rem .8rem;white-space:nowrap;font-size:.88rem}
td,th{padding-left:.45rem;padding-right:.45rem}.f{grid-template-columns:1fr}.grid{grid-template-columns:1fr}.hide-sm{display:none}
fieldset,.card{padding:1rem}.tabs a{padding:.75rem .8rem}
.savebar span{font-size:.82rem}}
</style></head>`

const page = head + `<body class="narrow">`
