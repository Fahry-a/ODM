<script setup>
import CopyButton from './components/CopyButton.vue'

// --- page data (was inline HTML in the single-file page) ---------------------

const navLinks = [
  { href: '#install', label: 'Install' },
  { href: '#quickstart', label: 'Quickstart' },
  { href: '#profiles', label: 'Profiles' },
  { href: '#rpc', label: 'RPC' },
  { href: 'https://github.com/Fahry-a/ODM', label: 'Source' },
]

const heroMeta = [
  'manual · edition v1.7.0',
  'linux · single static binary',
  'AUR · odm-bin',
  'MIT licence',
]

const stripChips = [
  'one budget · many connections',
  'work-stealing chunks',
  'resume · per-chunk sha-256',
  'json-rpc + websocket',
  'mirrors rotate per chunk',
  '429 back-off built in',
]

const installCmds = [
  'paru -S odm-bin',
  'yay -S odm-bin',
  'git clone https://github.com/Fahry-a/ODM && cd ODM && go build -o odm ./cmd/odm',
]

const modeRows = [
  { mode: 'A', when: 'one URL', controls: 'whole budget → the file', per: 'min(-c, 32)' },
  { mode: 'B', when: 'many URLs, no -sf', controls: 'files in parallel', per: '1' },
  { mode: 'C', when: 'many URLs + -sf N', controls: 'floor(-c / N) files at once', per: 'N + remainder' },
]

const profiles = [
  { name: 'odm', desc: 'Fixed chunks in a work-stealing queue over N real HTTP/1.1 connections. The default — multi-connection aggregation is the point of the tool.' },
  { name: 'aria2c', desc: 'Static equal split into --split segments, multiplexed as HTTP/2 streams over one connection. Mirrors aria2c’s -s/-x model.' },
  { name: 'both', desc: 'A 50/50 split: first half on the odm engine, second half on the aria2c engine — both working the same file at once. Under 4 MiB degrades to plain odm.' },
  { name: 'smart', desc: 'Probes range support, size, h2 readiness, then picks per file: small or non-rangeable → odm; ≥ 256 MiB with ≥ 6 conns → both; else → aria2c.' },
]

const rpcRows = [
  { method: 'odm.addUri / addBatch', what: 'queue one or many URLs' },
  { method: 'odm.pause · unpause · remove', what: 'steer running tasks (+ *All variants)' },
  { method: 'odm.tellStatus / Active / Waiting / Stopped', what: 'inspect tasks by state' },
  { method: 'odm.changeOption', what: 'rate limits + connections, mid-flight' },
  { method: 'odm.getGlobalStat · getVersion', what: 'counts and capabilities' },
  { method: 'odm.shutdown', what: 'wind the daemon down' },
]

const flags = [
  { flag: '-c, --connections', meaning: 'total connection budget', def: '5' },
  { flag: '-sf, --split-file', meaning: 'connections per file in batch mode', def: 'unset (1)' },
  { flag: '-s, --chunk-size', meaning: 'work-stealing chunk size', def: '4M' },
  { flag: '--profile', meaning: 'engine: odm | aria2c | both | smart', def: 'odm' },
  { flag: '-x, --continue', meaning: 'resume via .odm control file', def: 'on' },
  { flag: '-l, --limit-rate', meaning: 'global speed cap (token bucket)', def: 'off' },
  { flag: '-r / -w', meaning: 'retries per segment · backoff base', def: '3 · 2s' },
  { flag: '-t, --timeout', meaning: 'dial + headers timeout', def: '30s' },
  { flag: '-p, --proxy', meaning: 'http(s)/socks5 proxy', def: '—' },
  { flag: '--checksum / --checksum-url', meaning: 'verify md5/sha1/sha256', def: '—' },
  { flag: '--mirror', meaning: 'alternate source for the same file', def: '—' },
  { flag: '--auto-rename / --skip-existing', meaning: 'collision policies for existing files', def: '—' },
  { flag: '--session-log', meaning: 'JSONL event feed for wrappers/GUIs', def: '—' },
  { flag: '--dry-run / -q / -y', meaning: 'plan only · quiet · no prompt', def: '—' },
]

