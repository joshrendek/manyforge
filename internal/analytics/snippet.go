// Package analytics implements the tenant-facing web analytics slice of manyforge-as0 on top of
// the manyforge-p20 storage foundation: the embeddable snippet, the principal-less collect
// endpoint, and the authenticated read API a dashboard renders.
//
// Privacy is the design constraint, not a feature. There are no cookies, no persistent
// identifiers, and no cross-site profiles. A visitor is counted via a hash salted with a per-day
// secret that is deleted once past retention, so the count is meaningful within a day and
// impossible to correlate across days.
package analytics

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// snippetJS is the embeddable tracker. Constraints that shaped it:
//
//   - No dependencies, no build step, small enough to inline-read before trusting.
//   - sendBeacon so a pageview survives the tab closing; XHR only as a fallback.
//   - The beacon is sent as text/plain, NOT application/json. application/json is not a CORS
//     "simple" content type, so it forces a preflight OPTIONS on every pageview from every
//     embedding site — doubling requests and failing outright if preflight is ever blocked.
//     text/plain sends the identical bytes with no preflight; the server parses the body by
//     shape, not by Content-Type.
//   - Only pushState and popstate are tracked. replaceState is deliberately NOT wrapped: SPAs
//     routinely use it to rewrite query strings, and treating those as pageviews inflates counts.
//   - A repeated path is skipped, so a framework that fires several navigations for one screen
//     does not multiply the count.
//   - Sends the referrer HOST only, and never for same-host navigation.
const snippetJS = `(function(){
try{
var s=document.currentScript;
if(!s){var a=document.getElementsByTagName('script');s=a[a.length-1];}
if(!s)return;
var k=s.getAttribute('data-key');
if(!k)return;
var ep;
try{ep=(s.getAttribute('data-host')||new URL(s.src).origin)+'/a/e';}catch(e){return;}
if(navigator.doNotTrack==='1'||window.doNotTrack==='1'||navigator.msDoNotTrack==='1')return;
var h=location.hostname;
if(location.protocol==='file:'||h==='localhost'||h==='127.0.0.1'||h==='::1')return;
var last=null;
function send(){
var p=location.pathname||'/';
if(p===last)return;
last=p;
var r='';
try{if(document.referrer){var u=new URL(document.referrer);if(u.hostname&&u.hostname!==location.hostname)r=u.hostname;}}catch(e){}
var b=JSON.stringify({k:k,p:p,r:r});
try{
if(navigator.sendBeacon){navigator.sendBeacon(ep,new Blob([b],{type:'text/plain;charset=UTF-8'}));return;}
}catch(e){}
try{var x=new XMLHttpRequest();x.open('POST',ep,true);x.setRequestHeader('Content-Type','application/json');x.send(b);}catch(e){}
}
send();
var ps=history.pushState;
if(ps){history.pushState=function(){var r=ps.apply(this,arguments);send();return r;};}
addEventListener('popstate',send);
}catch(e){}
})();`

// SnippetRoutes mounts the public snippet. It is served from the API origin so a tenant embeds a
// single tag with no build step:
//
//	<script defer src="https://hub.example.com/a.js" data-key="mfk_..."></script>
//
// No Subresource Integrity is advertised, deliberately: pinning a hash would break every
// embedding site on each snippet update. See the design doc for that tradeoff.
func (h *PublicHandler) SnippetRoutes(r chi.Router) {
	r.Get("/a.js", h.serveSnippet)
}

func (h *PublicHandler) serveSnippet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	// Short cache: long enough to avoid re-fetching on every page, short enough that a fix to the
	// snippet reaches embedding sites the same day.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A <script src> tag needs no CORS, but allow it anyway so a site can also fetch() the file
	// to self-host or hash it.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, "a.js", snippetBuildTime, strings.NewReader(snippetJS))
}

// snippetBuildTime is a fixed modtime so ServeContent can answer conditional requests. It changes
// only when the snippet does (bump it with the content).
var snippetBuildTime = time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
