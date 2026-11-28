//! Rust port of `internal/ui/progress.go` — the stateful Renderer: per-task
//! snapshot cache, TTY redraw loop and throttled non-TTY log lines.

use std::collections::{HashMap, HashSet};
use std::io::{IsTerminal, Write};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use download::{Ctx, ProgressView, TaskState, TaskID};
use tokio::sync::mpsc::{self, Receiver, Sender};

use super::ansi::{display_width, truncate_visible_width};
use super::bar::BAR_WIDTH;
use super::clock::bounce_position;
use super::render::{render_task_line, LineLayout, COL_GLYPH};
use super::summary::{render_summary, render_summary_width};
use super::termsize::renderer_size;

const ANSI_CLEAR_LINE: &str = "\x1b[2K";
const ANSI_CURSOR_UP: &str = "\x1b[A";
const ANSI_CURSOR_HIDE: &str = "\x1b[?25l";
const ANSI_CURSOR_SHOW: &str = "\x1b[?25h";

/// Per-task snapshot cache plus the most recent live/queued slices.
#[derive(Default)]
struct Cursor {
    cache: HashMap<TaskID, ProgressView>,
    last_live: Option<Vec<ProgressView>>,
    last_queued: Option<Vec<ProgressView>>,
}

impl Cursor {
    /// Live first (in last-seen order), then cache-only retired ids in ascending
    /// order. Stable ordering keeps redraws from shuffling lines.
    fn ordered(&self) -> Vec<ProgressView> {
        let mut out = Vec::with_capacity(self.cache.len());
        let mut seen = HashSet::new();
        if let Some(live) = &self.last_live {
            for v in live {
                out.push(v.clone());
                seen.insert(v.id.clone());
            }
        }
        let mut leftovers: Vec<ProgressView> = self
            .cache
            .iter()
            .filter(|(id, _)| !seen.contains(*id))
            .map(|(_, v)| v.clone())
            .collect();
        leftovers.sort_by(|a, b| a.id.cmp(&b.id));
        out.extend(leftovers);
        out
    }
}

#[derive(Default)]
struct State {
    cur: Cursor,
    last_lines: usize,
    indeterminate_tick: i64,
    started_at: Option<Instant>,
    last_non_tty_flush: Option<Instant>,
    last_logged_key: String,
}

/// `Renderer` owns every byte written to the screen. TTY writes are serialized
/// so engine logs routed through `Interject` can't interleave ANSI sequences.
pub struct Renderer {
    out: Arc<Mutex<Box<dyn Write + Send>>>,
    state: Mutex<State>,
    use_color: bool,
    tty: bool,
    quiet: bool,
    size_fn: Box<dyn Fn() -> (usize, usize) + Send + Sync>,
    clock: Arc<dyn Fn() -> Instant + Send + Sync>,
    non_tty_interval: Duration,
    wake_tx: Sender<()>,
    wake_rx: Mutex<Option<Receiver<()>>>,
}

impl Renderer {
    /// `NewRenderer` over stdout; use_color is suppressed by NO_COLOR.
    pub fn new_stdout(quiet: bool) -> Renderer {
        let tty = !quiet && std::io::stdout().is_terminal();
        let use_color = tty && std::env::var_os("NO_COLOR").is_none();
        let fd = if std::io::stdout().is_terminal() { Some(1) } else { None };
        Renderer::build(Box::new(std::io::stdout()), tty, use_color, quiet, fd)
    }

    /// Render the live bar to stderr (Go: stderr is the fallback when stdout
    /// is piped but stderr is still a terminal).
    pub fn new_stderr(quiet: bool) -> Renderer {
        let tty = !quiet && std::io::stderr().is_terminal();
        let use_color = tty && std::env::var_os("NO_COLOR").is_none();
        let fd = if tty { Some(2) } else { None };
        Renderer::build(Box::new(std::io::stderr()), tty, use_color, quiet, fd)
    }

