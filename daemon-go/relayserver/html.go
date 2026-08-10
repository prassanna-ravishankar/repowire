package relayserver

import (
	"html"
	"strings"
)

const landingPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Repowire</title><style>
body{font:16px system-ui;background:#0a0a0f;color:#c8c8d0;display:grid;place-items:center;min-height:100vh;margin:0}.card{width:min(440px,90vw);text-align:center}input,button{padding:.7rem;border:1px solid #30303f;border-radius:6px;background:#14141f;color:inherit}input{width:65%}button{cursor:pointer}.error{color:#f08080}a{color:#9292c0}
</style></head><body><main class="card"><h1>repowire</h1><p>Mesh network for AI coding agents</p><form action="/auth" method="post"><input name="token" placeholder="rw_…" autocomplete="off"><button>Open</button></form>{{ERROR}}<p><small>Run <code>repowire setup --relay</code> to get your key.</small></p><p><a href="https://docs.repowire.io/">Docs</a> · <a href="https://github.com/prassanna-ravishankar/repowire">GitHub</a></p></main></body></html>`

func landingHTML(code string) string {
	messages := map[string]string{
		"missing_token": "Enter your relay key.",
		"invalid_key":   "That relay key was not recognized.",
		"no_daemon":     "No daemon is connected for that relay key.",
	}
	errorHTML := ""
	if message := messages[code]; message != "" {
		errorHTML = `<p class="error">` + html.EscapeString(message) + `</p>`
	}
	return strings.Replace(landingPage, "{{ERROR}}", errorHTML, 1)
}

const viewerPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>repowire — {{PEER}}</title><style>
body{font:15px system-ui;background:#0a0a0f;color:#ddd;margin:0}.wrap{max-width:760px;margin:auto;padding:2rem}.event{white-space:pre-wrap;border-bottom:1px solid #242433;padding:.7rem 0}.badge{color:#999}form{display:flex;gap:.5rem;margin-top:1rem}textarea{flex:1;min-height:4rem;background:#15151d;color:#eee;border:1px solid #333;border-radius:6px;padding:.6rem}button{padding:.6rem 1rem}
</style></head><body><main class="wrap"><h1>@{{PEER}}</h1><p class="badge">{{PERMS}}</p><div id="events"></div>{{COMPOSE}}</main><script>
const events=document.getElementById('events');const stream=new EventSource('/s/{{SHARE}}/stream');stream.onmessage=e=>{const row=document.createElement('div');row.className='event';try{row.textContent=JSON.stringify(JSON.parse(e.data),null,2)}catch{row.textContent=e.data}events.append(row)};
{{SEND_SCRIPT}}</script></body></html>`

func viewerHTML(token ShareToken) string {
	peer := html.EscapeString(token.PeerName)
	permissions := "read-only"
	compose := ""
	script := ""
	if token.Permissions == "rw" {
		permissions = "read-write"
		compose = `<form id="compose"><textarea id="message" placeholder="Message @` + peer + `"></textarea><button id="send-btn">Send</button></form>`
		script = `document.getElementById('compose').onsubmit=async e=>{e.preventDefault();const box=document.getElementById('message');const text=box.value.trim();if(!text)return;const r=await fetch('/s/` + html.EscapeString(token.ShareID) + `/ask',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({text})});if(r.ok)box.value='';};`
	}
	replacer := strings.NewReplacer(
		"{{PEER}}", peer,
		"{{PERMS}}", permissions,
		"{{SHARE}}", html.EscapeString(token.ShareID),
		"{{COMPOSE}}", compose,
		"{{SEND_SCRIPT}}", script,
	)
	return replacer.Replace(viewerPage)
}
