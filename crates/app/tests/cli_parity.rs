//! Regression tests for parity with the Go reference CLI text/behaviour.
//! These exercise the real binary so clap's own error text is covered too.

use std::process::Command;

fn odm() -> Command {
    Command::new(env!("CARGO_BIN_EXE_odm"))
}

fn stderr(args: &[&str]) -> String {
    let out = odm().args(args).output().expect("run odm");
    String::from_utf8_lossy(&out.stderr).into_owned()
}

#[test]
fn help_text_matches_go_reference() {
    let out = odm().arg("--help").output().expect("run odm --help");
    let s = String::from_utf8_lossy(&out.stdout);
    assert!(s.contains("  odm [OPTIONS] <URL> [URL ...]"), "usage rows lost their indent:\n{s}");
    assert!(s.contains("(mutually exclusive with --skip-existing)"), "auto-rename note missing");
    assert!(s.contains("(sha256sum-style file)"), "checksum-url note missing");
    assert!(s.contains("chunks rotate (repeatable)"), "mirror note missing");
    assert!(s.contains("(stacked on global)"), "limit-rate-per-task note missing");
    assert!(
        s.contains("URLs with a literal comma must use space-separated form or -i. For batches >10\nURLs, -i is recommended."),
        "comma/batch advice missing"
    );
    assert!(
        s.contains("Exit codes: 0 ok | 1 bad args | 2 network (all retries exhausted) | 3 partial |\n            4 cancelled."),
        "exit-code legend differs from Go"
    );
}

#[test]
fn repeated_bool_flags_use_last_value() {
    // pflag lets a bool flag appear more than once (last value wins).
    let e = stderr(&["--quiet=false", "-y", "--dry-run", "-q"]);
    assert!(!e.contains("cannot be used multiple times"), "clap rejected a repeated flag: {e}");
    assert!(e.contains("no URLs provided"), "unexpected error after repeated flags: {e}");
}

#[test]
fn load_cookie_error_keeps_go_os_error_text() {
    let e = stderr(&["--load-cookies", "/no/such/file"]);
    assert!(
        e.contains("load-cookies \"/no/such/file\": open /no/such/file: no such file or directory"),
        "load-cookie error text differs from Go: {e}"
    );
}

#[test]
fn unknown_flag_error_matches_go_reference() {
    let e = stderr(&["--bogus"]);
    assert_eq!(e, "odm: unknown flag: --bogus (run 'odm -h' for usage)\n");
}

#[test]
fn bad_integer_error_matches_go_strconv_text() {
    assert_eq!(
        stderr(&["-c", "abc"]),
        "odm: invalid argument \"abc\" for \"-c, --connections\" flag: strconv.ParseInt: parsing \"abc\": invalid syntax (run 'odm -h' for usage)\n"
    );
    let huge = "999999999999999999999999";
    assert!(stderr(&["-c", huge]).contains(&format!(
        "strconv.ParseInt: parsing \"{huge}\": value out of range"
    )));
}