const exits = [
  { code: '0', what: 'all downloads succeeded' },
  { code: '1', what: 'invalid argument' },
  { code: '2', what: 'network error' },
  { code: '3', what: 'partial failure' },
  { code: '4', what: 'cancelled by user' },
]
</script>

<template>
<header class="nav-slab">
  <a class="slab-mark" href="#top">odm</a>
  <nav class="slab-nav" aria-label="Primary">
    <ul>
      <li v-for="link in navLinks" :key="link.href"><a :href="link.href">{{ link.label }}</a></li>
    </ul>
  </nav>
</header>

<main id="top">

<!-- hero declaration -->
<section class="hero">
  <h1>ONE BUDGET<span class="verb" aria-hidden="true">.</span> SPLIT IT <span class="verb">SENSIBLY</span>.</h1>
  <p class="hero-sub">A CLI download manager in Go that takes one connection budget and splits it sensibly — across files, across engines, across mirrors. Inspired by aria2c. Written in Go.</p>
  <ul class="hero-meta tnum">
    <li v-for="item in heroMeta" :key="item">{{ item }}</li>
  </ul>
</section>

<div class="rule-bold"></div>

<!-- the install plate: the page's one bleed-colour block -->
<section class="plate" id="install" aria-labelledby="install-h">
  <div class="plate-inner">
    <div class="plate-copy">
      <h2 id="install-h">get it on your machine<span aria-hidden="true">.</span></h2>
      <p>Prebuilt on the AUR for i686, x86_64, armv7h and aarch64 — no toolchain needed. Source builds need Go 1.26+. Tarballs for linux and darwin ship on every release.</p>
    </div>
    <div class="plate-cmds install-cmds">
      <div class="cmdrow" v-for="cmd in installCmds" :key="cmd">
        <code>{{ cmd }}</code>
        <CopyButton :text="cmd" />
      </div>
      <p class="note">Prefer a tarball? Grab one from the <a href="https://github.com/Fahry-a/ODM/releases/latest">releases page →</a></p>
    </div>
  </div>
</section>

<!-- feature strip: marquee on phones, static chips from tablet up -->
<div class="strip">
  <div class="strip-track">
    <div class="strip-group" role="list" aria-label="Capabilities">
      <span role="listitem" v-for="chip in stripChips" :key="chip">{{ chip }}</span>
    </div>
    <div class="strip-group" aria-hidden="true">
      <span v-for="chip in stripChips" :key="`dup-${chip}`">{{ chip }}</span>
    </div>
  </div>
</div>

<!-- quickstart -->
<section class="sect" id="quickstart" aria-labelledby="qs-h">
  <h2 id="qs-h">three invocations cover everything.</h2>
  <div class="sect-grid has-side">
    <div>
      <pre class="block"><code><span class="cmt"># one file, 16 parallel connections</span>
odm -c 16 https://files.test.xyz/file.tar.gz

<span class="cmt"># batch: -c = how many files run at once, 1 conn each</span>
odm -c 16 url1 url2 url3 …

<span class="cmt"># or 4 conns per file → 4 files at a time, rest queue</span>
odm -c 16 -sf 4 url1 url2 url3 …</code></pre>
      <p style="margin-top:var(--space-sm)">Large lists go in a file — one URL per line, <code>#</code> comments allowed:</p>
      <pre class="block"><code>odm -i file-list.txt</code></pre>
    </div>
    <aside class="side-object">
      <span class="label">what happens on enter</span>
      <ol class="mini-steps tnum">
        <li><b>probe</b> — HEAD → ranged GET → single-stream fallback per URL</li>
        <li><b>plan</b> — the balancer allocates connections (mode A/B/C)</li>
        <li><b>confirm</b> — one prompt with sizes and engines (<code>-y</code> skips)</li>
        <li><b>download</b> — workers pull chunks until the queue drains</li>
      </ol>
    </aside>
  </div>
