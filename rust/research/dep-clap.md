# clap 4.6.9 — research notes for the ODM Go→Rust port

## Entry API: builder vs derive

Both exist and are supported.

- Builder (procedural): `clap::Command::new("odm").arg(...)` → `.get_matches()` or `.try_get_matches_from([...])`.
- Derive (declarative): `#[derive(Parser)]` on a struct with doc-comment help text, then `Cli::parse()`; requires the optional `derive` feature (`cargo add clap --features derive`).

clap recommends derive for new code; the builder is the natural fit when porting a fixed flag set. A `#[derive(Parser)]` struct of `url: Vec<String>`, `conns: u64` etc. maps 1:1 to the Go flags. There is also a one-liner `arg!(--flag <V>)` macro.

## Declaring the ODM fixed flag set (builder form)

```rust
let cmd = clap::Command::new("odm")
    // positionals: no .short/.long, definition order = index (1-based)
    .arg(clap::Arg::new("urls").num_args(1..).required(true).value_name("URL"))
    // -c N: ODM's flag is connections/-c (internal/config/config.go:345)
    .arg(clap::Arg::new("connections").short('c').long("connections")
         .value_name("N").default_value("8"))
    // --session-log FILE: long-only
    .arg(clap::Arg::new("session_log").long("session-log").value_name("FILE"))
    // -H k:v, repeatable
    .arg(clap::Arg::new("header").short('H').action(clap::ArgAction::Append))
    // env-style: Arg::env("ODM_X") — feature "env" is optional, not default
    .arg(clap::Arg::new("log_level").long("log").env("ODM_LOG_LEVEL"));
```

- Actions: default is `ArgAction::Set`. Use `SetTrue` for bool flags, `SetFalse`, `Count` (for `-v` levels), `Append` (repeatable `-H`; also multiple values via `num_args`).
- Auto flags: `-h/--help`, `-V/--version` when `Command::version()` is set; `disable_help_flag`/`disable_version_flag` to customize.
- Help text: `Arg::help("...")` / `.long_help(...)`; derive uses the doc comments above fields/variants.
- Validation groups: `ArgGroup` for "one of" sets; `requires`/`conflicts_with` between args.

## Reading values (ArgMatches)

- Options: `matches.get_one::<T>("connections") -> Option<&T>`; flags: `matches.get_flag("verbose")`; counters: `matches.get_count("debug")`.
- Repeatable/append: `matches.get_many::<String>("header")` → iterator of `&String`.
- Positionals: same API — `matches.get_many("urls")` or `get_one("urls")`.
- Distinguishing default vs user-provided: `matches.value_source("id") == Some(ValueSource::DefaultValue)`. Note `contains_id()` is true even for defaults; `Arg::default_value("8")`, `default_value_if(...)`, `required_unless_present(...)`.
- Defaults for typed flags: `value_parser` + `default_value`; derive uses `#[arg(default_value_t = 8)]`.

## Typed values and "4M"-style sizes

- `Arg::value_parser(clap::value_parser!(u64))` — built-ins: `String`, `PathBuf`, `OsString` (`ValueParser::string()/path_buf()/os_string()`), ints via `value_parser!(u64)`, ranges via `value_parser!(u64).range(1..)`; static lists via `value_parser(["a", "b"])` or `PossibleValuesParser`. Derive picks `value_parser!(T)` per field type automatically.
- Suffix parsing (`4M`, `8K`): write `fn parse_size(s: &str) -> Result<u64, String>` and use `Arg::value_parser(clap::builder::ValueParser::new(parse_size))` (derive: `#[arg(value_parser = parse_size)]`). Any `Fn(&str) -> Result<T, E>` is accepted as a `TypedValueParser`.

## Errors and exit codes

- `cmd.try_get_matches_from(["odm", ...]) -> Result<ArgMatches, clap::Error>`; `cmd.get_matches()` prints + exits.
- `err.kind() -> clap::error::ErrorKind`: `UnknownArgument`, `MissingRequiredArgument`, `ArgumentConflict`, `ValueValidation`, `DisplayHelp`, `DisplayVersion`, …
- `err.exit()` — stderr + `std::process::exit(2)` for usage errors; stdout + exit 0 for help/version. (`USAGE_CODE=2`, `SUCCESS_CODE=0`; `err.exit_code()` returns 2 or 0; `err.print()` prints without exiting.)
- Suggestions: default `suggestions` feature prints "Did you mean '--myoption'?" tips for typos.
- Custom errors: `Cli::command().error(ErrorKind::X, "msg").exit()` or `.format(cmd).print()/.exit()`.

## Breaking changes across majors (v3 → v4)

- Renames: `App` → `Command`, `SubCommand` → `Subcommand`; `AppSettings`/`ArgSettings` enums → builder get/setters on `Command`/`Arg`.
- v4: `ArgAction` replaces implicit occurrence behavior; typed `ValueParser`/`TypedValueParser` parsing introduced; `Arg::num_args(range)` collapsed `takes_value`/`number_of_values`/`multiple_values`/`min/max_values`; `Id` newtype; `ArgMatches::ids()` iterator.
- clap policy: willing to break between majors (6–9 month cadence), `debug_assert`-based CLI self-checks via `Cli::command().debug_assert()` / `cmd.debug_assert()`.
- Migration guide: https://github.com/clap-rs/clap/blob/v4.6.9/CHANGELOG.md ("Clearing Out Deprecations", "Migrating" section).

## Sources

docs.rs clap 4.6.9: crate root, `builder::{Arg, ArgAction, ValueParser, Command, Error, ArgMatches}`; `_derive/_tutorial`, `_features`, `error::{Error, ErrorKind}`; README at github.com/clap-rs/clap; CHANGELOG 4.0.0 (v3→v4 breaking changes); clap_builder source `error/mod.rs` (exit codes), `util/mod.rs` (`USAGE_CODE=2`, `SUCCESS_CODE=0`); ODM `internal/config/config.go` (flag names: `connections/-c`, `--session-log`, `--header/-H`).
