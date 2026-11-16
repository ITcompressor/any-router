// admin — веб-админка /admin (задача 3 PRD): HTML-страница со списком
// провайдеров, API-токенов и баланс-алиасов, формы создания/удаления;
// изменения сохраняются в any-router.yaml через store (атомарно, 0600),
// плюс настройки экспорта langfuse и примеры вызова. Вкладки — Провайдеры,
// API-токены, Балансировка, Langfuse, Примеры вызова.
package web

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ITcompressor/any-router/internal/config"
	"github.com/ITcompressor/any-router/internal/proxy"
	"github.com/ITcompressor/any-router/internal/store"
	"github.com/ITcompressor/any-router/internal/version"
)

// AdminCookie — HttpOnly-cookie с админ-токеном (SameSite=Lax),
// ставится после входа через ?token=.
const AdminCookie = "any_router_admin"

// iconSVG — иконка проекта (маршрутизация: один вход, ветвление к провайдерам);
// отдаётся на /icon.svg (favicon + логотип шапки админки).
//
//go:embed static/icon.svg
var iconSVG []byte

// adminPage — шаблон админки (html/template с авто-экранированием,
// secretCell — доверенный HTML-хелпер маски секрета с глазом/копией).
var adminPage = template.Must(template.New("admin").
	Funcs(template.FuncMap{"secretCell": secretCell}).
	Parse(`<!doctype html>
<html lang="{{.Str.Lang}}">
<head>
<meta charset="utf-8">
<title>{{.Str.PageTitle}}</title>
<link rel="icon" type="image/svg+xml" href="/icon.svg">
<script>
// тема — до отрисовки (без «мигания»): сохранённый выбор, иначе системный
(function () {
  var t = localStorage.getItem('any_router_theme');
  if (t !== 'dark' && t !== 'light') {
    t = (window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches) ? 'dark' : 'light';
  }
  document.documentElement.setAttribute('data-theme', t);
})();
</script>
<style>
 :root {
  --bg: #ffffff; --fg: #1a1a2e; --border: #e4e4ee; --surface: #f4f4f8; --active: #fff; --card: #fbfbfe;
  --accent: #4fd1c5; --accent-strong: #1fa49b; --accent-fg: #062e2a; --ring: rgba(31,164,155,.25);
  --danger: #c94d5e;
  --notice-bg: #fff8e1; --notice-bd: #f0d98c; --err-bg: #fde8e8; --err-bd: #f0b4b4;
  --muted: #7a7a90; --icon: #555; --icon-hover: #000; --pre-bg: #1a1a2e; --pre-fg: #e8e8f0;
  --shadow: 0 1px 2px rgba(26,26,46,.05), 0 2px 8px rgba(26,26,46,.07);
 }
 html[data-theme="dark"] {
  --bg: #12121c; --fg: #e8e8f0; --border: #34344c; --surface: #1c1c2a; --active: #26263c; --card: #191926;
  --accent: #4fd1c5; --accent-strong: #2fb3aa; --accent-fg: #06251f; --ring: rgba(79,209,197,.28);
  --danger: #e5717a;
  --notice-bg: #2d2814; --notice-bd: #7a6c33; --err-bg: #2e1616; --err-bd: #7a3333;
  --muted: #8f8fa8; --icon: #a8a8c0; --icon-hover: #fff; --pre-bg: #0c0c14; --pre-fg: #e8e8f0;
  --shadow: 0 1px 3px rgba(0,0,0,.35);
 }
 * { box-sizing: border-box; }
 [hidden] { display: none !important; }
 body { font-family: -apple-system, "Segoe UI", Roboto, sans-serif; max-width: 64rem; margin: 2rem auto; padding: 0 1rem; background: var(--bg); color: var(--fg); }
 h1 { font-size: 1.4rem; letter-spacing: -.01em; }
 h2 { font-size: 1.1rem; margin: .1rem 0 .9rem; }
 h3 { font-size: .98rem; margin: 1.5rem 0 .5rem; }
 .logo { height: 1.35rem; margin-right: .4rem; vertical-align: -0.3rem; }
 /* таблицы — карточка со скруглением, мягкие разделители, подсветка строки */
 .tableView { border: 1px solid var(--border); border-radius: .65rem; overflow: hidden; box-shadow: var(--shadow); }
 table { border-collapse: collapse; width: 100%; }
 th, td { border-bottom: 1px solid var(--border); padding: .5rem .75rem; text-align: left; font-size: .92rem; }
 th { background: var(--surface); font-weight: 600; font-size: .84rem; }
 tbody tr { transition: background .12s; }
 tbody tr:hover { background: var(--active); }
 tbody tr:last-child td { border-bottom: none; }
 code { font-family: ui-monospace, Menlo, monospace; background: var(--surface); padding: .05rem .3rem; border-radius: .3rem; }
 form.inline { display: inline; }
 /* кнопки: нейтральная / primary (акцент) / danger (удаление) */
 button { cursor: pointer; font: inherit; font-size: .92rem; color: var(--fg); background: var(--bg); border: 1px solid var(--border); border-radius: .45rem; padding: .42rem .9rem; transition: background .15s, border-color .15s, color .15s, filter .15s; }
 button:hover { background: var(--surface); border-color: var(--muted); }
 button:active { transform: translateY(1px); }
 button.primary { background: var(--accent-strong); border-color: var(--accent-strong); color: var(--accent-fg); font-weight: 600; }
 button.primary:hover { background: var(--accent); border-color: var(--accent); filter: none; }
 button.danger { color: var(--danger); background: none; border-color: transparent; padding: .25rem .55rem; font-size: .88rem; }
 button.danger:hover { background: var(--err-bg); border-color: var(--err-bd); }
 button:focus-visible, .tab:focus-visible, .iconbtn:focus-visible { outline: 2px solid var(--accent-strong); outline-offset: 1px; }
 /* поля и формы: сетка «подпись — поле», фокус-кольцо */
 input, select { margin: 0; padding: .42rem .6rem; font: inherit; font-size: .92rem; background: var(--bg); color: var(--fg); border: 1px solid var(--border); border-radius: .45rem; transition: border-color .15s, box-shadow .15s, filter .15s; }
 input::placeholder { color: var(--muted); }
 input:focus, select:focus { outline: none; border-color: var(--accent-strong); box-shadow: 0 0 0 3px var(--ring); }
 fieldset { border: 1px solid var(--border); border-radius: .7rem; margin: 1.2rem 0 .3rem; padding: .95rem 1.2rem 1.05rem; background: var(--card); box-shadow: var(--shadow); }
 legend { font-weight: 600; font-size: .92rem; padding: 0 .45rem; margin-bottom: .4rem; }
 /* .frow — виртуальные строки: сетка на самом fieldset, поэтому лейбл-column
    общая для всех строк формы (единый левый край инпутов при любых лейблах);
    display: contents прячет сам div, в сетку уходят label и поле */
 .frow { display: contents; }
 .frow label { font-size: .9rem; line-height: 1.3; text-align: right; }
 .frow label .muted { font-size: .82rem; }
 .frow > input, .frow > select, .frow > .flexline { width: 100%; }
 .frow.actions button { grid-column: 1 / -1; justify-self: stretch; }
 .flexline { display: flex; align-items: center; gap: .4rem; }
 .flexline > select { flex: 1 1 auto; min-width: 0; }
 .frow.actions .flexline { grid-column: 1 / -1; width: 100%; }
 .frow.actions .flexline > button.primary { flex: 1 1 auto; }
 /* единая сетка «лейбл — поле»: fieldset и фильтры примеров — общий
   левый край полей; .frow внутри — виртуальные строки (display: contents) */
 fieldset, .exbar { display: grid; grid-template-columns: minmax(10.5rem, max-content) minmax(0, 1fr); gap: .65rem .9rem; align-items: center; }
 /* .frow.wide — подпись строкой над полем на всю ширину (длинные подсказки
    multi-select'ов не распирают колонку подписей и не сжимают поле) */
 .frow.wide label { grid-column: 1 / -1; text-align: left; }
 .frow.wide > .flexline { grid-column: 1 / -1; }
 @media (max-width: 40rem) {
  fieldset, .exbar { grid-template-columns: 1fr; gap: .3rem; }
  .frow label { text-align: left; }
 }
 .notice { background: var(--notice-bg); border: 1px solid var(--notice-bd); border-radius: .6rem; padding: .65rem .95rem; margin: 1rem 0 0; }
 .err { background: var(--err-bg); border: 1px solid var(--err-bd); }
 footer { margin-top: 3rem; color: var(--muted); font-size: .8rem; }
 /* вкладки + активная вкладка сливается с карточкой панели */
 .tabs { display: flex; gap: .35rem; border-bottom: 1px solid var(--border); margin: 1.3rem 0 0; }
 .tab { border: 1px solid transparent; border-radius: .55rem .55rem 0 0; background: transparent; color: var(--muted); padding: .5rem 1.05rem; font-size: .95rem; }
 .tab:hover { background: var(--surface); color: var(--fg); }
 .tab.active { background: var(--card); border: 1px solid var(--border); border-bottom: 1px solid var(--card); color: var(--fg); font-weight: 600; margin-bottom: -1px; }
 .pane { display: none; }
 .pane.active { display: block; background: var(--card); border: 1px solid var(--border); border-top: none; border-radius: 0 0 .7rem .7rem; padding: 1.15rem 1.35rem 1.25rem; }
 .iconbtn { border: none; background: none; cursor: pointer; padding: 0 .25rem; font-size: .95rem; color: var(--icon); border-radius: .3rem; }
 .iconbtn:hover { color: var(--icon-hover); background: var(--surface); }
 .headrow { display: flex; justify-content: space-between; align-items: center; gap: 1rem; }
 .version { color: var(--muted); font-size: .8rem; white-space: nowrap; }
 pre.example { background: var(--pre-bg); color: var(--pre-fg); padding: .8rem 2.8rem .8rem 1rem; border-radius: .55rem; overflow-x: auto; font-size: .85rem; line-height: 1.5; }
 pre.example code { background: none; padding: 0; border-radius: 0; color: inherit; }
 .exblock { position: relative; }
 .exblock .copybtn { position: absolute; top: .45rem; right: .5rem; background: rgba(255,255,255,.1); border-radius: .3rem; padding: .15rem .45rem; color: var(--pre-fg); }
 .exblock .copybtn:hover { background: rgba(255,255,255,.25); color: #fff; }
 .exbar { margin: .7rem 0; padding: .75rem 1.2rem; background: var(--active); border: 1px solid var(--border); border-radius: .6rem; }
 .muted { color: var(--muted); font-size: .85rem; }
</style></style>
</head>
<body>
<div class="headrow">
 <h1><img class="logo" src="/icon.svg" alt="any-router"> {{.Str.PageTitle}}</h1>
 <div>
  <span class="version" title="{{.Str.VersionTitle}}">{{.Version}}</span>
  <select class="lang-switch" id="lang-switch" data-lang="{{.Str.Lang}}" title="{{.Str.LangTitle}}">
   <option value="ru">ru</option>
   <option value="en">en</option>
  </select>
  <button type="button" class="iconbtn" id="theme-toggle" title="{{.Str.ThemeTitle}}" onclick="toggleTheme()">🌙</button>
 </div>
</div>
{{with .Notice}}<div class="notice">{{.}}</div>{{end}}
{{with .Problem}}<div class="notice err">{{.}}</div>{{end}}

<nav class="tabs">
 <button type="button" class="tab" data-pane="providers">{{.Str.TabProviders}}</button>
 <button type="button" class="tab" data-pane="tokens">{{.Str.TabTokens}}</button>
 <button type="button" class="tab" data-pane="balances">{{.Str.TabBalances}}</button>
 <button type="button" class="tab" data-pane="langfuse">{{.Str.TabLangfuse}}</button>
 <button type="button" class="tab" data-pane="examples">{{.Str.TabExamples}}</button>
</nav>

<!-- ══ Провайдеры ══ -->
<div id="pane-providers" class="pane">
<h2>{{.Str.ProvHead}}</h2>
<div class="tableView">
<table>
 <tr><th>{{.Str.ThName}}</th><th>{{.Str.ThBaseURL}}</th><th>{{.Str.ThAPIKey}}</th><th></th></tr>
 {{$token := .URLToken}}
 {{range .Providers}}
 <tr>
  <td><code>{{.Name}}</code></td>
  <td><code>{{.BaseURL}}</code></td>
  <td>{{if .HasAPIKey}}{{secretCell .APIKeyMasked .APIKeyFull $.Str}}{{else}}—{{end}}</td>
  <td>
   <form class="inline" method="post" action="/admin{{if $token}}?token={{$token}}{{end}}">
    <input type="hidden" name="op" value="provider-delete">
    <input type="hidden" name="name" value="{{.Name}}">
    <button class="danger">{{$.Str.Delete}}</button>
   </form>
  </td>
 </tr>
 {{else}}
 <tr><td colspan="4">{{$.Str.ProvEmpty}}</td></tr>
 {{end}}
</table>
</div>
<form method="post" action="/admin{{if .URLToken}}?token={{.URLToken}}{{end}}">
 <input type="hidden" name="op" value="provider-add">
 <fieldset>
  <legend>{{.Str.ProvLegend}}</legend>
  <div class="frow"><label>{{.Str.ProvNameLabel}} <span class="muted">{{.Str.ProvNameHint}}</span></label><input name="name" required spellcheck="false"></div>
  <div class="frow"><label>{{.Str.ProvBaseLabel}} <span class="muted">{{.Str.ProvBaseHint}}</span></label><input name="baseurl" placeholder="http://127.0.0.1:11434/v1" required spellcheck="false"></div>
  <div class="frow"><label>{{.Str.ProvKeyLabel}} <span class="muted">{{.Str.ProvKeyHint}}</span></label><input name="apikey" autocomplete="off" spellcheck="false"></div>
  <div class="frow actions"><button class="primary">{{.Str.AddBtn}}</button></div>
 </fieldset>
</form>
</div>

<!-- ══ API-токены ══ -->
<div id="pane-tokens" class="pane">
<h2>{{.Str.TokHead}}</h2>
<p class="muted">{{.Str.TokHint}}</p>
<div class="tableView">
<table>
 <tr><th>{{.Str.TokIDLabel}}</th><th>{{.Str.ThToken}}</th><th>{{.Str.TokRightsLabel}}</th><th></th></tr>
 {{range .Tokens}}
 <tr data-id="{{.ID}}" data-scope="{{.Scope}}" data-provider="{{.ScopeProvider}}" data-model="{{.ScopeModel}}" data-models="{{.ScopeModels}}">
  <td><code>{{.ID}}</code></td>
  <td>{{secretCell .Masked .Token $.Str}}</td>
  <td>{{.Rights}}</td>
  <td>
   <button type="button" class="iconbtn" title="{{$.Str.EditRowTitle}}" onclick="editToken(this)">✎</button>
   <form class="inline" method="post" action="/admin{{if $token}}?token={{$token}}{{end}}">
    <input type="hidden" name="op" value="token-delete">
    <input type="hidden" name="id" value="{{.ID}}">
    <button class="danger">{{$.Str.Delete}}</button>
   </form>
  </td>
 </tr>
 {{else}}
 <tr><td colspan="4">{{$.Str.TokEmpty}}</td></tr>
 {{end}}
</table>
</div>
<form method="post" action="/admin{{if .URLToken}}?token={{.URLToken}}{{end}}">
 <input type="hidden" name="op" value="token-add" id="tok-op">
 <input type="hidden" name="edit-id" id="tok-edit-id" value="">
 <input type="hidden" name="scope" value="models">
 <fieldset>
  <legend id="tok-legend">{{.Str.TokLegend}}</legend>
  <div class="frow"><label>{{.Str.TokIDLabel}} <span class="muted">{{.Str.TokIDHint}}</span></label><input name="id" id="tok-id" autocomplete="off" spellcheck="false"></div>
  <div class="frow wide"><label>{{.Str.TokModelsLabel}} <span class="muted">{{.Str.TokModelsHint}}</span></label>
   <div class="flexline">
    <select name="models" id="tok-models-select" multiple size="8"><option value="">{{.Str.LoadModelsTok}}</option></select>
    <button type="button" class="iconbtn" title="{{.Str.RefreshTitle}}" onclick="refreshTokenModels()">⟳</button>
   </div>
  </div>
  <div class="frow actions">
   <div class="flexline">
    <button class="primary" id="tok-submit">{{.Str.TokCreateBtn}}</button>
    <button type="button" class="danger" id="tok-cancel" hidden onclick="tokEditReset()">{{.Str.CancelEditBtn}}</button>
   </div>
  </div>
 </fieldset>
</form>
</div>

<!-- ══ Балансировка ══ -->
<div id="pane-balances" class="pane">
<h2>{{.Str.BalHead}}</h2>
<p class="muted">{{.Str.BalHint}}</p>
<div class="tableView">
<table>
 <tr><th>{{.Str.BalNameLabel}}</th><th>{{.Str.BalModelsLabel}}</th><th></th></tr>
 {{$token := .URLToken}}
 {{range .Balances}}
 <tr data-name="{{.Name}}" data-models="{{.Models}}">
  <td><code>{{.Name}}</code></td>
  <td>{{.ModelsJoined}}</td>
  <td>
   <button type="button" class="iconbtn" title="{{$.Str.EditRowTitle}}" onclick="editBalance(this)">✎</button>
   <form class="inline" method="post" action="/admin{{if $token}}?token={{$token}}{{end}}">
    <input type="hidden" name="op" value="balance-delete">
    <input type="hidden" name="name" value="{{.Name}}">
    <button class="danger">{{$.Str.Delete}}</button>
   </form>
  </td>
 </tr>
 {{else}}
 <tr><td colspan="3">{{$.Str.BalEmpty}}</td></tr>
 {{end}}
</table>
</div>
<form method="post" action="/admin{{if .URLToken}}?token={{.URLToken}}{{end}}">
 <input type="hidden" name="op" value="balance-add" id="bal-op">
 <input type="hidden" name="edit-name" id="bal-edit-name" value="">
 <fieldset>
  <legend id="bal-legend">{{.Str.BalLegend}}</legend>
  <div class="frow"><label>{{.Str.BalNameLabel}} <span class="muted">{{.Str.BalNameHint}}</span></label><input name="name" id="bal-name" autocomplete="off" spellcheck="false"></div>
  <div class="frow wide"><label>{{.Str.BalModelsLabel}} <span class="muted">{{.Str.BalModelsHint}}</span></label>
   <div class="flexline">
    <select name="models" id="bal-models-select" multiple size="8"></select>
    <button type="button" class="iconbtn" title="{{.Str.RefreshTitle}}" onclick="refreshBalanceModels()">⟳</button>
   </div>
  </div>
  <div class="frow actions">
   <div class="flexline">
    <button class="primary" id="bal-submit">{{.Str.BalCreateBtn}}</button>
    <button type="button" class="danger" id="bal-cancel" hidden onclick="balEditReset()">{{.Str.CancelEditBtn}}</button>
   </div>
  </div>
 </fieldset>
</form>
</div>

<!-- ══ Langfuse ══ -->
<div id="pane-langfuse" class="pane">
<h2>{{.Str.LfHead}}</h2>
<p class="muted">{{.Str.LfHint}}
<code>{url}/api/public/otel/v1/traces</code> {{.Str.LfHintPost}}</p>
{{if .Langfuse.On}}
<p class="muted">{{.Str.LfStatus}} <b>{{.Str.LfOn}}</b> — <code>{{.Langfuse.URL}}</code></p>
{{else if .Langfuse.Configured}}
<p class="muted">{{.Str.LfStatus}} <b>{{.Str.LfOff}}</b> — {{.Str.LfOffToggled}}</p>
{{else}}
<p class="muted">{{.Str.LfStatus}} <b>{{.Str.LfOff}}</b> — {{.Str.LfOffHint}}</p>
{{end}}
<div class="tableView">
<table>
 <tr><th>{{.Str.ThURL}}</th><th>{{.Str.ThPubKey}}</th><th>{{.Str.ThSecKey}}</th></tr>
 <tr>
  <td><code>{{.Langfuse.URL}}</code></td>
  <td><code>{{.Langfuse.PublicKey}}</code></td>
  <td>{{if .Langfuse.SecretKey}}{{secretCell .Langfuse.SecretMasked .Langfuse.SecretKey $.Str}}{{else}}—{{end}}</td>
 </tr>
</table>
</div>
<form method="post" action="/admin{{if .URLToken}}?token={{.URLToken}}{{end}}">
 <input type="hidden" name="op" value="langfuse-set">
 <fieldset>
  <legend>{{.Str.LfLegend}}</legend>
  <div class="frow"><label>{{.Str.LfEnabledLabel}} <span class="muted">{{.Str.LfEnabledHint}}</span></label><input type="checkbox" name="enabled" value="on" {{if .Langfuse.Enabled}}checked{{end}}></div>
  <div class="frow"><label>{{.Str.LfURLLabel}} <span class="muted">{{.Str.LfURLHint}}</span></label><input name="url" value="{{.Langfuse.URL}}" placeholder="http://10.0.0.3:3000" spellcheck="false" autocomplete="off"></div>
  <div class="frow"><label>{{.Str.LfPubLabel}} <span class="muted">{{.Str.LfPubHint}}</span></label><input name="public_key" value="{{.Langfuse.PublicKey}}" spellcheck="false" autocomplete="off"></div>
  <div class="frow"><label>{{.Str.LfSecLabel}} <span class="muted">{{.Str.LfSecHint}}</span></label><input name="secret_key" value="{{.Langfuse.SecretKey}}" spellcheck="false" autocomplete="off"></div>
  <div class="frow"><label>{{.Str.LfEnvLabel}} <span class="muted">{{.Str.LfEnvHint}}</span></label><input name="environment" value="{{.Langfuse.Environment}}" spellcheck="false" autocomplete="off"></div>
  <div class="frow"><label>{{.Str.LfTagsLabel}} <span class="muted">{{.Str.LfTagsHint}}</span></label><input name="tags" value="{{.Langfuse.Tags}}" spellcheck="false" autocomplete="off"></div>
  <div class="frow actions"><button class="primary">{{.Str.SaveBtn}}</button></div>
 </fieldset>
</form>
</div>

<!-- ══ Примеры вызова ══ -->
<div id="pane-examples" class="pane">
<h2>{{.Str.ExHead}}</h2>
<p class="muted">{{.ExHint}}</p>
<div class="exbar">
 <div class="frow"><label>{{.Str.ExTokenLabel}} <span class="muted">{{.Str.ExTokenHint}}</span></label>
  <select id="ex-token" onchange="tokenChanged()">
   <option value="{{.AdminToken}}" data-scope="all">{{.Str.AdminOption}}</option>
   {{range .Tokens}}<option value="{{.Token}}" data-scope="{{.Scope}}" data-provider="{{.ScopeProvider}}" data-model="{{.ScopeModel}}" data-models="{{.ScopeModels}}">{{$.Str.ExTokenSuffix}}{{.ID}}{{if .Rights}} — {{.Rights}}{{end}}</option>{{end}}
  </select>
 </div>
 <div class="frow"><label>{{.Str.ExModelLabel}}</label>
  <select id="ex-model" onchange="buildExamples()">
   <option value="">{{.Str.LoadModelsEx}}</option>
  </select>
 </div>
</div>
<div id="row-chat">
<h3>POST /v1/chat/completions <span class="muted">({{.Str.ExChatCaption}})</span></h3>
<div class="exblock">
 <pre class="example" id="ex-chat"><code></code></pre>
 <button type="button" class="iconbtn copybtn" title="{{.Str.CopyBtnTitle}}" onclick="copyPre('ex-chat', this)">⧉</button>
</div>
</div>
<div id="row-embed" hidden>
<h3>POST /v1/embeddings <span class="muted">({{.Str.ExEmbedCaption}})</span></h3>
<div class="exblock">
 <pre class="example" id="ex-embeddings"><code></code></pre>
 <button type="button" class="iconbtn copybtn" title="{{.Str.CopyBtnTitle}}" onclick="copyPre('ex-embeddings', this)">⧉</button>
</div>
</div>
<div id="row-sysone" hidden>
<h3>POST /v1/systemone <span class="muted">({{.Str.ExSysoneCaption}})</span></h3>
<div class="exblock">
 <pre class="example" id="ex-systemone"><code></code></pre>
 <button type="button" class="iconbtn copybtn" title="{{.Str.CopyBtnTitle}}" onclick="copyPre('ex-systemone', this)">⧉</button>
</div>
</div>
<h3>GET /v1/models <span class="muted">({{.Str.ExModelsCaption}})</span></h3>
<div class="exblock">
 <pre class="example" id="ex-models-list"><code></code></pre>
 <button type="button" class="iconbtn copybtn" title="{{.Str.CopyBtnTitle}}" onclick="copyPre('ex-models-list', this)">⧉</button>
</div>
<p class="muted" id="ex-scope"></p>
<p class="muted" id="ex-hint"></p>
</div>

<script>
var STR = {{.Str}}; // строки интерфейса (язык сессии: cookie any_router_lang / yaml lang)
var urlToken = '{{.URLToken}}';
// вкладки: ?tab= (диплинк, только из белого списка) → вкладка после мутации →
// localStorage → первая.
var TABS = ['providers', 'tokens', 'balances', 'langfuse', 'examples'];
var urlTab = new URLSearchParams(location.search).get('tab') || '';
if (TABS.indexOf(urlTab) < 0) { urlTab = ''; } // чужой ?tab= — молча фолбэк
var initialTab = urlTab || '{{.ActiveTab}}' || localStorage.getItem('any_router_tab') || 'providers';
// base_url запуска (-base-url при запуске; пусто — дефолт на сервере):
// адрес, который подставляется в curl-примеры вместо origin строки браузера.
var routerBase = '{{.BaseURL}}';
// fmt — подстановка %s по числу аргументов (строки-шаблоны из STR).
function fmt(s) {
  var args = Array.prototype.slice.call(arguments, 1), i = 0;
  return String(s).replace(/%s/g, function () { return args[i++]; });
}
function routerOrigin() {
  var base = String(routerBase || '').replace(/\/+$/, '');
  return base || window.location.origin;
}

// ── язык интерфейса: cookie any_router_lang (yaml lang — дефолт), выбор — перезагрузка ──
var langSel = document.getElementById('lang-switch');
if (langSel) {
  var m = document.cookie.match(/(?:^|; )any_router_lang=([^;]*)/);
  langSel.value = m ? decodeURIComponent(m[1]) : langSel.dataset.lang || 'ru';
  langSel.onchange = function () {
    document.cookie = 'any_router_lang=' + encodeURIComponent(langSel.value) + ';path=/;max-age=31536000;samesite=lax';
    location.reload();
  };
}

// ── тема: светлая/тёмная (переключатель в шапке, выбор в localStorage) ──
function applyTheme(t) {
  document.documentElement.setAttribute('data-theme', t);
  var b = document.getElementById('theme-toggle');
  if (b) { b.textContent = (t === 'dark') ? '☀️' : '🌙'; }
}
function toggleTheme() {
  var t = (document.documentElement.getAttribute('data-theme') === 'dark') ? 'light' : 'dark';
  localStorage.setItem('any_router_theme', t);
  applyTheme(t);
}
applyTheme(document.documentElement.getAttribute('data-theme') || 'light');

// ── вкладки (диплинки: активная вкладка — в URL ?tab=; ссылка
// открывает админку сразу на вкладке; replaceState — без засорения
// истории; прочие параметры, token=, сохраняется) ──
function activateTab(name, opts) {
  opts = opts || {};
  document.querySelectorAll('.tab').forEach(function (b) {
    b.classList.toggle('active', b.dataset.pane === name);
  });
  document.querySelectorAll('.pane').forEach(function (p) {
    p.classList.toggle('active', p.id === 'pane-' + name);
  });
  localStorage.setItem('any_router_tab', name);
  if (!opts.silent) {
    var u = new URL(location.href);
    u.searchParams.set('tab', name);
    history.replaceState(null, '', u);
  }
}
document.querySelectorAll('.tab').forEach(function (b) {
  b.onclick = function () { activateTab(b.dataset.pane); };
});
activateTab(initialTab);

// ── секреты: глаз/копия (secretCell) ──
function secretEye(btn) {
  var code = btn.parentNode.querySelector('code.secret');
  var shown = code.textContent !== code.dataset.mask;
  code.textContent = shown ? code.dataset.mask : code.dataset.full;
  btn.textContent = shown ? '\uD83D\uDC41' /* 👁 */ : '\uD83D\uDE48' /* 🙈 */;
}
function secretCopy(btn) {
  if (!copyText(btn.dataset.secret)) { return; }
  var old = btn.textContent;
  btn.textContent = '\u2713'; /* ✓ */
  setTimeout(function () { btn.textContent = old; }, 1200);
}
function copyText(text) {
  // http (не localhost) не secure context: navigator.clipboard может отсутствовать
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text);
    return true;
  }
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  var ok = document.execCommand('copy');
  document.body.removeChild(ta);
  return ok;
}
function copyPre(id, btn) {
  var pre = document.getElementById(id);
  if (!pre || !pre.textContent || !copyText(pre.textContent)) { return; }
  if (btn) {
    var old = btn.textContent;
    btn.textContent = '\u2713'; /* ✓ */
    setTimeout(function () { btn.textContent = old; }, 1200);
  }
}

// ── примеры вызова ──
function shellQuote(s) { return "'" + String(s).replace(/'/g, "'\\''") + "'"; }
function jsonLiteral(obj) { return JSON.stringify(obj); }
// capsOf — capabilities выбранной модели из data-атрибута option
// (заполнен, только если бэкенд отдал capabilities; null — данных нет).
function capsOf(ref) {
  var opt = document.querySelector('#ex-model option[value="' + cssEscape(ref) + '"]');
  return opt && opt.dataset.caps ? String(opt.dataset.caps).split(',') : null;
}
function hasCap(caps, name) { return !!caps && caps.indexOf(name) >= 0; }
function buildExamples() {
  var tok = document.getElementById('ex-token').value;
  var model = document.getElementById('ex-model').value;
  var origin = routerOrigin();
  var chat = document.querySelector('#ex-chat code');
  var emb = document.querySelector('#ex-embeddings code');
  var sysone = document.querySelector('#ex-systemone code');
  var models = document.querySelector('#ex-models-list code');
  var hint = document.getElementById('ex-hint');
  // набор примеров — по capabilities модели из кэша бэкенда: capabilities
  // неизвестны (или модель не выбрана) — показываем базовый чат.
  var caps = model ? capsOf(model) : null;
  document.getElementById('row-chat').hidden = !!model && !!caps && !hasCap(caps, 'completion');
  document.getElementById('row-embed').hidden = !model || !hasCap(caps, 'embedding');
  document.getElementById('row-sysone').hidden = !model || !hasCap(caps, 'decision');
  var chatSnippet = 'curl -s ' + origin + '/v1/chat/completions \\\n' +
    "  -H 'Authorization: Bearer " + tok + "' \\\n" +
    '  -H \'Content-Type: application/json\' \\\n' +
    "  -d '" + jsonLiteral({
      model: model || STR.PickModel,
      messages: [{ role: 'user', content: 'ping' }]
    }) + "' | jq";
  var embSnippet = 'curl -s ' + origin + '/v1/embeddings \\\n' +
    "  -H 'Authorization: Bearer " + tok + "' \\\n" +
    '  -H \'Content-Type: application/json\' \\\n' +
    "  -d '" + jsonLiteral({
      model: model || STR.PickModel,
      input: 'ping'
    }) + "' | jq";
  // systemone — decision-API бэкенда (spec docs/specs/OllamaApi.yaml,
  // SystemOneRequest): state — строка или объект контекста; вопросы —
  // произвольные ключи (type choice|noul|score), у noul criteria —
  // пары «true/false → описание исхода». Пример простой — noul по состоянию.
  var sysoneSnippet = 'curl -s ' + origin + '/v1/systemone \\\n' +
    "  -H 'Authorization: Bearer " + tok + "' \\\n" +
    '  -H \'Content-Type: application/json\' \\\n' +
    "  -d '" + jsonLiteral({
      model: model || STR.PickModel,
      state: 'Hello World',
      questions: {
        says_hello: {
          type: 'noul',
          instructions: 'Does the state text contain a greeting?',
          criteria: {
            true: 'The state text contains a greeting.',
            false: 'The state text does not contain a greeting.'
          }
        }
      }
    }) + "' | jq";
  chat.textContent = chatSnippet;
  emb.textContent = embSnippet;
  sysone.textContent = sysoneSnippet;
  var modelsSnippet = 'curl -s ' + origin + '/v1/models \\\n  -H \'Authorization: Bearer ' + tok + '\'';
  models.textContent = modelsSnippet + ' | jq';
  hint.textContent = model
    ? (balanceOf(model)
        ? fmt(STR.HintBalanceFmt, model, membersOf(model) || '—')
        : fmt(STR.HintModelFmt, model, rawOf(model) || model))
    : STR.HintNoModel;
}

function rawOf(ref) {
  var opt = document.querySelector('#ex-model option[value="' + cssEscape(ref) + '"]');
  return opt && opt.dataset.raw ? opt.dataset.raw : '';
}
function balanceOf(ref) {
  var opt = document.querySelector('#ex-model option[value="' + cssEscape(ref) + '"]');
  return !!opt && opt.dataset.balance === '1';
}
function membersOf(ref) {
  var opt = document.querySelector('#ex-model option[value="' + cssEscape(ref) + '"]');
  return opt && opt.dataset.members ? String(opt.dataset.members).replace(/\n/g, ', ') : '';
}
function cssEscape(s) {
  return (window.CSS && CSS.escape) ? CSS.escape(s) : String(s).replace(/["\\]/g, '\\$&');
}

// ── модели: общий кэш списка всех провайдеров (/admin/api/models) ──
// Сервер держит полный список в кэше (первичная полная загрузка при старте
// + обновление раз в 10 минут), страница — полученный список в памяти
// (modelsCache, Promise). Фильтры (выбор токена, области прав) работают на
// этом локальном списке без дополнительных сетевых запросов к API;
// явное обновление — только ⟳ в форме создания токена
// (force → ?refresh=1 — немедленное обновление серверного кэша).
var modelsCache = null;
function adminModels(force) {
  if (force || !modelsCache) {
    var url = '/admin/api/models' + (urlToken ? '?token=' + urlToken : '');
    if (force) { url += (urlToken ? '&' : '?') + 'refresh=1'; }
    modelsCache = fetch(url)
      .then(function (r) { return r.json(); })
      .then(function (data) { return (data && data.data) || []; });
  }
  return modelsCache;
}
// fillModelSelect — наполнить select: optgroup по провайдеру, value — полная
// форма provider/model (у path-моделей raw виден после ⟶). Пустой список —
// плейсхолдер emptyText (value пустой — примеры/создание не подставятся).
// hostOpt — первой опцией каждой группы провайдера добавить «весь хост»
// (value — provider/*, все его модели; dataset.host — для предвыбора
// при редактировании старых областей).
function fillModelSelect(sel, list, emptyText, hostOpt) {
  sel.innerHTML = '';
  if (!list.length) {
    var o = document.createElement('option');
    o.value = '';
    o.textContent = emptyText || STR.EmptyModels;
    sel.appendChild(o);
    return;
  }
  var groups = {}, order = [];
  list.forEach(function (m) {
    // баланс-алиасы — своя optgroup после провайдеров (голые имена без префикса)
    var p = m.balance ? '\u0001balance' : (m.provider || '');
    if (!groups[p]) {
      groups[p] = document.createElement('optgroup');
      groups[p].label = m.balance ? STR.BalanceGroup : (p || STR.NoProviderGlob);
      order.push(p);
      if (hostOpt && !m.balance && p) {
        var h = document.createElement('option');
        h.value = p + '/*';
        h.textContent = STR.HostOpt;
        h.dataset.host = '1';
        groups[p].appendChild(h);
      }
    }
    var o = document.createElement('option');
    o.value = m.id;
    o.textContent = m.id + (m.raw ? '  ⟶  ' + (m.raw.length > 48 ? m.raw.slice(0, 45) + '…' : m.raw) : '');
    if (m.raw) { o.dataset.raw = m.raw; }
    if (m.provider) { o.dataset.provider = m.provider; }
    if (m.capabilities && m.capabilities.length) { o.dataset.caps = m.capabilities.join(','); }
    if (m.balance) { o.dataset.balance = '1'; o.dataset.members = (m.members || []).join('\n'); }
    groups[p].appendChild(o);
  });
  order.forEach(function (p) { sel.appendChild(groups[p]); });
}

// ── права выбранного токена (data-атрибуты option из сервера) ──
function tokenAttrs() {
  var sel = document.getElementById('ex-token');
  var opt = sel.options[sel.selectedIndex];
  return {
    scope: (opt && opt.dataset.scope) || 'all',
    provider: (opt && opt.dataset.provider) || '',
    model: (opt && opt.dataset.model) || '',
    models: (opt && opt.dataset.models) || ''
  };
}
// allowedForToken — модели, доступные токену: provider — его модели,
// model — ровно своя запись (ref как в option value либо по raw-пути,
// если область записана путём), models — любая из записей списка.
// Баланс-алиас — область покрывает каждого участника (round-robin вправе
// отправить запрос любому из них — как проверяет сервер).
function entryAllowed(m, a) {
  if (a.scope === 'provider') {
    return m.provider === a.provider;
  }
  if (a.scope === 'model') {
    var ref = a.provider + '/' + a.model; // у path-моделей получится «p//path» — рабочая форма
    return m.id === ref || (m.raw && m.raw === a.model);
  }
  if (a.scope === 'models') {
    var set = {};
    a.models.split('\n').forEach(function (r) { if (r) { set[r] = true; } });
    return !!set[m.id] || !!(m.raw && set[m.provider + '/' + m.raw]) || !!set[m.provider + '/*']; // /* — весь хост
  }
  return true;
}
function allowedForToken(list, a) {
  var direct = list.filter(function (m) { return !m.balance && entryAllowed(m, a); });
  var ids = {};
  direct.forEach(function (m) {
    ids[m.id] = true;
    if (m.raw) { ids[m.provider + '/' + m.raw] = true; }
  });
  // хосты области (provider/*): накрывают любую модель своего провайдера —
  // и прямые записи, и участников баланс-алиасов
  var hosts = {};
  if (a.scope === 'models') {
    a.models.split('\n').forEach(function (r) {
      var cut = r.indexOf('/');
      if (cut > 0 && r.slice(cut + 1) === '*') { hosts[r.slice(0, cut)] = true; }
    });
  }
  var alias = list.filter(function (m) {
    if (!m.balance) { return false; }
    if (a.scope === 'models') {
      var granted = {};
      a.models.split('\n').forEach(function (r) {
        var cut = r.indexOf('/');
        if (r && !(cut > 0 && r.slice(cut + 1) === '*')) { granted[r] = true; }
      });
      if (granted[m.id]) { return true; } // алиас явно выдан токену (голое имя)
    }
    var mem = m.members || [];
    return mem.length > 0 && mem.every(function (ref) {
      return !!ids[ref] || !!hosts[ref.slice(0, ref.indexOf('/'))];
    });
  });
  return direct.concat(alias);
}
function scopeNote(a) {
  if (a.scope === 'all') { return ''; }
  if (a.scope === 'provider') {
    return fmt(STR.ScopeProviderNote, a.provider);
  }
  if (a.scope === 'models') {
    return fmt(STR.ScopeModelsNote, a.models.replace(/\n/g, ', '));
  }
  return fmt(STR.ScopeModelNote, a.provider + '/' + a.model);
}
function rebuildExampleModels() {
  adminModels(false).then(function (list) {
    var a = tokenAttrs();
    var sel = document.getElementById('ex-model');
    fillModelSelect(sel, allowedForToken(list, a), STR.NoModelsForToken, false);
    // единственная доступная модель — сразу выбрана, пример готов
    if (sel.options.length === 1 && sel.options[0].value) { sel.selectedIndex = 0; }
    document.getElementById('ex-scope').textContent = scopeNote(a);
    buildExamples();
  }).catch(function () {
    document.getElementById('ex-hint').textContent = STR.FetchFailed;
  });
}
function tokenChanged() { rebuildExampleModels(); }

// ── форма создания/редактирования токена: один multi-select прав — модели
// (с «весь хост») и баланс-алиасы из общего списка; в режиме редактирования
// (✎ в строке) выбор восстанавливается из data-атрибутов строки и переживает
// ⟳. Исторические области отображаются эквивалентом: all → все «весь хост»,
// provider → «весь хост» его провайдера, model → его модель. ──
var editingToken = '';    // id редактируемого токена ('' — создание нового)
var pendingModels = [];   // выбранные записи — пере-выбор после ⟳ и перезаливки списка
var pendingHosts = false; // области all: отметить все «весь хост» после перезаливки
// fillTokenModels — наполнить multi-select прав токена из общего кэша
// (опция «весь хост» в каждой группе провайдера) и восстановить выбор.
// Без force уже наполненный селект не трогаем (не терять ручной выбор,
// как у балансировки); force — ⟳ и редактирование (перезаливка с
// пере-выбором pendingModels/pendingHosts).
function fillTokenModels(force) {
  var sel = document.getElementById('tok-models-select');
  if (!force && sel.options.length > 1 && !editingToken && !pendingModels.length && !pendingHosts) { return; }
  adminModels(false).then(function (list) {
    sel = document.getElementById('tok-models-select');
    // права токена — реальные модели (с «весь хост») и баланс-алиасы (голые
    // имена — явная выдача пула: токен зовёт алиас, участники напрямую не светятся)
    fillModelSelect(sel, list, STR.ModelsFailed, true);
    if (pendingHosts) { selectHostOptions(sel); pendingHosts = false; }
    pendingModels.forEach(function (ref) { pickOption(sel, ref); });
    tokCanonHosts(sel); // страховка от избыточных пар «p/* + p/m» (например, из рукописного yaml)
  }).catch(function () {
    fillModelSelect(document.getElementById('tok-models-select'), [], STR.ModelsFailed, true);
  });
}
// selectHostOptions — отметить все опции «весь хост» (value — provider/*).
function selectHostOptions(sel) {
  Array.prototype.forEach.call(sel.options, function (o) { if (o.dataset.host) { o.selected = true; } });
}
// tokHostProvider — имя провайдера из value опции «весь хост» (value = p + '/*').
function tokHostProvider(o) { return o.value.slice(0, o.value.length - 2); }
// tokCanonHosts — канонизация выбора: конкретные модели, покрытые выбранным
// «весь хост» своего провайдера, снять (p/* покрывает их всё равно).
function tokCanonHosts(sel) {
  var hosts = {};
  Array.prototype.forEach.call(sel.options, function (o) {
    if (o.selected && o.dataset.host) { hosts[tokHostProvider(o)] = true; }
  });
  if (!Object.keys(hosts).length) { return; }
  Array.prototype.forEach.call(sel.options, function (o) {
    if (o.selected && !o.dataset.host && o.dataset.provider && hosts[o.dataset.provider]) { o.selected = false; }
  });
}
// «весь хост» и конкретные модели одного провайдера взаимоисключающие —
// последний выбор вытесняет прежний. Клик по option приходит с уже
// сменившимся selected — по нему точный «последний выбор»; change
// (клавиатура, программная установка) — мягкая канонизация «хост побеждает».
(function () {
  var tokSelect = document.getElementById('tok-models-select');
  if (!tokSelect) { return; }
  tokSelect.addEventListener('click', function (e) {
    var o = e.target;
    if (!o || o.nodeName !== 'OPTION' || !o.value || !o.selected) { return; } // снятие выбора — всегда ок
    if (o.dataset.host) {
      var p = tokHostProvider(o);
      Array.prototype.forEach.call(tokSelect.options, function (m) {
        if (!m.dataset.host && m.dataset.provider === p) { m.selected = false; } // хост вытесняет модели
      });
    } else if (o.dataset.provider) {
      var h = tokSelect.querySelector('option[value="' + cssEscape(o.dataset.provider + '/*') + '"]');
      if (h) { h.selected = false; } // модель вытесняет «весь хост»
    }
  });
  tokSelect.addEventListener('change', function () { tokCanonHosts(tokSelect); });
})();
// pickOption — отметить option по полной форме (после перезаливки списка).
// Область может быть записана путевой формой («p//path»), а модель в списке —
// под авто-алиасом: фолбэк сравнивает raw-путь option.
function pickOption(sel, ref) {
  if (!ref) { return; }
  var found = false;
  Array.prototype.forEach.call(sel.options, function (o) {
    if (o.value === ref) { o.selected = true; found = true; }
  });
  if (found) { return; }
  var cut = ref.indexOf('/');
  var prov = cut > 0 ? ref.slice(0, cut) : '';
  var path = cut > 0 ? ref.slice(cut + 1) : '';
  if (!prov || !path) { return; }
  Array.prototype.forEach.call(sel.options, function (o) {
    if (path && o.dataset.raw === path && o.dataset.provider === prov) { o.selected = true; }
  });
}
function refreshTokenModels() {
  adminModels(true).then(function () { fillTokenModels(true); }, function () { fillTokenModels(true); });
}
// editToken — ✎ в строке токена: форма переключается в редактирование прав
// (op=token-edit, id заблокирован и передаётся скрытым полем edit-id); выбор
// в multi-select строится из области строки: models — её записи; провайдер —
// «весь хост»; модель — полная форма provider/… (path-модели восстанавливаются
// по raw через pickOption); all — все «весь хост» (эквивалент «всё»),
// сохранение перепишет токен в scope=models.
function editToken(btn) {
  var tr = btn.closest('tr');
  if (!tr) { return; }
  editingToken = tr.dataset.id || '';
  pendingModels = [];
  pendingHosts = false;
  var sc = tr.dataset.scope || 'all';
  if (sc === 'provider') {
    pendingModels = [tr.dataset.provider + '/*'];
  } else if (sc === 'model') {
    if (tr.dataset.provider && tr.dataset.model) {
      pendingModels = [tr.dataset.provider + '/' + tr.dataset.model];
    }
  } else if (sc === 'models') {
    pendingModels = (tr.dataset.models || '').split('\n').filter(function (s) { return s; });
  } else {
    pendingHosts = true;
  }
  document.getElementById('tok-op').value = 'token-edit';
  document.getElementById('tok-edit-id').value = editingToken;
  var idInput = document.getElementById('tok-id');
  idInput.value = editingToken;
  idInput.disabled = true;
  fillTokenModels(true);
  document.getElementById('tok-legend').textContent = fmt(STR.TokEditingFmt, editingToken);
  document.getElementById('tok-submit').textContent = STR.SaveBtn;
  document.getElementById('tok-cancel').hidden = false;
  document.getElementById('tok-id').focus();
}
function balSelect() { return document.getElementById('bal-models-select'); }
var editingBalance = '';   // имя редактируемого баланс-алиаса ('' — создание нового)
var pendingBalModels = []; // выбранные участники — пере-выбор после ⟳ и перезаливки списка
function selectedRefs(sel) {
  var out = [];
  Array.prototype.forEach.call(sel.options, function (o) { if (o.selected && o.value) { out.push(o.value); } });
  return out;
}
// fillBalanceModels — участник multi-select из общего кэша моделей (с
// баланс-алиасами — вложение алиаса в алиас не запрещено). Без force —
// уже заполненный селект не трогаем (не терять ручной выбор); force —
// перезаливка с пере-выбором pendingBalModels (редактирование) или текущего.
function fillBalanceModels(force) {
  var sel = balSelect();
  if (!force && sel.options.length > 1) { return; }
  adminModels(false).then(function (list) {
    // участники группы — реальные модели provider/model (алиас-в-алиас не бывает;
    // «весь хост» и алиасы участниками тоже не бывают — группа плоская)
    fillModelSelect(sel, list.filter(function (m) { return !m.balance; }), STR.ModelsFailed, false);
    pendingBalModels.forEach(function (ref) { pickOption(sel, ref); });
  }).catch(function () {
    fillModelSelect(sel, [], STR.ModelsFailed, false);
  });
}
function refreshBalanceModels() {
  if (!editingBalance) { pendingBalModels = selectedRefs(balSelect()); }
  adminModels(true).then(function () { fillBalanceModels(true); }, function () { fillBalanceModels(true); });
}
// editBalance — ✎ в строке баланс-алиаса: форма переключается в
// редактирование (op=balance-edit; имя — ключ записи, блокируется и
// уходит скрытым полем edit-name), участники — из data-атрибутов строки.
function editBalance(btn) {
  var tr = btn.closest('tr');
  if (!tr) { return; }
  editingBalance = tr.dataset.name || '';
  pendingBalModels = (tr.dataset.models || '').split('\n').filter(function (s) { return s; });
  document.getElementById('bal-op').value = 'balance-edit';
  document.getElementById('bal-edit-name').value = editingBalance;
  var nameInput = document.getElementById('bal-name');
  nameInput.value = editingBalance;
  nameInput.disabled = true;
  document.getElementById('bal-legend').textContent = fmt(STR.BalEditingFmt, editingBalance);
  document.getElementById('bal-submit').textContent = STR.SaveBtn;
  document.getElementById('bal-cancel').hidden = false;
  fillBalanceModels(true);
  nameInput.focus();
}
// balEditReset — отмена: форма возвращается к созданию.
function balEditReset() {
  editingBalance = '';
  pendingBalModels = [];
  document.getElementById('bal-op').value = 'balance-add';
  document.getElementById('bal-edit-name').value = '';
  var nameInput = document.getElementById('bal-name');
  nameInput.value = '';
  nameInput.disabled = false;
  document.getElementById('bal-legend').textContent = STR.BalLegend;
  document.getElementById('bal-submit').textContent = STR.BalCreateBtn;
  document.getElementById('bal-cancel').hidden = true;
}
// tokEditReset — отмена редактирования: форма возвращается к созданию
// (выбор в multi-select остаётся как есть — пользователь видит, что наполнено).
function tokEditReset() {
  editingToken = '';
  pendingModels = [];
  pendingHosts = false;
  document.getElementById('tok-op').value = 'token-add';
  document.getElementById('tok-edit-id').value = '';
  var idInput = document.getElementById('tok-id');
  idInput.value = '';
  idInput.disabled = false;
  document.getElementById('tok-legend').textContent = STR.TokLegend;
  document.getElementById('tok-submit').textContent = STR.TokCreateBtn;
  document.getElementById('tok-cancel').hidden = true;
}
// вход вкладки «Примеры» — подгрузить модели (кэш на страницу)
document.querySelector('[data-pane="examples"]').addEventListener('click', rebuildExampleModels);
if (initialTab === 'examples') { rebuildExampleModels(); }
// вход вкладки «Балансировка» — наполнить multi-select участников (кэш на страницу)
document.querySelector('[data-pane="balances"]').addEventListener('click', function () { fillBalanceModels(false); });
if (initialTab === 'balances') { fillBalanceModels(false); }
// вход вкладки «API-токены» — наполнить multi-select прав (кэш на страницу)
document.querySelector('[data-pane="tokens"]').addEventListener('click', fillTokenModels);
if (initialTab === 'tokens') { fillTokenModels(); }
buildExamples();
</script>
</body>
</html>`))

