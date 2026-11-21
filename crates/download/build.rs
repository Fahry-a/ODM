fn main() {
    println!("cargo:rerun-if-changed=c/sha256_evp.c");
    cc::Build::new()
        .file("c/sha256_evp.c")
        .compile("odm_sha256_evp");
}
