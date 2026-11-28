fn main() {
    // Link the Go download core (static c-archive) into every odm binary.
    // Linux musl builds need a post-link constructor patch (see
    // scripts/patch_go_ctor.py); Darwin calls init_array with argc/argv
    // natively and needs no patch.
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(|p| p.parent())
        .expect("workspace root");
    let gocore = root.join("gocore");

    let os = std::env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    let arch = std::env::var("CARGO_CFG_TARGET_ARCH").unwrap_or_default();
    let env_name = std::env::var("CARGO_CFG_TARGET_ENV").unwrap_or_default();

    let lib = match (os.as_str(), arch.as_str(), env_name.as_str()) {
        ("linux", "x86_64", "musl") => "odmgo_musl",
        ("linux", "x86_64", _) => "odmgo",
        ("linux", "aarch64", "musl") => "odmgo_aarch64",
        ("linux", "arm", "musl") => "odmgo_armv7",
        ("linux", "x86", "musl") => "odmgo_i686",
        ("macos", "aarch64", _) => "odmgo_darwin_arm64",
        ("macos", "x86_64", _) => "odmgo_darwin_amd64",
        _ => "odmgo",
    };

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
    println!("cargo:rerun-if-changed={}", gocore.join(format!("lib{lib}.a")).display());
}