// adminData — модель страницы админки.
type adminData struct {
	Providers  []adminProvider
	Tokens     []adminToken
	Balances   []adminBalance // баланс-алиасы (вкладка «Балансировка»)
	Langfuse   adminLangfuse
	AdminToken string        // для примеров вызова (страница и так за ним)
	Notice     template.HTML // доверенный HTML: имя провайдера/созданный токен (не из клиента)
	Problem    string        // экранируется
	ConfigPath string
	URLToken   string
	BaseURL    string        // base_url в «Примерах вызова» (пусто — config.DefaultBaseURL)
	ActiveTab  string        // вкладка, открытая после мутации (пусто — из localStorage)
	Version    string        // версия + commit (топбар, internal/version)
	Str        *adminStrings // строки интерфейса (язык сессии/yaml)
	ExHint     template.HTML // подсказка «Примеров» с живым base_url
}

// adminLangfuse — настройки экспорта для вкладки: текущие значения (секрет
// маскирован, глаз/копия — как у apikey), переключатель enabled и статус
// включенности (On — переключатель и url с обоими ключами; Configured —
// url/ключи заполнены, статус «выключен» различает причину).
type adminLangfuse struct {
	Enabled      bool // checkbox формы (yaml enabled)
	URL          string
	PublicKey    string
	SecretKey    string
	SecretMasked string
	Environment  string
	Tags         string // через запятую (форму ввода отделяем от yaml-списка)
	On           bool
	Configured   bool
}

