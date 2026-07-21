package main

// dashboardHTML é a página de gerência servida em /dashboard. Autossuficiente:
// pede a API key uma vez (fica no localStorage do navegador) e consome as
// rotas do serve com o header apikey — a chave nunca aparece em URLs.
const dashboardHTML = `<!doctype html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>wacli · ponte WhatsApp</title>
<style>
  :root { --bg:#0b0b0d; --card:#151519; --ink:#f2ede7; --muted:#9a938c; --ok:#4ade80; --warn:#fbbf24; --bad:#f87171; --accent:#dcbc75; }
  * { margin:0; padding:0; box-sizing:border-box; }
  body { background:var(--bg); color:var(--ink); font:15px/1.5 -apple-system, system-ui, sans-serif; padding:32px 20px; }
  main { max-width:760px; margin:0 auto; display:grid; gap:18px; }
  h1 { font-size:20px; letter-spacing:.02em; }
  h1 em { font-style:normal; color:var(--accent); }
  .card { background:var(--card); border:1px solid #26262c; border-radius:12px; padding:20px; }
  .row { display:flex; align-items:center; gap:10px; flex-wrap:wrap; }
  .pill { display:inline-flex; align-items:center; gap:8px; padding:6px 14px; border-radius:999px; font-weight:600; font-size:13px; }
  .pill::before { content:""; width:9px; height:9px; border-radius:50%; background:currentColor; }
  .open { color:var(--ok); background:rgba(74,222,128,.12); }
  .connecting { color:var(--warn); background:rgba(251,191,36,.12); }
  .close { color:var(--bad); background:rgba(248,113,113,.12); }
  button { background:var(--accent); color:#111; border:0; border-radius:8px; padding:9px 16px; font-weight:700; cursor:pointer; font-size:14px; }
  button.ghost { background:transparent; color:var(--muted); border:1px solid #333; }
  input { background:#0f0f12; border:1px solid #333; color:var(--ink); border-radius:8px; padding:9px 12px; font-size:14px; flex:1; min-width:180px; }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th, td { text-align:left; padding:7px 10px; border-bottom:1px solid #222; }
  th { color:var(--muted); font-weight:600; text-transform:uppercase; font-size:11px; letter-spacing:.08em; }
  td.ack-read { color:var(--ok); } td.ack-delivered { color:var(--ok); } td.ack-sent { color:var(--warn); }
  #qr-wrap { display:none; text-align:center; }
  #qr-wrap img { width:260px; height:260px; border-radius:8px; background:#fff; padding:10px; }
  .hint { color:var(--muted); font-size:13px; margin-top:8px; }
  #toast { position:fixed; bottom:24px; left:50%; transform:translateX(-50%); background:#222; padding:10px 18px; border-radius:8px; opacity:0; transition:opacity .3s; }
</style>
</head>
<body>
<main>
  <h1>wacli · ponte <em>WhatsApp</em></h1>

  <section class="card row" id="status-card">
    <span class="pill close" id="state-pill">…</span>
    <span class="hint" id="state-hint"></span>
    <span style="flex:1"></span>
    <button class="ghost" id="btn-restart">Reiniciar conexão</button>
    <button class="ghost" id="btn-logout-key">Trocar chave</button>
  </section>

  <section class="card" id="qr-wrap">
    <h2 style="font-size:16px;margin-bottom:12px">Parear dispositivo</h2>
    <img id="qr-img" alt="QR de pareamento">
    <p class="hint">WhatsApp → Configurações → Dispositivos conectados → Conectar dispositivo.<br>O QR renova sozinho — escaneie quando quiser.</p>
  </section>

  <section class="card">
    <h2 style="font-size:16px;margin-bottom:12px">Enviar teste</h2>
    <div class="row">
      <input id="send-to" placeholder="Número (ex.: 5561999998888)">
      <input id="send-text" placeholder="Mensagem" value="Teste da ponte wacli">
      <button id="btn-send">Enviar</button>
    </div>
  </section>

  <section class="card">
    <h2 style="font-size:16px;margin-bottom:12px">Últimos envios</h2>
    <table>
      <thead><tr><th>Quando (UTC)</th><th>Para</th><th>Status</th></tr></thead>
      <tbody id="sent-rows"><tr><td colspan="3" class="hint">—</td></tr></tbody>
    </table>
  </section>
</main>
<div id="toast"></div>
<script>
const $ = id => document.getElementById(id);
function key() {
  let k = localStorage.getItem('wacli_key');
  while (!k) { k = prompt('API key da ponte:') || ''; if (k) localStorage.setItem('wacli_key', k); }
  return k;
}
async function api(path, options = {}) {
  const response = await fetch(path, { ...options, headers: { apikey: key(), 'content-type': 'application/json', ...(options.headers || {}) } });
  if (response.status === 401) { localStorage.removeItem('wacli_key'); throw new Error('chave inválida'); }
  return response;
}
function toast(message) { const t = $('toast'); t.textContent = message; t.style.opacity = 1; setTimeout(() => t.style.opacity = 0, 2600); }

async function refresh() {
  try {
    const qrState = await (await api('/qr')).json();
    const pill = $('state-pill');
    pill.className = 'pill ' + qrState.state;
    pill.textContent = { open: 'Conectado', connecting: qrState.pairing ? 'Aguardando QR' : 'Conectando…', close: 'Desconectado' }[qrState.state] || qrState.state;
    $('state-hint').textContent = qrState.state === 'open' ? 'Sessão ativa — envios operacionais.' : '';
    const wrap = $('qr-wrap');
    if (qrState.pairing && qrState.qr) {
      wrap.style.display = 'block';
      const png = await api('/qr.png');
      if (png.ok) $('qr-img').src = URL.createObjectURL(await png.blob());
    } else {
      wrap.style.display = 'none';
    }
    const recent = await (await api('/messages/recent')).json();
    const rows = (recent.sent || []).slice(0, 15).map(item =>
      '<tr><td>' + item.time.replace('T', ' ').slice(0, 19) + '</td><td>' + item.to.replace('@s.whatsapp.net', '') + '</td><td class="ack-' + item.ack + '">' + item.ack + '</td></tr>'
    ).join('');
    $('sent-rows').innerHTML = rows || '<tr><td colspan="3" class="hint">Nenhum envio ainda.</td></tr>';
  } catch (error) {
    $('state-pill').className = 'pill close';
    $('state-pill').textContent = 'Erro: ' + error.message;
  }
}
$('btn-restart').onclick = async () => { await api('/instance/restart/bridge', { method: 'POST' }); toast('Reinício disparado.'); };
$('btn-logout-key').onclick = () => { localStorage.removeItem('wacli_key'); location.reload(); };
$('btn-send').onclick = async () => {
  const response = await api('/message/sendText/bridge', { method: 'POST', body: JSON.stringify({ number: $('send-to').value.trim(), text: $('send-text').value }) });
  const payload = await response.json();
  toast(response.ok ? 'Enviado: ' + payload.key.id : 'Falhou: ' + (payload.error || response.status));
  refresh();
};
refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`