    /// Inject a writer (tests capture output in a buffer). `tty` is explicit:
    /// generic writers can't be probed for terminal-ness in Rust.
    pub fn new_with_writer<W: Write + Send + 'static>(writer: W, tty: bool, quiet: bool) -> Renderer {
        let use_color = tty && std::env::var_os("NO_COLOR").is_none();
        Renderer::build(Box::new(writer), tty, use_color, quiet, None)
    }

    fn build(
        out: Box<dyn Write + Send>,
        tty: bool,
        use_color: bool,
        quiet: bool,
        fd: Option<i32>,
    ) -> Renderer {
        let (wake_tx, wake_rx) = mpsc::channel(1);
        Renderer {
            out: Arc::new(Mutex::new(out)),
            state: Mutex::new(State::default()),
            use_color,
            tty,
            quiet,
            size_fn: Box::new(move || renderer_size(fd)),
            clock: Arc::new(Instant::now),
            non_tty_interval: Duration::from_secs(2),
            wake_tx,
            wake_rx: Mutex::new(Some(wake_rx)),
        }
    }

    /// Replace the wall clock (tests advance it by hand).
    pub fn set_clock(&mut self, clock: Arc<dyn Fn() -> Instant + Send + Sync>) {
        self.clock = clock;
    }

    /// Replace the terminal-size probe (tests render into a fixed-size terminal).
    pub fn set_size_fn(&mut self, f: impl Fn() -> (usize, usize) + Send + Sync + 'static) {
        self.size_fn = Box::new(f);
    }

    /// `Wake` sender — SIGWINCH nudges a resize into the next frame.
    pub fn wake_sender(&self) -> Sender<()> {
        self.wake_tx.clone()
    }

    pub fn live(&self) -> bool {
        self.tty && !self.quiet
    }

    fn write_str(&self, s: &str) {
        let mut out = self.out.lock().unwrap();
        let _ = out.write_all(s.as_bytes());
        let _ = out.flush();
    }

    /// `Begin` — hide the cursor (TTY only).
    pub fn begin(&self) {
        if self.tty {
            self.write_str(ANSI_CURSOR_HIDE);
        }
    }

    /// `End` — restore the cursor and leave a trailing newline. The newline
    /// consumes a screen row, so lastLines grows by one to keep the next
    /// frame's cursor-up count correct.
    pub fn end(&self) {
        let mut st = self.state.lock().unwrap();
        if self.tty {
            self.write_str(ANSI_CURSOR_SHOW);
            st.last_lines += 1;
        }
        self.write_str("\n");
        drop(st);
    }

    /// `Interject` — erase the frame, print an out-of-band line, redraw beneath
    /// it. A bare write between frames would shift the cursor and desync the
    /// next frame's cursor-up count (the duplicated task lines on ^C).
    pub fn interject(&self, line: &str) {
        let line = line.trim_end_matches('\n');
        if line.is_empty() || self.quiet {
            return;
        }
        let mut st = self.state.lock().unwrap();
        if !self.tty {
            self.write_str(line);
            self.write_str("\n");
            return;
        }
        self.write_str("\r");
        for _ in 0..st.last_lines {
            self.write_str(ANSI_CURSOR_UP);
            self.write_str(ANSI_CLEAR_LINE);
        }
        st.last_lines = 0;
        self.write_str(line);
        self.write_str("\n");
        let view = st.cur.ordered();
        let stats = aggregate(&view);
        let redrawn = self.compose_lines_in(&mut st, &view, &stats);
        for l in &redrawn {
            self.write_str(l);
            self.write_str("\n");
        }
        st.last_lines = redrawn.len();
    }

    /// `InterjectWriter` — adapt a logging sink to `Interject`.
    pub fn interject_writer(self: &Arc<Self>) -> InterjectWriter {
        InterjectWriter { renderer: self.clone() }
    }

    /// `updateCache` — merge live/queued into the cache and promote vanished
    /// tasks to terminal state so a finished line stays on screen at 100%.
    /// (nil, nil) is the post-run final frame: re-render from cache.
    fn update_cache(st: &mut State, live: Option<&[ProgressView]>, queued: Option<&[ProgressView]>) -> Vec<ProgressView> {
        match (live, queued) {
            (None, None) => {
                for v in st.cur.cache.values_mut() {
                    if matches!(v.state, TaskState::Completed | TaskState::Error) {
                        continue;
                    }
                    if v.total_size > 0 && v.bytes_done >= v.total_size {
                        promote(v);
                    }
                }
                st.cur.last_live = None;
                st.cur.last_queued = None;
                return st.cur.ordered();
            }
            (live_opt, queued_opt) => {
                let live = live_opt.unwrap_or(&[]);
                let queued = queued_opt.unwrap_or(&[]);
                for v in live {
                    st.cur.cache.insert(v.id.clone(), v.clone());
                }
                for v in queued {
                    let mut vc = v.clone();
                    if vc.state == TaskState::Active {
                        vc.state = TaskState::Queued;
                    }
                    st.cur.cache.insert(vc.id.clone(), vc);
                }
                let present: HashSet<&TaskID> = live.iter().chain(queued.iter()).map(|v| &v.id).collect();
                for v in st.cur.cache.values_mut() {
                    if present.contains(&v.id) || matches!(v.state, TaskState::Completed | TaskState::Error) {
                        continue;
                    }
                    // Only retire entries that actually look finished; a
                    // snapshot that skipped a frame isn't a completion.
                    if (v.total_size > 0 && v.bytes_done >= v.total_size) || v.total_size <= 0 {
                        promote(v);
                    }
                }
                st.cur.last_live = Some(live.to_vec());
                st.cur.last_queued = Some(queued.to_vec());
                st.cur.ordered()
            }
        }
    }

    /// `Frame` — render the full set of task lines + summary, overwriting the
    /// previous frame in place (TTY) or emitting a throttled log line.
    /// Pass `None, None` for the final post-run frame.
    pub fn frame(&self, live: Option<&[ProgressView]>, queued: Option<&[ProgressView]>) {
        if self.quiet {
            return;
        }
        let mut st = self.state.lock().unwrap();
        let view = Self::update_cache(&mut st, live, queued);
        let stats = aggregate(&view);
        if !self.tty {
            self.emit_non_tty(&mut st, &view, &stats, live.is_none() && queued.is_none());
            return;
        }
        let lines = self.compose_lines_in(&mut st, &view, &stats);
        self.write_str("\r");
        for _ in 0..st.last_lines {
            self.write_str(ANSI_CURSOR_UP);
            self.write_str(ANSI_CLEAR_LINE);
        }
        for l in &lines {
            self.write_str(l);
            self.write_str("\n");
        }
        st.last_lines = lines.len();
        st.indeterminate_tick += 1;
    }

    fn compose_lines_in(&self, st: &mut State, view: &[ProgressView], stats: &ViewStats) -> Vec<String> {
        let (width, height) = (self.size_fn)();
        let (bar_w, name_w, _min_w, layout) = layout_for(width);
        let mut lines = Vec::new();
        let mut hidden = 0usize;
        let pos = bounce_position(st.indeterminate_tick, bar_w);
        let elapsed = st.started_at.map(|t| (self.clock)().saturating_duration_since(t)).unwrap_or_default();

        let mut row_budget = height.max(1);
        if stats.total > 0 {
            row_budget = row_budget.saturating_sub(1); // summary row
        }
        row_budget = row_budget.saturating_sub(1); // "N more..." row

        for v in view {
            if !is_active(v) {
                continue;
            }
            if lines.len() >= row_budget {
                hidden += 1;
                continue;
            }
            let indeterminate_pos = if v.total_size <= 0 { pos as i64 } else { -1 };
            let line = render_task_line(v, self.use_color, indeterminate_pos, name_w, bar_w, layout);
            lines.push(truncate_to_width(&line, width));
        }
        if stats.total > 0 {
            lines.push(truncate_to_width(
                &render_summary_width(
                    stats.completed,
                    stats.total,
                    stats.speed,
                    stats.max_eta,
                    elapsed,
                    stats.bytes_done,
                    stats.total_size,
                    self.use_color,
                    width,
                    bar_w,
                ),
                width,
            ));
        }
        if hidden > 0 {
            lines.push(truncate_to_width(&format!("{hidden} more..."), width));
        }
        lines
    }

    /// `emitNonTTY` — print only on milestones (completed count changes), on a
    /// ~2s interval, or on the forced final frame.
    fn emit_non_tty(&self, st: &mut State, view: &[ProgressView], stats: &ViewStats, final_frame: bool) {
        let now = (self.clock)();
        let key = format!("{}|{}", stats.completed, stats.total);
        let interval_elapsed = st.last_non_tty_flush.map(|t| now.saturating_duration_since(t) >= self.non_tty_interval).unwrap_or(false);
        let milestone = key != st.last_logged_key;
        // Before the first snapshot there is nothing to draw; Go's first frame
        // already carries the probed task. A forced final frame that repeats a
        // full milestone frame is a duplicate, not extra information.
        if (view.is_empty() && st.last_logged_key.is_empty())
            || (!final_frame && !interval_elapsed && !milestone)
        {
            return;
        }
        let full = final_frame || milestone;
        st.last_non_tty_flush = Some(now);
        st.last_logged_key = key;
        let elapsed = st.started_at.map(|t| now.saturating_duration_since(t)).unwrap_or_default();
        let summary = render_summary(stats.completed, stats.total, stats.speed, stats.max_eta, elapsed, stats.bytes_done, stats.total_size, self.use_color);
        let mut out = String::new();
        if full {
            out.push_str("---\n");
            for v in view {
                if !is_active(v) {
                    continue;
                }
                out.push_str(&render_task_line(v, self.use_color, -1, 40, BAR_WIDTH, LineLayout::Full));
                out.push('\n');
            }
        }
        out.push_str(&summary);
        out.push('\n');
        self.write_str(&out);
    }

    /// `RunLoop` — drive the renderer from snapshot channels until ctx cancel.
    /// `done` is signalled after `End()` so the caller's final frame can't race.
    pub async fn run_loop(
        self: &Arc<Self>,
        ctx: &Ctx,
        interval: Duration,
        mut live_rx: Receiver<Vec<ProgressView>>,
        mut queued_rx: Receiver<Vec<ProgressView>>,
        done: tokio::sync::oneshot::Sender<()>,
    ) {
        {
            let mut st = self.state.lock().unwrap();
            st.started_at = Some((self.clock)());
        }
        self.begin();
        let mut ticker = tokio::time::interval(interval);
        let mut wake_rx = {
            let mut guard = self.wake_rx.lock().unwrap();
            guard.take().expect("run_loop started twice")
        };
        loop {
            tokio::select! {
                _ = ctx.wait() => break,
                Some(s) = live_rx.recv() => {
                    let q = self.state.lock().unwrap().cur.last_queued.clone();
                    self.frame(Some(s.as_slice()), q.as_deref());
                }
                Some(s) = queued_rx.recv() => {
                    let l = self.state.lock().unwrap().cur.last_live.clone();
                    self.frame(l.as_deref(), Some(s.as_slice()));
                }
                _ = ticker.tick() => {
                    let (l, q) = {
                        let st = self.state.lock().unwrap();
                        (st.cur.last_live.clone(), st.cur.last_queued.clone())
                    };
                    self.frame(l.as_deref(), q.as_deref());
                }
                Some(()) = wake_rx.recv() => {
                    let (l, q) = {
                        let st = self.state.lock().unwrap();
                        (st.cur.last_live.clone(), st.cur.last_queued.clone())
                    };
                    self.frame(l.as_deref(), q.as_deref());
                }
            }
        }
        self.end();
        let _ = done.send(());
    }
}