// adminProvider / adminToken — строки таблиц; client-токены — целиком
// (админ раздаёт их клиентам), apikey провайдера — маскирован (секрет апстрима).
// В токенах рядом область прав: текстом для таблицы и полями для data-атрибутов
// селекта примеров (фильтрация моделей по правам токена на клиенте).
// Форма админки всегда пишет область models — список моделей/хостов/алиасов;
// все/provider/model — legacy-области старых конфигов (сервер их признаёт,
// ✎ в строке показывает эквивалент на multi-select прав).
type adminProvider struct {
	Name         string
	BaseURL      string
	HasAPIKey    bool
	APIKeyMasked string
	APIKeyFull   string
}

type adminToken struct {
	ID            string
	Token         string
	Masked        string
	Rights        string // «все модели» / «провайдер X» / «модель X/Y» / «модели A, B»
	Scope         string // all|provider|model|models
	ScopeProvider string
	ScopeModel    string // короткая форма модели (у path-моделей с ведущим «/»)
	ScopeModels   string // список области models (полные формы через \n)
}

// adminBalance — строка таблицы балансировки: голое имя алиаса и участники
// (полные формы provider/model) — в ячейку через запятую и в data-атрибут
// (восстановление выбора в форме редактирования).
type adminBalance struct {
	Name         string
	ModelsJoined string
	Models       string // через \n (data-models для JS)
}