</section>

<hr class="rule-bold">

<!-- balancer -->
<section class="sect" id="balancer" aria-labelledby="bal-h">
  <h2 id="bal-h">-c is a budget<span aria-hidden="true">,</span> not a guess.</h2>
  <div class="sect-grid has-side">
    <div>
      <table class="modes">
        <caption class="label">allocation modes</caption>
        <thead><tr><th scope="col">mode</th><th scope="col">when</th><th scope="col">-c controls</th><th scope="col">per file</th></tr></thead>
        <tbody class="tnum">
          <tr v-for="row in modeRows" :key="row.mode"><th scope="row">{{ row.mode }}</th><td>{{ row.when }}</td><td>{{ row.controls }}</td><td>{{ row.per }}</td></tr>
        </tbody>
      </table>
      <p>Servers refusing HTTP range requests get exactly one connection; the freed budget redistributes to the rest in the same pass. Above the soft ceiling of 32, odm warns and proceeds.</p>
    </div>
    <aside class="side-object">
      <span class="label">why work-stealing beats equal split</span>
      <p>A 100-chunk file over 8 connections: static tools give each connection 12–13 fixed ranges — one slow peer holds the tail hostage. The chunk queue hands out 4 MiB pieces on demand:</p>
      <div class="stepped tnum" role="img" aria-label="Chunks served: primary source 62, mirror 38 of 100">
        <i style="width:100%"></i>
        <i style="width:61%"></i>
      </div>
      <p class="tnum muted text-sm" style="margin:0">fast worker: 62 chunks<br>slow worker: 38 chunks — file finishes together</p>
    </aside>
  </div>
</section>

<hr class="rule-bold">

<!-- profiles -->
<section class="sect" id="profiles" aria-labelledby="prof-h">
  <h2 id="prof-h">four engines<span aria-hidden="true">.</span> one flag.</h2>
  <div class="sect-grid has-side">
    <div>
      <div class="profiles">
        <article class="profile" v-for="profile in profiles" :key="profile.name">
          <h3>{{ profile.name }}</h3>
          <p>{{ profile.desc }}</p>
        </article>
      </div>
    </div>
    <aside class="side-object">
      <span class="label">choosing by hand</span>
      <pre class="block" style="margin-top:0"><code><span class="cmt"># aria2c-style: 5 segments over h2</span>
odm --profile aria2c -c 5 big.iso

<span class="cmt"># tune the split</span>
odm --profile aria2c --split 4 \
    --min-split-size 10M big.iso</code></pre>
      <p class="text-sm">HTTP/2 negotiation needs HTTPS (ALPN). On plain http:// or h1-only servers the h2 profiles fall back automatically.</p>
    </aside>
  </div>
</section>

<hr class="rule-bold">

<!-- integrity -->
<section class="sect" id="integrity" aria-labelledby="int-h">
  <h2 id="int-h">interrupted is not corrupted.</h2>
  <div class="sect-grid has-side">
    <div>
      <p>Each download writes a <code>.odm</code> control file recording finished chunks. Re-run the same command and only the missing ranges are fetched. On resume the stored ETag rides as <code>If-Range</code>: if the remote file changed since the interruption, odm restarts cleanly instead of stitching old bytes to new ones.</p>
      <p>Mirrors (<code>--mirror URL</code>, repeatable) join the per-chunk rotation — a throttling source serves fewer chunks while the rest keep speed. Each mirror’s responses are validated independently, so a misbehaving one fails its own chunks, not your file.</p>
      <p>When a server answers 429, the global rate halves so every connection eases off together, then restores your configured cap after a quiet period.</p>
    </div>
    <aside class="side-object">
      <span class="label">three ways to verify</span>
      <ol class="mini-steps">
        <li><code>--checksum sha256:&lt;hex&gt;</code> — hash the written file</li>
        <li><code>--checksum-url</code> — fetch a sha256sum-style sidecar</li>
        <li><code>-i file.meta4</code> — Metalink4 embeds the strongest hash</li>
      </ol>
      <p class="text-sm muted">Completed chunks also carry per-chunk SHA-256 digests, verified against disk before any resume continues. A mismatch fails the task — never silent corruption.</p>
    </aside>
  </div>
