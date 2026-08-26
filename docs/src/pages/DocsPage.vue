<script setup>
import { nextTick, onMounted, onUnmounted, ref } from 'vue'
import DownloadAnimation from '../components/DownloadAnimation.vue'


// --- page data (was inline HTML in the single-file page) ---------------------

const navLinks = [
  { href: '#install', label: 'Install' },
  { href: '#quickstart', label: 'Quickstart' },
  { href: '#profiles', label: 'Profiles' },
  { href: '#rpc', label: 'RPC' },
  { href: '#/docs', label: 'Docs' },
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


// --- docs scroll-spy: which section is under the top rule --------------------
const activeSection = ref('')
const onScroll = () => {
  if (!window.matchMedia('(min-width: 64rem)').matches) return
  const bandTop = 3.65 * 16 + 24 // sticky header + breathing room
  let current = ''
  for (const { id } of docsSections) {
    const el = document.getElementById(id)
    if (el && el.getBoundingClientRect().top - bandTop <= 0) current = id
  }
  if (current) activeSection.value = current
}

onMounted(() => {
  nextTick(() => {
    onScroll()
    window.addEventListener('scroll', onScroll, { passive: true })
  })
})

const docsSections = [
  { id: 'docs-install', title: 'Installation' },
  { id: 'docs-concepts', title: 'Core concepts' },
  { id: 'docs-balancer', title: 'Balancer & scheduling' },
  { id: 'docs-cli', title: 'CLI reference' },
  { id: 'docs-profiles', title: 'Profiles' },
  { id: 'docs-resume', title: 'Resume and integrity' },
  { id: 'docs-rate', title: 'Rate limiting' },
  { id: 'docs-rpc', title: 'RPC reference' },
  { id: 'docs-config', title: 'Configuration' },
  { id: 'docs-progress', title: 'Progress bar & environment' },
  { id: 'docs-exit', title: 'Exit codes' },
  { id: 'docs-examples', title: 'Examples' },
  { id: 'docs-operations', title: 'Operations playbook' },
  { id: 'docs-security', title: 'Security notes' },
  { id: 'docs-structure', title: 'Documentation map' },
  { id: 'docs-limits', title: 'Known limitations' },
  { id: 'docs-troubleshooting', title: 'Troubleshooting' },
]

const cliGroups = [
  {
    title: 'Input and output',
    rows: [
      ['URL...', 'Download one or more URLs. Multiple URLs trigger the scheduler modes described below. Space-separated positional args are the recommended form; a comma inside a URL (e.g. ?ids=1,2,3) is ambiguous in the legacy comma form.'],
      ['-i, --input-file', 'Read a URL list from FILE — one per line, # comments and blanks skipped. Local .meta4 / .metalink Metalink4 files and remote Metalink documents are supported.'],
      ['-d, --dir', 'Destination directory for final files and .odm resume sidecars. Defaults to the current working directory.'],
      ['-o, --output', 'Override the output filename. Single-file mode only; batches use server names or Metalink names.'],
      ['--auto-rename', 'Keep an existing destination and save as name.<N>.ext instead. Mutually exclusive with --skip-existing.'],
      ['--skip-existing', 'Skip files already present with a matching size. Useful for idempotent scripts.'],
    ],
  },
  {
    title: 'Connection budget and scheduling',
    rows: [
      ['-c, --connections', 'The total parallel-connection budget. ODM allocates it across files and chunks by scheduler mode (A/B/C, see Balancer & scheduling). Default: 5.'],
      ['-m, --max-connections', 'Soft ceiling for the budget; exceeding it only prints a warning. Default: 32. Some servers treat >~30 concurrent connections from one IP as abusive; raise it on LANs / local CDNs.'],
      ['-sf, --split-file', 'Connections per active file in batch mode. Unset means 1 per file; with -c 16 -sf 4, four files run at once.'],
      ['-s, --chunk-size', 'Work-stealing chunk size, e.g. 4M. Default: 4M. Smaller chunks rebalance faster; larger chunks reduce overhead.'],
      ['--profile', 'Select odm, aria2c, both, or smart. Smart probes each URL and chooses an engine.'],
      ['--split', 'Aria2c-style segment count for the aria2c profile. Default: 5.'],
      ['--min-split-size', 'Aria2c: do not split ranges below twice this size. Default: 20M.'],
      ['--max-connection-per-server', 'Aria2c: per-server connection cap in the h1 fallback. Default: 1.'],
    ],
  },
  {
    title: 'Network behavior',
    rows: [
      ['-t, --timeout', 'Dial and response-header timeout, in seconds. Default: 30.'],
      ['-r, --retry', 'Retries per segment on failure. Default: 3.'],
      ['-w, --retry-wait', 'Delay between retries, in seconds. Default: 2.'],
      ['-n, --max-redirect', 'Redirect hops to follow. Default: 5.'],
      ['-u, --user-agent', 'Custom User-Agent. Default: odm/<version>.'],
      ['-H, --header', 'Add a custom header as K:V. Repeatable.'],
      ['--load-cookies', 'Load a Netscape cookies.txt and send it as a Cookie header.'],
      ['--referer', 'Set the Referer header.'],
      ['-p, --proxy', 'HTTP, HTTPS, or SOCKS5 proxy URL. SOCKS5H resolves hostnames through the proxy.'],
      ['--check-certificate', 'Verify TLS. Default: true.'],
      ['--mirror', 'Alternate URL serving the same file. Repeatable; chunk requests rotate across all sources, and each mirror is validated independently.'],
    ],
  },
  {
    title: 'Rate limiting and integrity',
    rows: [
      ['-l, --limit-rate', 'Global token-bucket download cap shared by every worker of every task, e.g. 5M or 500K.'],
      ['--limit-rate-per-task', 'Per-task speed cap stacked on top of the global one, e.g. 2M. Effective cap is min(per-task, global).'],
      ['--checksum', 'Verify the final file with md5, sha1, or sha256, syntax algo:hash such as sha256:<hex>.'],
      ['--checksum-url', 'Fetch the checksum from a sidecar URL (sha256sum/md5sum-style file or bare hash) and verify the final file.'],
      ['-x, --continue', 'Resume an incomplete file via the .odm control file. Default: on.'],
      ['--dry-run', 'Probe every URL and show the download plan; nothing is downloaded.'],
      ['-y, --yes', 'Skip the interactive confirmation prompt.'],
      ['-q, --quiet', 'No progress bar (cron/scripts); also skips the prompt.'],
      ['--session-log', 'Append JSONL progress/summary events to FILE for wrappers and GUIs.'],
    ],
  },
  {
    title: 'Config, logging, and RPC',
    rows: [
      ['--config', 'Config file path. Default: /etc/odm/config.conf (user file: ~/.config/odm/config.conf).'],
      ['-L, --log', 'Mirror logs to FILE.'],
      ['--log-level', 'Log verbosity: debug | info | warn | error. Default: info.'],
      ['--rpc', 'Run as the RPC server (daemon).'],
      ['--rpc-listen-port', 'TCP port for the RPC listener. Default: 6900.'],
      ['--rpc-listen-all', 'Bind 0.0.0.0 instead of 127.0.0.1. Pair with --rpc-secret.'],
      ['--rpc-secret', 'Auth token for RPC (token:<secret> param or ?secret= on the WebSocket upgrade).'],
      ['--rpc-tls-cert / --rpc-tls-key', 'PEM certificate + key; when both are provided the server switches to HTTPS/WSS.'],
      ['-V, --version / -h, --help', 'Print version and exit / show help.'],
    ],
  },
]

const rpcMethods = [
  ['odm.addUri', 'Queue one URL and return its GID. The first param may be token:<secret> when auth is enabled.'],
  ['odm.addBatch', 'Queue many URLs in one request. ODM applies the same scheduler logic as the CLI.'],
  ['odm.tellStatus', 'Return the same progress view used by the terminal UI: state, bytes, speed, ETA, conns, and error.'],
  ['odm.tellActive / tellWaiting / tellStopped', 'List tasks by lifecycle bucket for dashboards and queue managers.'],
  ['odm.pause / unpause / remove', 'Control a single task.'],
  ['odm.pauseAll / unpauseAll', 'Pause / resume every task in one call.'],
  ['odm.changeOption', 'Adjust max-download-limit, max-download-limit-per-task, or connections while a task is running.'],
  ['odm.getGlobalStat', 'Inspect aggregate active / waiting / stopped counts across the daemon.'],
  ['odm.getVersion', 'Read the daemon version and enabled feature surface.'],
  ['odm.shutdown', 'Ask the daemon to stop after current bookkeeping is complete.'],
]

const rpcEvents = [
  ['onDownloadStart', 'a task is added via odm.addUri'],
  ['onDownloadProgress', 'periodic snapshot — throttled to ~250 ms'],
  ['onDownloadComplete', 'a task finishes cleanly'],
  ['onDownloadError', 'a task fails or is cancelled'],
  ['onDownloadPause', 'a task is paused via odm.pause'],
]

const configKeys = [
  ['connections / max-connections / split-file', 'the balancer budget (defaults 5 / 32 / unset)'],
  ['dir / output', 'destination directory (default cwd) / output name (single-file only)'],
  ['max-redirect / retry / retry-wait / timeout', '5 / 3 / 2 / 30'],
  ['user-agent / header / load-cookies / referer', 'request identity and custom headers'],
  ['proxy / check-certificate', 'proxy URL / TLS verification (default true)'],
  ['checksum / checksum-url / mirror', 'final verification and alternate sources (mirror repeatable)'],
  ['limit-rate / limit-rate-per-task', 'global and per-task token-bucket caps'],
  ['chunk-size', 'work-stealing chunk size (default 4M)'],
  ['profile / split / min-split-size / max-connection-per-server', 'engine profile and aria2c tuning (odm / 5 / 20M / 1)'],
  ['continue / quiet / dry-run / auto-rename / skip-existing / session-log', 'behaviour switches'],
  ['rpc / rpc-listen-port / rpc-listen-all / rpc-secret / rpc-tls-cert / rpc-tls-key', 'daemon settings (port 6900, loopback bind, auth token, TLS)'],
  ['log / log-level', 'log mirror file / verbosity (debug|info|warn|error, default info)'],
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
<main id="docs" class="docs-page">
  <section class="docs-hero" aria-labelledby="docs-title">
    <p class="label">Reference manual · v1.7.0</p>
    <h1 id="docs-title">ODM Documentation</h1>
    <p class="docs-lead">A detailed field guide for installing, configuring, scripting, and operating ODM: the Go download manager that treats connections as one budget and then splits that budget across files, chunks, profiles, mirrors, and RPC clients.</p>
    <DownloadAnimation compact />
    <div class="docs-actions">
      <a class="docs-cta" href="#top">Back to landing</a>
      <a class="docs-cta ghost" href="https://github.com/Fahry-a/ODM">Source on GitHub</a>
    </div>
  </section>

  <div class="docs-layout">
    <aside class="docs-toc" aria-label="Documentation sections">
      <p class="label">On this page</p>
      <a
        v-for="section in docsSections"
        :key="section.id"
        :href="`#${section.id}`"
        :class="{ active: activeSection === section.id }"
        :aria-current="activeSection === section.id ? 'location' : undefined"
      >{{ section.title }}</a>
    </aside>

    <div class="docs-content">
      <section id="docs-install" class="docs-block">
        <h2>Installation</h2>
        <p>Use the AUR package when you want a prebuilt binary on Arch-based systems, or build from source when you want the exact current branch. The binary ships as a single static file with no runtime dependencies.</p>
        <pre class="block"><code>paru -S odm-bin
yay -S odm-bin
git clone https://github.com/Fahry-a/ODM
cd ODM
go build -o odm ./cmd/odm</code></pre>
        <p>Release artifacts are named <code>odm_X.Y.Z_&lt;os&gt;_&lt;arch&gt;</code>; tarballs for linux and darwin ship on every release. Prebuilt on the AUR for i686, x86_64, armv7h and aarch64.</p>
        <div class="docs-callout"><b>Verify the install</b><span>Run <code>odm --version</code>, then <code>odm --help</code>. The version string should match the current release documented by this site.</span></div>
      </section>

      <section id="docs-concepts" class="docs-block">
        <h2>Core concepts</h2>
        <p>ODM is an aria2c-inspired download manager written in Go. Its core differentiator is the <b>Connection Balancer</b>: automatic allocation of parallel connections that adapts between single-file and batch modes, so one connection budget (<code>-c</code>) is split sensibly across files instead of you computing connections-per-file by hand.</p>
        <div class="docs-cards">
          <article><h3>Budget first</h3><p><code>-c</code> is not blindly copied to every file. ODM treats it as a global budget and allocates it by scheduler mode (A/B/C).</p></article>
          <article><h3>Chunks are work</h3><p>The default engine slices rangeable files into chunks. Workers steal the next unfinished chunk instead of owning a fixed slice forever.</p></article>
          <article><h3>State is resumable</h3><p>A sidecar <code>.odm</code> file records URL, total size, chunk size, and completed chunks so interrupted work continues safely and is deleted on clean completion.</p></article>
          <article><h3>RPC mirrors CLI</h3><p>The daemon exposes queueing, status, pause, resume, removal, limits, and shutdown through JSON-RPC 2.0 on <code>/rpc</code> and event notifications on <code>/ws</code>.</p></article>
        </div>
        <p>Run <code>odm --rpc</code> to act as the daemon; otherwise the same engine runs in the foreground and renders a pacman-style (<code>ILoveCandy</code>) progress bar.</p>
      </section>

      <section id="docs-balancer" class="docs-block">
        <h2>Balancer &amp; scheduling</h2>
        <p>What <code>-c</code> controls depends on how many URLs you pass and whether <code>-sf</code> is set. The balancer picks one of three modes on every scheduling pass:</p>
        <table class="docs-table"><tbody>
          <tr><th scope="row">Mode A</th><td>one URL (<code>-sf</code> ignored): the whole budget goes to the one file, capped at <code>min(-c, --max-connections)</code>.</td></tr>
          <tr><th scope="row">Mode B</th><td>many URLs, no <code>-sf</code>: <code>-c</code> decides how many files run in parallel, 1 connection each.</td></tr>
          <tr><th scope="row">Mode C</th><td>many URLs + <code>-sf N</code>: <code>floor(-c / N)</code> files run in parallel with N connections each; the remainder is distributed to the first files.</td></tr>
        </tbody></table>
        <p>Files that do not support HTTP range requests — the probe falls back HEAD → ranged GET → single-stream — are given exactly one connection, and the freed budget is redistributed to the other files in the same pass.</p>
        <p><code>--max-connections</code> (default 32) is a soft ceiling: going above it only prints a warning, because many CDNs treat more than ~30 concurrent connections from one IP as abusive. Power users on a LAN or local CDN can raise it.</p>
      </section>

      <section id="docs-cli" class="docs-block">
        <h2>CLI reference</h2>
        <p>These groups cover the options used most often in real scripts and terminals. For the canonical generated list, run <code>odm --help</code>.</p>
        <div class="docs-table-group" v-for="group in cliGroups" :key="group.title">
          <h3>{{ group.title }}</h3>
          <table class="docs-table"><tbody><tr v-for="row in group.rows" :key="row[0]"><th scope="row"><code>{{ row[0] }}</code></th><td>{{ row[1] }}</td></tr></tbody></table>
        </div>
      </section>

      <section id="docs-profiles" class="docs-block">
        <h2>Profiles</h2>
        <p>Choose <code>odm</code> for the work-stealing engine, <code>aria2c</code> for h2-oriented static segments, <code>both</code> to split one file between both strategies, or <code>smart</code> to let probes decide per URL.</p>
        <ul class="docs-list">
          <li><b>odm</b> — fixed chunks in a work-stealing queue over N real HTTP/1.1 connections. The default; multi-connection aggregation is the point of the tool.</li>
          <li><b>aria2c</b> — static equal split into <code>--split</code> segments, multiplexed as HTTP/2 streams over one connection. Mirrors aria2c’s <code>-s</code>/<code>-x</code> model; tune with <code>--split</code>, <code>--min-split-size</code>, and <code>--max-connection-per-server</code>.</li>
          <li><b>both</b> — a 50/50 split: the first half on the odm engine, the second half on the aria2c engine, both working the same file at once. Under 4 MiB it degrades to plain odm. HTTP/2 negotiation needs HTTPS (ALPN); on plain http:// or h1-only servers the h2 profiles fall back automatically.</li>
          <li><b>smart</b> — probes range support, size, and h2 readiness, then picks per file: small or non-rangeable → odm; ≥ 256 MiB with ≥ 6 connections → both; otherwise → aria2c.</li>
        </ul>
        <pre class="block"><code>odm --profile odm -c 16 big.iso
odm --profile aria2c --split 8 --min-split-size 16M big.iso
odm --profile both -c 12 big.iso
odm --profile smart -c 16 url1 url2 url3</code></pre>
      </section>

      <section id="docs-resume" class="docs-block">
        <h2>Resume and integrity</h2>
        <p>With <code>-x / --continue</code> (on by default), each download writes a <code>&lt;file&gt;.odm</code> control file — JSON recording URL, total size, chunk size, and the list of completed chunks. On re-run with the same destination present, only the unfinished chunks are re-fetched: no corruption, no restart. The control file is deleted on a clean completion.</p>
        <p>Resume is conservative by design: on the next attempt the stored ETag rides as <code>If-Range</code>, so if the remote object changed since the interruption ODM restarts cleanly instead of stitching old bytes to new ones.</p>
        <ul class="docs-list"><li>Completed chunks are verified against stored per-chunk SHA-256 digests before any resume continues; a mismatch fails the task — never silent corruption.</li><li>Final checksums can come from <code>--checksum</code>, <code>--checksum-url</code>, or Metalink metadata.</li><li>Mirrors are validated independently so a bad mirror fails its own chunks rather than corrupting the output silently.</li></ul>
      </section>

      <section id="docs-rate" class="docs-block">
        <h2>Rate limiting</h2>
        <p><code>--limit-rate</code> is enforced with a <b>global token bucket</b> shared across all active workers of all tasks — not split per connection. Throttling happens at the data-stream level (bytes read from the network, before writing to disk), so aggregate throughput stays close to the configured cap regardless of how many connections are alive or how the batch queue advances.</p>
        <p>An optional <code>--limit-rate-per-task</code> adds a per-task token bucket stacked on top of the global one. Each worker acquires tokens from both buckets before writing to disk, so a single task is capped to <code>min(per-task, global)</code> and the sum across all tasks never exceeds the global cap.</p>
        <pre class="block"><code># cap everything at 5 MiB/s aggregate
odm -l 5M https://files.test.xyz/big.iso

# 5 MiB/s global, but one task may use at most 2 MiB/s of it
odm -l 5M --limit-rate-per-task 2M https://files.test.xyz/a.iso https://files.test.xyz/b.iso</code></pre>
        <p>Both limits can be changed at runtime via RPC <code>odm.changeOption</code>. When a server answers 429, the global cap eases down so every connection backs off together, then restores your configured cap after a quiet period.</p>
      </section>

      <section id="docs-rpc" class="docs-block">
        <h2>RPC reference</h2>
        <p>Run <code>odm --rpc</code> to act as a daemon exposing JSON-RPC 2.0 + WebSocket, so third-party GUIs and scripts can drive it without parsing terminal output:</p>
        <pre class="block"><code>POST  http://127.0.0.1:6900/rpc   (JSON-RPC 2.0)
 WS   ws://127.0.0.1:6900/ws      (event notifications)</code></pre>
        <p>The default bind is <code>127.0.0.1</code> — safe for local automation. <code>--rpc-listen-all</code> binds <code>0.0.0.0</code>; pair that with <code>--rpc-secret</code>. Auth is aria2-style: the first JSON-RPC param is <code>token:&lt;secret&gt;</code> (or <code>?secret=&lt;value&gt;</code> on the WebSocket upgrade).</p>
        <p>TLS is available via <code>--rpc-tls-cert</code> and <code>--rpc-tls-key</code> (PEM). When both are provided the server switches to HTTPS/WSS and prints an <code>https://</code> URL.</p>
        <pre class="block"><code>odm --rpc --rpc-secret hunter2 -d ~/Downloads
curl -s http://127.0.0.1:6900/rpc \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"odm.getVersion","params":["token:hunter2"],"id":1}'</code></pre>
        <div class="docs-table-group">
          <h3>Methods</h3>
          <table class="docs-table"><tbody><tr v-for="method in rpcMethods" :key="method[0]"><th scope="row"><code>{{ method[0] }}</code></th><td>{{ method[1] }}</td></tr></tbody></table>
        </div>
        <div class="docs-table-group">
          <h3>WebSocket events (…/ws)</h3>
          <table class="docs-table"><tbody><tr v-for="event in rpcEvents" :key="event[0]"><th scope="row"><code>{{ event[0] }}</code></th><td>{{ event[1] }}</td></tr></tbody></table>
          <p>Each event’s <code>params</code> carries the same field set as <code>odm.tellStatus</code>’s result, so a GUI needs exactly these two endpoints to render live progress.</p>
        </div>
      </section>

      <section id="docs-config" class="docs-block">
        <h2>Configuration</h2>
        <p>Configuration files mirror CLI behavior for defaults you do not want to repeat; CLI flags remain the clearest place to express one-off choices in scripts. Key names match the CLI long-flags without the leading <code>--</code>.</p>
        <p><b>Precedence</b> (a defaulted flag never overwrites a value the user put in a file):</p>
        <pre class="block"><code>CLI args &gt; ~/.config/odm/config.conf &gt; /etc/odm/config.conf &gt; defaults</code></pre>
        <p>Format is <code>key = value</code>, one option per line, <code>#</code> starts a comment. The shipped template lives at <code>configs/odm.conf.example</code> (or <code>/etc/odm/config.conf.example</code> once installed).</p>
        <pre class="block"><code># common pattern
dir = ~/Downloads
connections = 16
profile = smart
continue = true
limit-rate = 0</code></pre>
        <div class="docs-table-group">
          <h3>Every key</h3>
          <table class="docs-table"><tbody><tr v-for="row in configKeys" :key="row[0]"><th scope="row"><code>{{ row[0] }}</code></th><td>{{ row[1] }}</td></tr></tbody></table>
        </div>
      </section>

      <section id="docs-progress" class="docs-block">
        <h2>Progress bar &amp; environment</h2>
        <p>When stderr/stdout is a TTY, odm renders a pacman / CachyOS-style (<code>ILoveCandy</code>) progress bar. The per-file line format:</p>
        <pre class="block"><code>&lt;file&gt;  &lt;done/total&gt;  &lt;speed&gt;/s  &lt;ETA&gt;  [x&lt;N&gt;-&lt;bar&gt;]  &lt;percent&gt;%</code></pre>
        <ul class="docs-list">
          <li><b>&lt;done/total&gt;</b> — hybrid downloaded/total size with compact suffixes (e.g. <code>42.0M/256.0M</code>) that grows live as bytes arrive.</li>
          <li><b>x&lt;N&gt;</b> — the number of parallel connections currently used by that file (e.g. <code>[x4-----c  o  o  o  o]</code>). <code>c</code> is the pacman icon moving left to right; <code>o</code> is a dot not yet eaten; blank/<code>-</code> is already eaten. At 100% the whole bar is blank/dashes.</li>
          <li><b>Colors</b> (when the terminal supports ANSI): completed = green, downloading = yellow, retrying after an error = red, queued/waiting = dim/grey. Disabled automatically on non-TTY.</li>
        </ul>
        <p>If stdout is not a terminal — <code>-q/--quiet</code> or output redirected to a file/pipe — the progress bar is replaced with periodic log lines without ANSI cursor control. <code>--session-log FILE</code> appends JSONL lifecycle events (start, progress, complete, error, pause) for wrappers and GUIs.</p>
        <div class="docs-callout"><b>Environment</b><span><code>NO_COLOR</code> — if set, disables colored progress output even when writing to a TTY.</span></div>
      </section>

      <section id="docs-exit" class="docs-block">
        <h2>Exit codes</h2>
        <p>odm exits with one of these codes, useful for scripting and cron:</p>
        <table class="docs-table"><tbody><tr v-for="exit in exits" :key="exit.code"><th scope="row"><code>{{ exit.code }}</code></th><td>{{ exit.what }}</td></tr></tbody></table>
      </section>

      <section id="docs-examples" class="docs-block">
        <h2>Examples</h2>
        <pre class="block"><code># single file, 16 parallel connections
odm -c 16 https://files.test.xyz/file.tar.gz

# batch: default -c 5 → 5 files at a time, 1 connection each
odm url1 url2 url3 url4 url5 url6

# 8 connections split 4 per file → 2 files at a time, resume, logs only
odm -c 8 -sf 4 -x -q -i list.txt

# throttle to 5 MiB/s aggregate and verify sha256 after completion
odm --limit-rate 5M --checksum sha256:9f86... https://files.test.xyz/x.iso

# RPC daemon on port 6900 with a shared secret, downloads to ~/Downloads
odm --rpc --rpc-secret hunter2 --rpc-listen-port 6900 -d ~/Downloads</code></pre>
      </section>


      <section id="docs-operations" class="docs-block">
        <h2>Operations playbook</h2>
        <p>Use these patterns when ODM runs unattended in shells, cron, systemd user units, or a GUI wrapper. The core rule is to keep human UI and machine output separate: terminal users get the animated progress renderer, while automation should use quiet mode, logs, session JSONL, or RPC snapshots.</p>
        <div class="docs-cards">
          <article><h3>Batch queues</h3><p>Keep large URL sets in <code>-i urls.txt</code>, use <code>--skip-existing</code> for repeatable jobs, and choose <code>-sf</code> only when each active file deserves multiple connections.</p></article>
          <article><h3>Daemons</h3><p>Start with <code>odm --rpc -d ~/Downloads</code>, bind loopback by default, and use <code>odm.tellActive</code>, <code>tellWaiting</code>, and <code>tellStopped</code> for dashboards.</p></article>
          <article><h3>Slow networks</h3><p>Prefer <code>--profile smart</code>, set <code>--limit-rate</code> below your link capacity, and let 429 adaptive back-off reduce aggregate pressure automatically.</p></article>
          <article><h3>Reliable archives</h3><p>Pair <code>-x</code> with <code>--checksum-url</code> or Metalink metadata so both resumed chunks and the final artifact are verified before a job exits 0.</p></article>
        </div>
      </section>

      <section id="docs-security" class="docs-block">
        <h2>Security notes</h2>
        <p>ODM is a downloader, so most hardening choices are about source trust, TLS, auth, and output paths. Leave certificate checking enabled unless you are testing a private endpoint, keep RPC on loopback unless there is a real remote-control requirement, and never expose an unauthenticated daemon to a shared network.</p>
        <ul class="docs-list">
          <li><b>TLS</b> — <code>--check-certificate</code> defaults to true. Disabling it makes man-in-the-middle replacement possible and should be temporary.</li>
          <li><b>RPC auth</b> — when using <code>--rpc-listen-all</code>, also set <code>--rpc-secret</code>; clients pass <code>token:&lt;secret&gt;</code> as the first JSON-RPC param.</li>
          <li><b>Output collisions</b> — use <code>--auto-rename</code> to preserve existing files or <code>--skip-existing</code> to make reruns idempotent.</li>
          <li><b>Untrusted lists</b> — review URL lists and Metalink files before running them; they control what hosts are contacted and which destination names are proposed.</li>
        </ul>
      </section>

      <section id="docs-structure" class="docs-block">
        <h2>Documentation map</h2>
        <p>The web manual is now split by page responsibility instead of living in one oversized component. That makes it easier to keep the landing page visual, the reference page dense, and reusable UI elements isolated.</p>
        <div class="docs-path-grid">
          <article class="docs-path-card"><h3><code>src/App.vue</code></h3><p>Owns the top navigation, hash route selection, and shared footer only.</p></article>
          <article class="docs-path-card"><h3><code>src/pages/HomePage.vue</code></h3><p>Landing-page narrative, quickstart, feature strip, profiles, RPC teaser, and flag summary.</p></article>
          <article class="docs-path-card"><h3><code>src/pages/DocsPage.vue</code></h3><p>Long-form manual with table of contents, scroll spy, CLI details, operations guidance, security notes, and troubleshooting.</p></article>
        </div>
      </section>

      <section id="docs-limits" class="docs-block">
        <h2>Known limitations</h2>
        <ul class="docs-list">
          <li><b>Mid-flight connection reallocation</b> — <code>odm.changeOption</code> with key <code>connections</code> is partially supported: graceful drain works (excess workers finish their current chunk and retire), but dynamic rebalancing of already-dispatched chunks across tasks is a roadmap item.</li>
          <li><b>HTTP/2 / HTTP/3 multiplexing is deliberately not a core feature</b> — odm’s value proposition is opening many independent TCP connections. Against HTTP/2-only servers it falls back to single-stream.</li>
          <li><b>Legacy comma form</b> — <code>"url1,url2,..."</code> still works, but a comma <i>inside</i> a URL (e.g. <code>?ids=1,2,3</code>) is ambiguous; use space-separated positional args or <code>-i</code> for such URLs.</li>
        </ul>
      </section>

      <section id="docs-troubleshooting" class="docs-block">
        <h2>Troubleshooting</h2>
        <details open><summary>Server ignores ranges</summary><p>ODM falls back to one stream for non-rangeable targets and redistributes unused budget to other files when possible.</p></details>
        <details><summary>Downloads slow down after 429</summary><p>That is intentional adaptive back-off. The global cap eases down when a server asks clients to slow down, then restores after a quiet period.</p></details>
        <details><summary>Resume restarts from zero</summary><p>The remote object likely changed, validators did not match, or the saved chunk hashes did not match disk. Restarting is safer than stitching mismatched bytes.</p></details>
        <details><summary>A file I control is slow over HTTPS</summary><p>Check <code>--check-certificate</code> stays true, then rule out the server throttling >~30 connections from one IP — <code>--max-connections</code> defaults to 32, which some CDNs treat as abusive.</p></details>
      </section>
    </div>
  </div>
</main>
</template>
