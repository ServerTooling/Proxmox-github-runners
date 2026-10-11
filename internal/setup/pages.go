package setup

import "html/template"

// logo is a small server-rack mark for the header.
const logo = `<svg width="26" height="26" viewBox="0 0 24 24" fill="none" aria-hidden="true"><rect x="3" y="3.5" width="18" height="7" rx="2" fill="#2f6fe4"/>
<rect x="3" y="13.5" width="18" height="7" rx="2" fill="#2f6fe4" opacity=".6"/><circle cx="7" cy="7" r="1.2" fill="#fff"/><circle cx="7" cy="17" r="1.2" fill="#fff"/>
<rect x="11" y="6.3" width="7" height="1.4" rx=".7" fill="#fff" opacity=".8"/><rect x="11" y="16.3" width="7" height="1.4" rx=".7" fill="#fff" opacity=".8"/></svg>`

var loginTmpl = template.Must(template.New("login").Parse(page + `
<div style="display:flex;align-items:center;gap:.6rem;margin:2rem 0 1.25rem">` + logo + `<h1 style="margin:0">pve-runners <span class="muted" style="font-weight:400">on {{.Host}}</span></h1></div>
{{if .Error}}<p class="err">Wrong user or password, or the account cannot change this host (it needs Sys.Modify).</p>{{end}}
<form method="post" action="/login" class="card"><h2>Log in with your Proxmox account</h2>
<div class="f"><div class="l"><label for="user">User</label></div><div class="c"><input id="user" name="user" value="root@pam" autocomplete="username" required></div></div>
<div class="f"><div class="l"><label for="password">Password</label></div><div class="c"><input id="password" name="password" type="password" autocomplete="current-password" required></div></div>
<p style="margin:.75rem 0 0"><button class="primary">Log in</button></p></form></body></html>`))

var savedTmpl = template.Must(template.New("saved").Parse(page + `<meta http-equiv="refresh" content="15;url=/">
<div class="card"><h1>Saved</h1><p class="muted" style="margin:0">The service is restarting with the new settings. This page reloads in 15 seconds.</p></div></body></html>`))