</section>

<hr class="rule-bold">

<!-- rpc -->
<section class="sect" id="rpc" aria-labelledby="rpc-h">
  <h2 id="rpc-h">drive it from anywhere.</h2>
  <div class="sect-grid has-side">
    <div>
      <pre class="block"><code><span class="cmt"># start the daemon</span>
odm --rpc --rpc-secret hunter2 -d ~/Downloads &amp;

<span class="cmt"># add a download — token:&lt;secret&gt; first param</span>
curl -s -X POST http://127.0.0.1:6900/rpc \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"odm.addUri",
       "params":["token:hunter2","https://files.test.xyz/x.tar.gz"],"id":1}'</code></pre>
      <p>JSON-RPC 2.0 on <code>/rpc</code>, five WebSocket events on <code>/ws</code> (start, progress ≈250 ms, complete, error, pause). Default bind 127.0.0.1 — pair <code>--rpc-listen-all</code> with <code>--rpc-secret</code>.</p>
      <div class="rpc-index">
        <div class="rpc-row" v-for="row in rpcRows" :key="row.method"><code>{{ row.method }}</code><span>{{ row.what }}</span></div>
      </div>
    </div>
    <aside class="side-object">
      <span class="label">build on it</span>
      <p class="text-sm">The event feed carries the same field set as tellStatus, so a GUI needs exactly two endpoints to render live progress. For scripting without RPC, <code>--session-log</code> writes JSONL events to a file.</p>
    </aside>
  </div>
</section>

<hr class="rule-bold">

<!-- spec sheet -->
<section id="flags">
  <div class="specwrap">
    <p class="label" id="flags-h">flag reference</p>
    <table class="spec-sheet tnum">
      <thead><tr><th scope="col">flag</th><th scope="col">meaning</th><th scope="col">default</th></tr></thead>
      <tbody>
        <tr v-for="row in flags" :key="row.flag"><td><code>{{ row.flag }}</code></td><td>{{ row.meaning }}</td><td>{{ row.def }}</td></tr>
      </tbody>
    </table>
    <p class="fine-note muted text-sm">Full list: <code>odm --help</code> · config reference: <a href="https://github.com/Fahry-a/ODM/blob/main/configs/odm.conf.example">odm.conf.example</a></p>
  </div>
</section>

<hr class="rule-bold">

<!-- exit codes -->
<section class="sect" aria-labelledby="exit-h" style="padding-block: var(--space-lg);">
  <p class="label" id="exit-h" style="margin-top:0">exit codes</p>
  <div class="exits" style="margin-top: var(--space-xs);">
    <div class="exit" v-for="exit in exits" :key="exit.code"><b>{{ exit.code }}</b><div>{{ exit.what }}</div></div>
  </div>
</section>

</main>

<footer class="foot-stmt">
  <p class="foot-stmt__line">One budget<em>.</em> Split it sensibly<em>.</em></p>
  <div class="foot-stmt__meta">
    <span class="wordmark">odm</span>
    <span class="links">
      <a href="https://github.com/Fahry-a/ODM">GitHub</a>
      <a href="https://aur.archlinux.org/packages/odm-bin">AUR</a>
      <a href="https://github.com/Fahry-a/ODM/releases/latest">Releases</a>
    </span>
    <span class="foot-stmt__fine">a Go program by Fahry-a · MIT · this page documents v1.7.0</span>
  </div>
</footer>
</template>