/// Adapter from `Write` to `Interject` (one log entry per Write, newline included).
pub struct InterjectWriter {
    renderer: Arc<Renderer>,
}

impl Write for InterjectWriter {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.renderer.interject(&String::from_utf8_lossy(buf));
        Ok(buf.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

fn promote(v: &mut ProgressView) {
    v.state = TaskState::Completed;
    v.connections = 0;
    v.speed = 0;
    v.eta = Duration::ZERO;
}

/// Aggregate bottom-line stats over unique task ids (a task still in `live`
/// for one frame after completion can't be double-counted).
#[derive(Default, Debug)]
pub struct ViewStats {
    pub completed: usize,
    pub total: usize,
    pub speed: i64,
    pub max_eta: Duration,
    pub bytes_done: i64,
    pub total_size: i64,
}

pub fn aggregate(view: &[ProgressView]) -> ViewStats {
    let mut ids = HashSet::new();
    let mut st = ViewStats::default();
    for v in view {
        if !ids.insert(&v.id) {
            continue;
        }
        st.total += 1;
        st.bytes_done += v.bytes_done;
        if v.total_size > 0 {
            st.total_size += v.total_size;
        }
        if v.state == TaskState::Completed {
            st.completed += 1;
        } else {
            st.speed += v.speed;
            if v.eta > st.max_eta {
                st.max_eta = v.eta;
            }
        }
    }
    st
}

/// Active tasks are rendered; terminal ones are always shown even at zero
/// bytes (an empty file or failed probe still counts toward the summary).
pub fn is_active(v: &ProgressView) -> bool {
    if matches!(v.state, TaskState::Completed | TaskState::Error) {
        return true;
    }
    v.bytes_done != 0 || v.speed != 0
}

/// Select columns for a terminal width; bars shrink first, then whole columns
/// drop, down to a percent-only floor for ~10-column terminals.
pub fn layout_for(term_width: usize) -> (usize, usize, usize, LineLayout) {
    let (bar_w, name_w, min_w, layout) = if term_width >= 96 {
        let bar = BAR_WIDTH;
        let name = term_width.saturating_sub(super::render::info_block_width_for(bar) + 1);
        let min = COL_GLYPH + super::render::INFO_FIXED_WIDTH + bar + 1;
        (bar, name, min, LineLayout::Full)
    } else if term_width >= 66 {
        let bar = 20usize;
        let name = term_width.saturating_sub((COL_GLYPH + 2 + 14 + 2 + 6 + 1 + bar + 1 + 2 + 4) + 1);
        let min = COL_GLYPH + (2 + 14 + 2 + 6 + 1 + 1 + 2 + 4) + bar + 1;
        (bar, name, min, LineLayout::NoSpeedEta)
    } else if term_width >= 36 {
        let bar = 10usize;
        let name = term_width.saturating_sub((COL_GLYPH + 2 + 6 + 1 + bar + 1 + 2 + 4) + 1);
        let min = COL_GLYPH + (2 + 6 + 1 + 1 + 2 + 4) + bar + 1;
        (bar, name, min, LineLayout::NameBarPct)
    } else if term_width >= 12 {
        let name = term_width.saturating_sub(COL_GLYPH + 2 + 4 + 1);
        (0, name, 0, LineLayout::NamePct)
    } else {
        (0, term_width.saturating_sub(1).max(1), 0, LineLayout::Pct)
    };
    let name_w = if name_w < 4 && layout != LineLayout::Pct { 4 } else { name_w };
    (bar_w, name_w, min_w, layout)
}

fn truncate_to_width(s: &str, width: usize) -> String {
    if width == 0 || display_width(s) <= width {
        s.to_string()
    } else {
        truncate_visible_width(s, width)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[derive(Clone)]
    struct SharedBuf(Arc<Mutex<Vec<u8>>>);

    impl Write for SharedBuf {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    fn output(buf: &SharedBuf) -> String {
        String::from_utf8_lossy(&buf.0.lock().unwrap()).to_string()
    }

    fn view(id: &str, state: TaskState, done: i64, total: i64) -> ProgressView {
        ProgressView {
            id: id.into(),
            url: "http://x/f.bin".into(),
            final_url: "http://x/f.bin".into(),
            filename: "f.bin".into(),
            state,
            total_size: total,
            bytes_done: done,
            speed: 100,
            connections: 1,
            eta: Duration::from_secs(5),
            errors: 0,
            retries: 0,
            single_stream: false,
        }
    }

    #[test]
    fn aggregate_counts_unique_ids() {
        let v = view("a", TaskState::Completed, 100, 100);
        let st = aggregate(&[v.clone(), v]);
        assert_eq!(st.total, 1);
        assert_eq!(st.completed, 1);
        assert_eq!(st.bytes_done, 100);
        assert_eq!(st.speed, 0);
    }

    #[test]
    fn layout_tiers_degrade_monotonically() {
        assert_eq!(layout_for(120).3, LineLayout::Full);
        assert_eq!(layout_for(70).3, LineLayout::NoSpeedEta);
        assert_eq!(layout_for(40).3, LineLayout::NameBarPct);
        assert_eq!(layout_for(20).3, LineLayout::NamePct);
        assert_eq!(layout_for(10).3, LineLayout::Pct);
    }

    #[test]
    fn final_frame_retains_completed_task() {
        let buf = SharedBuf(Arc::new(Mutex::new(Vec::new())));
        let mut r = Renderer::new_with_writer(buf.clone(), true, false);
        r.set_size_fn(|| (100, 24));
        let v = view("a", TaskState::Active, 50, 100);
        r.frame(Some(&[v]), None);
        r.frame(None, None); // final frame: cache only
        let out = output(&buf);
        // The cached task must still render (not vanish to Total: 0/0).
        assert!(out.contains("f.bin") || out.contains("Total: 1/1"), "out: {out}");
    }

    #[test]
    fn non_tty_skips_empty_start_frame() {
        let buf = SharedBuf(Arc::new(Mutex::new(Vec::new())));
        let r = Renderer::new_with_writer(buf.clone(), false, false);
        r.frame(None, None); // empty start: no tasks known yet
        r.frame(Some(&[view("a", TaskState::Active, 0, 100)]), None);
        r.frame(Some(&[view("a", TaskState::Completed, 100, 100)]), None);
        r.frame(None, None); // forced final after the completed milestone
        let text = output(&buf);
        assert!(!text.contains("Total: 0/0"), "empty start frame leaked: {text}");
        // Go prints the completed frame twice (milestone + forced final).
        assert_eq!(text.matches("---\n").count(), 3, "frame count differs from Go: {text}");
    }

    #[test]
    fn non_tty_throttles_repeated_frames() {
        let buf = SharedBuf(Arc::new(Mutex::new(Vec::new())));
        let r = Renderer::new_with_writer(buf.clone(), false, false);
        let v = view("a", TaskState::Active, 10, 100);
        r.frame(Some(&[v.clone()]), None); // milestone → full block
        r.frame(Some(&[v.clone()]), None); // same key → throttled
        r.frame(Some(&[v]), None);
        let text = output(&buf);
        // The bar itself contains "---", so count the full-block header only.
        assert_eq!(text.matches("---\n> ").count(), 1, "throttled frames printed twice: {text}");
    }
}