// The page is one document with tabs: without JavaScript every panel shows and the tabs are links to
// them. Inputs on hidden panels are still part of the settings form, so one save covers every tab.
var settingsTmpl = template.Must(template.New("settings").Parse(head + `<body{{if .Busy}} data-busy{{end}} data-default="{{if .Error}}t-settings{{else}}t-overview{{end}}">
{{if .Busy}}<noscript><meta http-equiv="refresh" content="5"></noscript>{{end}}
<header class="top"><div class="in"><div class="brand">` + logo + `<span>pve-runners</span>
<small>{{.Cfg.HostID}}{{with .Report}} · {{.Version}}{{end}}</small></div>
<form method="post" action="/logout"><button class="ghost">Log out</button></form></div></header>
<nav class="tabs" data-nav><div class="in"><a href="#t-overview">Overview</a><a href="#t-actions">Actions{{if .Busy}} <span class="badge run">running</span>{{end}}</a><a href="#t-settings">Settings</a></div></nav>
<main class="wrap" data-panels>

<section class="panel" id="t-overview">
{{if .StatusErr}}<p class="err">Status unavailable: {{.StatusErr}}</p>{{end}}
{{with .Report}}{{range .Warnings}}<p class="warn"><b>Warning:</b> {{.}}</p>{{end}}
<div class="stats">
<div class="stat"><span>Runners</span><b>{{len .Runners}} <span>/ {{.Max}}</span></b><div class="meter"><i style="width:{{.UsedPercent}}%"></i></div></div>
<div class="stat"><span>Version</span><b>{{.Version}}</b></div>
<div class="stat"><span>Host</span><b>{{.Host}}</b></div>
<div class="stat"><span>Container storage</span><b>{{if .Storage}}{{.Storage}}{{else}}–{{end}}</b></div>
</div>
<div class="card"><h2>Runners</h2>{{if .Runners}}<div class="scroll"><table><thead><tr><th>ID</th><th class="hide-sm">Name</th><th>Status</th><th class="hide-sm">Pool</th><th>Owner</th><th>Age</th></tr></thead><tbody>
{{range .Runners}}<tr><td class="mono">{{.ID}}</td><td class="mono hide-sm">{{.Name}}</td><td><span class="badge{{if eq .Status "running"}} ok{{end}}">{{.Status}}</span></td><td class="hide-sm">{{.Pool}}</td><td>{{.Owner}}</td><td>{{.Age}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="empty">No runners right now. They start when a job is queued.</p>{{end}}
{{if .Pending}}<p style="margin:.75rem 0 0"><small>Next cleanup: {{range $i, $p := .Pending}}{{if $i}}, {{end}}{{$p}}{{end}}</small></p>{{end}}</div>
{{end}}
<div class="grid">
<div class="card"><h2>Orgs and accounts</h2>{{if .Cfg.Targets}}<ul class="list">{{range .Cfg.Targets}}<li><span>{{if .Org}}{{.Org}} <small>org</small>{{else}}{{.User}} <small>account</small>{{end}}</span>
<small>{{range $j, $p := .Pools}}{{if $j}}, {{end}}{{$p}}{{end}}</small></li>{{end}}</ul>{{else}}<p class="empty">None yet. Add one under Settings.</p>{{end}}</div>
<div class="card"><h2>Pools</h2><ul class="list">{{range $name, $p := .Cfg.Pools}}<li><span>{{$name}}</span>
<small>{{$p.Cores}} cores · {{$p.MemoryMB}} MB · max {{$p.Max}}{{if $p.Warm}} · {{$p.Warm}} warm{{end}}</small></li>{{end}}</ul></div>
</div>
</section>

<section class="panel" id="t-actions"><div class="cols">
{{range .Actions}}<div class="card"><h2>{{.Name}}</h2>
{{range .Actions}}<form method="post" action="/action/{{.Name}}" id="action-{{.Name}}" class="act"{{if .Confirm}} onsubmit="return confirm('{{.Confirm}}')"{{end}}>
{{$run := and .Last .Last.Running}}
<div class="row"><div><div class="t">{{.Label}}</div>{{if .Help}}<small>{{.Help}}</small>{{end}}</div>
{{if not .Args}}<button class="{{if .Confirm}}danger{{end}}"{{if $run}} disabled{{end}}>Run</button>{{end}}</div>
{{if .Args}}<div class="args">{{range .Args}}<label>{{.}}<input name="{{.}}" autocomplete="off"></label>{{end}}
<button class="{{if .Confirm}}danger{{end}}" style="align-self:flex-end"{{if $run}} disabled{{end}}>Run</button></div>{{end}}
{{with .Last}}<div class="last">{{if .Running}}<span class="badge run">running</span> since {{.Started}}…{{else}}{{if .Err}}<span class="badge bad">failed</span>{{else}}<span class="badge ok">done</span>{{end}} ran {{.Started}}–{{.Ended}}{{end}}</div>
{{if or .Output .Err}}<pre>{{.Output}}{{if .Err}}error: {{.Err}}{{end}}</pre>{{end}}{{end}}
</form>{{end}}</div>{{end}}
</div></section>

<section class="panel" id="t-settings">
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/settings" id="settings"><div class="layout">
<nav class="side" data-nav><a href="#s-orgs">Orgs and accounts</a><a href="#s-pools">Pools</a>{{range .Sections}}<a href="#{{.ID}}">{{.Title}}</a>{{end}}</nav>
<div><div data-panels>
<section class="panel card" id="s-orgs"><h2>Orgs and accounts</h2>
<p class="muted" style="margin-top:0">Each has its own private GitHub App. Adding one saves and restarts right away.</p>
<div class="inline"><input name="org" form="add-org" placeholder="your-org" aria-label="Org"><button form="add-org">Add org</button></div>
<div class="inline"><input name="user" form="add-user" placeholder="or your GitHub username" aria-label="Your username"><button form="add-user">Add my account</button></div>
{{range $i, $t := .Cfg.Targets}}<div class="card" style="margin:1rem 0 0;box-shadow:none"><h2>{{if .Org}}Org {{.Org}}{{else}}User {{.User}}{{end}}</h2>
<div class="f"><div class="l"><label for="target.{{$i}}.pools">Pools</label><small>Comma-separated pool names.</small></div><div class="c"><input id="target.{{$i}}.pools" name="target.{{$i}}.pools" value="{{range $j, $p := .Pools}}{{if $j}}, {{end}}{{$p}}{{end}}"></div></div>
{{if .Org}}<div class="f"><div class="l"><label for="target.{{$i}}.runner_group">Runner group</label></div><div class="c"><input id="target.{{$i}}.runner_group" name="target.{{$i}}.runner_group" value="{{.RunnerGroup}}" placeholder="Default"></div></div>
{{else}}<div class="f"><div class="l"><label for="target.{{$i}}.repos">Repositories</label><small>Comma-separated; empty for all private ones.</small></div><div class="c"><input id="target.{{$i}}.repos" name="target.{{$i}}.repos" value="{{range $j, $r := .Repos}}{{if $j}}, {{end}}{{$r}}{{end}}"></div></div>{{end}}
<label class="check"><input type="checkbox" name="target.{{$i}}.remove"> Remove on save (the GitHub App stays until you delete it on GitHub)</label>
</div>{{end}}</section>
<section class="panel" id="s-pools">
{{range .Pools}}<div class="card"><h2>Pool {{.Name}}</h2>{{range .Fields}}{{template "field" .}}{{end}}
<label class="check"><input type="checkbox" name="pool.{{.Name}}.delete"> Delete this pool on save</label></div>{{end}}
<div class="card"><h2>New pool</h2><div class="f"><div class="l"><label for="new_pool">Name</label>
<small>Starts from the published image with its name as the label; edit it after saving.</small></div><div class="c"><input id="new_pool" name="new_pool" placeholder="optional"></div></div></div>
</section>
{{range .Sections}}<section class="panel card" id="{{.ID}}"><h2>{{.Title}}</h2>{{range .Fields}}{{template "field" .}}{{end}}
{{if eq .Title "Alerts"}}<div class="f"><div class="l"><label for="notify_url">Webhook URL</label><small>ntfy, Discord or Slack; kept in its own file.</small>
{{if $.NotifySet}}<label class="check"><input type="checkbox" name="notify_url_clear"> turn off</label>{{end}}</div>
<div class="c"><input id="notify_url" name="notify_url" type="password" autocomplete="off" placeholder="{{if $.NotifySet}}set; leave empty to keep{{else}}ntfy, Discord or Slack URL{{end}}"></div></div>{{end}}
{{if eq .Title "Logs (Grafana Loki)"}}<div class="f"><div class="l"><label for="loki_token">Token</label><small>Kept in its own file.</small>
{{if $.LokiSet}}<label class="check"><input type="checkbox" name="loki_token_clear"> remove</label>{{end}}</div>
<div class="c"><input id="loki_token" name="loki_token" type="password" autocomplete="off" placeholder="{{if $.LokiSet}}set; leave empty to keep{{else}}access policy token with logs:write{{end}}"></div></div>{{end}}
</section>{{end}}
</div>
<div class="savebar"><span class="muted">Saves every tab and restarts the service.</span><button class="primary">Save and restart</button></div>
</div></div></form>
<form method="post" action="/app/new" id="add-org"></form><form method="post" action="/app/new" id="add-user"></form>
</section>
</main>
<script>
(function () {
  var dirty = false, settings = document.getElementById('settings');
  settings.addEventListener('input', function () { dirty = true; });
  function activate(p) {
    var kids = p.parentElement.children;
    for (var i = 0; i < kids.length; i++) if (kids[i].classList.contains('panel')) kids[i].hidden = kids[i] !== p;
  }
  function mark() {
    document.querySelectorAll('[data-nav] a').forEach(function (a) {
      var p = document.getElementById(a.hash.slice(1));
      a.classList.toggle('on', !!p && !p.hidden);
    });
  }
  function show(id) {
    var el = id && document.getElementById(id);
    if (!el) return false;
    for (var p = el; p; p = p.parentElement) if (p.classList && p.classList.contains('panel')) activate(p);
    mark();
    return true;
  }
  document.querySelectorAll('[data-panels]').forEach(function (g) { activate(g.querySelector(':scope > .panel')); });
  var h = location.hash.slice(1);
  if (!show(h)) show(document.body.dataset.default);
  else if (h.indexOf('action-') === 0) document.getElementById(h).scrollIntoView({block: 'center'});
  // A tab's anchor is the top of the page, not where the browser would scroll to.
  else addEventListener('load', function () { setTimeout(function () { window.scrollTo(0, 0); }); });
  document.querySelectorAll('[data-nav] a').forEach(function (a) {
    a.addEventListener('click', function (e) {
      e.preventDefault();
      show(a.hash.slice(1));
      history.replaceState(null, '', a.hash);
      if (a.closest('.tabs')) window.scrollTo(0, 0);
      else if (settings.getBoundingClientRect().top < 0) settings.scrollIntoView();
    });
  });
  // While an action runs, reload to show its output, unless that would lose edits to the settings.
  if (document.body.hasAttribute('data-busy')) setInterval(function () {
    if (!dirty && document.getElementById('t-settings').hidden) location.reload();
  }, 5000);
})();
</script></body></html>
{{define "field"}}<div class="f"><div class="l"><label for="{{.Name}}">{{.Label}}</label>{{if .Help}}<small>{{.Help}}</small>{{end}}</div><div class="c">
{{- if eq .Kind "bool"}}<input type="checkbox" class="switch" id="{{.Name}}" name="{{.Name}}"{{if eq .Value "true"}} checked{{end}}>
{{- else if eq .Kind "select"}}<select id="{{.Name}}" name="{{.Name}}">{{$v := .Value}}{{range .Options}}<option{{if eq . $v}} selected{{end}}>{{.}}</option>{{end}}</select>
{{- else}}<input id="{{.Name}}" name="{{.Name}}" value="{{.Value}}"{{if eq .Kind "number"}} inputmode="numeric"{{end}}>{{end}}</div></div>{{end}}`))
