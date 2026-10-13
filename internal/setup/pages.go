package setup

import "html/template"

// logo is a small server-rack mark for the header.
const logo = `<svg width="26" height="26" viewBox="0 0 24 24" fill="none" aria-hidden="true"><rect x="3" y="3.5" width="18" height="7" rx="2" fill="#2f6fe4"/>
<rect x="3" y="13.5" width="18" height="7" rx="2" fill="#2f6fe4" opacity=".6"/><circle cx="7" cy="7" r="1.2" fill="#fff"/><circle cx="7" cy="17" r="1.2" fill="#fff"/>
<rect x="11" y="6.3" width="7" height="1.4" rx=".7" fill="#fff" opacity=".8"/><rect x="11" y="16.3" width="7" height="1.4" rx=".7" fill="#fff" opacity=".8"/></svg>`

// serviceTmpl is all a browser gets from the service: the settings moved to the app.
var serviceTmpl = template.Must(template.New("service").Parse(page + `
<div style="display:flex;align-items:center;gap:.6rem;margin:2rem 0 1.25rem">` + logo + `<h1 style="margin:0">pve-runners <span class="muted" style="font-weight:400">on {{.Host}}</span></h1></div>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<div class="card"><p style="margin:0">Status, settings and actions are in the Dell Server Tools app: open this Proxmox server there
and tap <b>GitHub runners (pve-runners)</b>. Adding an org or account starts in the app too.</p></div></body></html>`))

// savedTmpl ends adding an org or account, which happens in the browser.
var savedTmpl = template.Must(template.New("saved").Parse(page + `
<div class="card"><h1>Added</h1><p class="muted" style="margin:0">The service is restarting with it. You can close this page and go back to the app.</p></div></body></html>`))