// admin — GET /admin (страница), POST /admin (мутации), GET /admin/api/models
// (JSON списка моделей из кэша; ?refresh=1 — немедленное обновление), logout.
func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/admin" || r.URL.Path == "/admin/"):
		s.adminWrite(w, r, adminData{}, "")
	case r.Method == http.MethodGet && r.URL.Path == "/admin/api/models":
		// ?refresh=1 — немедленный полный цикл обновления кэша (кнопка ⟳);
		// без параметра — текущий снимок (сервер сам обновляет раз в 10 мин).
		if r.URL.Query().Get("refresh") == "1" {
			s.mc.Refresh(r.Context())
		}
		entries := s.mc.Entries(r.Context(), "")
		entries = append(entries, s.balanceEntries(entries)...) // поверх кэша — баланс-алиасы конфига
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   entries,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/admin":
		s.adminPost(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/admin/logout":
		http.SetCookie(w, &http.Cookie{Name: AdminCookie, Value: "", Path: "/", MaxAge: -1})
		a := s.adminStringsFor(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html lang="`+a.Lang+`"><meta charset="utf-8"><title>`+htmlEsc(a.PageTitle)+`</title>
<style>body{font-family:sans-serif;max-width:32rem;margin:3rem auto}</style>
<p>`+fmt.Sprintf(a.LogoutFmt, `<code>/admin?token=…</code>`)+`</p>`)
	default:
		http.NotFound(w, r)
	}
}

// refreshModelsCache — фоновое немедленное обновление кэша списка моделей
// после изменения провайдеров: иначе новый провайдер (или исчезновение
// удалённого) подхватился бы только в ближайшем 10-минутном цикле.
// Контекст оторван от запроса: обновление не должно умирать с ответом.
func (s *Server) refreshModelsCache() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	go func() {
		defer cancel()
		s.mc.Refresh(ctx)
	}()
}

// tabOf — вкладка, на которой остаться после мутации (по op формы).
func tabOf(op string) string {
	switch {
	case strings.HasPrefix(op, "provider"):
		return "providers"
	case strings.HasPrefix(op, "token"):
		return "tokens"
	case strings.HasPrefix(op, "balance"):
		return "balances"
	case strings.HasPrefix(op, "langfuse"):
		return "langfuse"
	}
	return ""
}

// adminPost — разбор op из формы и мутации в store; результат — та же
// страница (на вкладке мутации) с notice/problem.
func (s *Server) adminPost(w http.ResponseWriter, r *http.Request) {
	a := s.adminStringsFor(r) // строки уведомлений/problem на языке сессии
	if err := r.ParseForm(); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, a.ProblemFormFmt, err)
		return
	}
	data := adminData{}
	problem := ""
	op := r.PostForm.Get("op")
	switch op {
	case "provider-add":
		name := strings.TrimSpace(r.PostForm.Get("name"))
		err := s.st.SetProvider(name, store.Provider{
			BaseURL: strings.TrimSpace(r.PostForm.Get("baseurl")),
			APIKey:  strings.TrimSpace(r.PostForm.Get("apikey")),
		})
		if err != nil {
			problem = err.Error()
		} else {
			data.Notice = template.HTML(fmt.Sprintf(a.NoticeProvSavedFmt, htmlEsc(name)))
			s.refreshModelsCache()
		}
	case "provider-delete":
		if _, err := s.st.DeleteProvider(r.PostForm.Get("name")); err != nil {
			problem = err.Error()
		} else {
			s.refreshModelsCache()
		}
	case "token-add", "token-edit":
		req := scopeFromForm(r.PostForm)
		if req.Scope == store.ScopeModel && req.Model == "" {
			// model пришла не полной формой provider/model — точный текст проблемы
			problem = a.ProblemModelRef
		}
		if problem == "" && op == "token-add" {
			tok, err := s.st.NewAPIToken(req)
			if err != nil {
				problem = err.Error()
			} else {
				data.Notice = template.HTML(fmt.Sprintf(a.NoticeTokenFmt,
					htmlEsc(tok.ID), htmlEsc(scopeRights(a, tok)), htmlEsc(a.TabTokens)))
			}
		}
		if problem == "" && op == "token-edit" {
			// права изменить — id из скрытого поля edit-id (видимый input заблокирован)
			tok, err := s.st.UpdateTokenScope(strings.TrimSpace(r.PostForm.Get("edit-id")), req)
			if err != nil {
				problem = err.Error()
			} else {
				data.Notice = template.HTML(fmt.Sprintf(a.NoticeTokenEditFmt,
					htmlEsc(tok.ID), htmlEsc(scopeRights(a, tok))))
			}
		}
	case "langfuse-set":
		l := store.Langfuse{
			Enabled:     r.PostForm.Get("enabled") == "on", // checkbox: снят — выгрузка выключена, настройки остаются
			URL:         strings.TrimSpace(r.PostForm.Get("url")),
			PublicKey:   strings.TrimSpace(r.PostForm.Get("public_key")),
			SecretKey:   strings.TrimSpace(r.PostForm.Get("secret_key")),
			Environment: strings.TrimSpace(r.PostForm.Get("environment")),
			Tags:        strings.Split(r.PostForm.Get("tags"), ","),
		}
		if l.Enabled && !hasHTTPScheme(l.URL) {
			problem = a.ProblemURLScheme
		} else if err := s.st.SetLangfuse(l); err != nil {
			problem = err.Error()
		} else {
			// экспортер обновляется на лету: следующие /v1-запросы — по новым настройкам
			s.lf.Update(langfuseConfig(l))
			switch {
			case l.On():
				data.Notice = template.HTML(fmt.Sprintf(a.NoticeLFOnFmt, htmlEsc(l.URL)))
			case l.Enabled: // переключатель вкл, но url/ключи не заполнены
				data.Notice = template.HTML(a.NoticeLFOff)
			default: // выключили переключателем — настройки сохраняются в файле
				data.Notice = template.HTML(a.NoticeLFOffToggled)
			}
		}
	case "token-delete":
		if _, err := s.st.DeleteToken(r.PostForm.Get("id")); err != nil {
			problem = err.Error()
		}
	case "balance-add", "balance-edit":
		name := strings.TrimSpace(r.PostForm.Get("name"))
		if op == "balance-edit" {
			// имя — ключ записи: берём из скрытого поля (видимый input заблокирован)
			name = strings.TrimSpace(r.PostForm.Get("edit-name"))
		}
		if err := s.st.SetBalance(name, r.PostForm["models"]); err != nil {
			problem = err.Error()
		} else {
			data.Notice = template.HTML(fmt.Sprintf(a.NoticeBalSavedFmt, htmlEsc(name)))
		}
	case "balance-delete":
		if _, err := s.st.DeleteBalance(r.PostForm.Get("name")); err != nil {
			problem = err.Error()
		}
	default:
		problem = a.ProblemUnknownOp
	}
	data.ActiveTab = tabOf(op)
	s.adminWrite(w, r, data, problem)
}

// adminWrite рендерит страницу админки.
func (s *Server) adminWrite(w http.ResponseWriter, r *http.Request, data adminData, problem string) {
	a := s.adminStringsFor(r)
	data.Str = a
	data.Version = version.Short()
	for _, p := range s.st.ProviderNames() {
		prov, _ := s.st.Provider(p)
		data.Providers = append(data.Providers, adminProvider{
			Name:         p,
			BaseURL:      prov.BaseURL,
			HasAPIKey:    prov.APIKey != "",
			APIKeyMasked: mask(prov.APIKey),
			APIKeyFull:   prov.APIKey,
		})
	}
	for _, t := range s.st.Tokens() {
		row := adminToken{
			ID:     t.ID,
			Token:  t.Token,
			Masked: mask(t.Token),
			Scope:  t.Scope, ScopeProvider: t.Provider, ScopeModel: t.Model,
			ScopeModels: strings.Join(t.Models, "\n"),
		}
		row.Rights = scopeRights(a, t) // права — на языке интерфейса
		data.Tokens = append(data.Tokens, row)
	}
	for _, b := range s.st.Balances() {
		data.Balances = append(data.Balances, adminBalance{
			Name:         b.Name,
			ModelsJoined: strings.Join(b.Models, ", "),
			Models:       strings.Join(b.Models, "\n"),
		})
	}
	data.AdminToken = s.st.AdminToken()
	data.ConfigPath = s.cfg.File
	// вкладка langfuse: действующие настройки + переключатель + статус включенности
	lf := s.st.LangfuseCfg()
	data.Langfuse = adminLangfuse{
		Enabled:      lf.Enabled,
		URL:          lf.URL,
		PublicKey:    lf.PublicKey,
		SecretKey:    lf.SecretKey,
		SecretMasked: mask(lf.SecretKey),
		Environment:  lf.Environment,
		Tags:         strings.Join(lf.Tags, ", "),
		On:           lf.On(),
		Configured:   lf.Valid(),
	}
	// base_url примеров вызова: из запуска (-base-url/env), иначе дефолт.
	data.BaseURL = s.cfg.BaseURL
	if data.BaseURL == "" {
		data.BaseURL = config.DefaultBaseURL
	}
	// ?token= прокидывается в формы и JS (fetch моделей), чтобы авторизация
	// не терялась (cookie уже поставлен, но пусть работает и без него).
	data.URLToken = r.URL.Query().Get("token")
	// подсказка «Примеров» — с живым адресом (бандл не знает запуска)
	data.ExHint = template.HTML(fmt.Sprintf(a.ExHintFmt, htmlEsc(data.BaseURL)))
	data.Problem = problem
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := adminPage.Execute(w, data); err != nil {
		s.log.Errorf("admin: рендер: %v", err)
	}
}

// scopeFromForm — область прав API-токена из полей формы token-add/token-edit:
// id (создание), scope/provider/model/models. Для scope=model поле model —
// полная форма provider/model (path-модели — с двойным слэшем), разбирается
// здесь; кривая форма обнуляет Model — вызывающий вернёт ProblemModelRef.
// Для scope=models список моделей приходит multi-select'ом (form["models"]).
func scopeFromForm(form url.Values) store.Token {
	req := store.Token{
		ID:       strings.TrimSpace(form.Get("id")),
		Scope:    strings.TrimSpace(form.Get("scope")),
		Provider: strings.TrimSpace(form.Get("provider")), // scope=provider|model
		Model:    strings.TrimSpace(form.Get("model")),    // scope=model — полная форма
		Models:   form["models"],                          // scope=models — multi-select
	}
	if req.Scope == store.ScopeModel {
		// полная форма; Cut по первому «/» отделяет провайдера
		// и у path-моделей (двойной слэш в форме)
		p, m, ok := strings.Cut(req.Model, "/")
		if !ok || p == "" || m == "" {
			req.Model = ""
		} else {
			req.Provider, req.Model = p, m
		}
	}
	return req
}

// htmlEsc — экранирование для доверенных HTML-строк.
func htmlEsc(s string) string {
	return template.HTMLEscapeString(s)
}

// hasHTTPScheme — явный http(s) у url (иначе langfuse-запрос не построится).
func hasHTTPScheme(u string) bool {
	parsed, err := url.Parse(u)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

// mask — маска секрета: первые 4 и последние 2 символа.
func mask(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 6 {
		return strings.Repeat("•", len(secret))
	}
	return secret[:4] + "…" + secret[len(secret)-2:]
}

// secretCell — ячейка секрета: маска в <code>, рядом глаз (показать/скрыть)
// и кнопка копирования; полный секрет живёт в data-атрибутах (DOM уже
// принадлежит залогиненному админу). Заголовки кнопок — на языке интерфейса.
func secretCell(masked, full string, a *adminStrings) template.HTML {
	e := template.HTMLEscapeString
	return template.HTML(
		`<code class="secret" data-mask="` + e(masked) + `" data-full="` + e(full) + `">` + e(masked) + `</code>` +
			`<button type="button" class="iconbtn" title="` + e(a.SecretEyeTitle) + `" onclick="secretEye(this)">👁</button>` +
			`<button type="button" class="iconbtn" title="` + e(a.SecretCopyTitle) + `" onclick="secretCopy(this)" data-secret="` + e(full) + `">⧉</button>`)
}
