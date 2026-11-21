//! Rust port of `internal/ui/clock.go` — the indeterminate-bar bounce seam.

/// `bouncePosition` maps a frame index to a back-and-forth slot in `[0,width)`.
/// Triangle wave: rise 0→width-1 over `width` frames, then fall over `width-2`
/// frames; period `2*(width-1)` so neither endpoint repeats back-to-back.
pub fn bounce_position(frame: i64, width: usize) -> usize {
    if width <= 1 {
        return 0;
    }
    let period = 2 * (width - 1) as i64;
    let mut step = frame % period;
    if step < 0 {
        step += period;
    }
    if step >= width as i64 {
        step = period - step; // descending half
    }
    step.clamp(0, (width - 1) as i64) as usize
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn triangle_wave_visits_each_endpoint_once() {
        let w = 4;
        let got: Vec<usize> = (0..7).map(|i| bounce_position(i, w)).collect();
        assert_eq!(got, vec![0, 1, 2, 3, 2, 1, 0]);
    }

    #[test]
    fn single_cell_bar_is_pinned() {
        assert_eq!(bounce_position(0, 1), 0);
        assert_eq!(bounce_position(99, 1), 0);
    }

    #[test]
    fn negative_frames_wrap() {
        assert_eq!(bounce_position(-1, 3), 1);
        assert_eq!(bounce_position(-2, 3), 2);
    }
}
