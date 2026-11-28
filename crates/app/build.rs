use std::process::Command;

fn main() {
    // Link the Go download core (static c-archive) into every odm binary.
    // The archives are gitignored, so build the one for this target on demand:
    // a fresh CI checkout (and release cross-build) must link without a
    // hand-run Go step. musl builds need a post-link constructor patch;
    // Darwin calls init_array with argc/argv natively and needs no patch.
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(|p| p.parent())
        .expect("workspace root");
    let gocore = root.join("gocore");

    let os = std::env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    let arch = std::env::var("CARGO_CFG_TARGET_ARCH").unwrap_or_default();
    let env_name = std::env::var("CARGO_CFG_TARGET_ENV").unwrap_or_default();

    let (lib, triple) = match (os.as_str(), arch.as_str(), env_name.as_str()) {
        ("linux", "x86_64", "musl") => ("odmgo_musl", "x86_64-unknown-linux-musl"),
        ("linux", "x86_64", _) => ("odmgo", "x86_64-unknown-linux-gnu"),
        ("linux", "aarch64", "musl") => ("odmgo_aarch64", "aarch64-unknown-linux-musl"),
        ("linux", "arm", "musl") => ("odmgo_armv7", "armv7-unknown-linux-musleabihf"),
        ("linux", "x86", "musl") => ("odmgo_i686", "i686-unknown-linux-musl"),
        ("macos", "aarch64", _) => ("odmgo_darwin_arm64", "aarch64-apple-darwin"),
        ("macos", "x86_64", _) => ("odmgo_darwin_amd64", "x86_64-apple-darwin"),
        _ => panic!("unsupported odm target: {os}/{arch}/{env_name}"),
    };

    let archive = gocore.join(format!("lib{lib}.a"));
    if !archive.exists() || !gocore.join(format!("lib{lib}.h")).exists() {
        let status = Command::new("bash")
            .arg(root.join("scripts/build-go-core.sh"))
            .arg(triple)
            .status()
            .expect("run scripts/build-go-core.sh (is bash/go on PATH?)");
        assert!(status.success(), "failed to build Go core for {triple}");
    }

    println!("cargo:rustc-link-search=native={}", gocore.display());
    println!("cargo:rustc-link-lib=static={lib}");
    if os == "macos" {
        println!("cargo:rustc-link-search=native={}", gocore.join("macos-stubs").display());
        println!("cargo:rustc-link-lib=Security");
        println!("cargo:rustc-link-lib=CoreFoundation");
    }
    if os == "linux" {
        if env_name != "musl" {
            println!("cargo:rustc-link-lib=pthread");
            println!("cargo:rustc-link-lib=dl");
        }
        println!("cargo:rustc-link-lib=m");
        if arch == "x86" || arch == "arm" {
            println!("cargo:rustc-link-lib=atomic");
            println!("cargo:rustc-link-lib=gcc");
            println!("cargo:rustc-link-arg=-Wl,--wrap=clock_gettime");
        }
    }
    println!("cargo:rerun-if-changed={}", gocore.join("cmd/odmbridge").display());
    println!("cargo:rerun-if-changed={}", gocore.join("internal").display());
    println!("cargo:rerun-if-changed={}", archive.display());
}
