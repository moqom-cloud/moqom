//! Regenerates the protobuf bindings, on request only.
//!
//! The generated file is checked in and this does nothing unless
//! `MOQOM_REGENERATE_PROTO` is set, which is how `tools/generate-proto.sh` invokes
//! it. A published SDK that generated at build time would require every consumer to
//! have `protoc` installed at a compatible version, which is a poor thing to demand
//! for code that changes when the API does and not otherwise.

fn main() {
    println!("cargo:rerun-if-env-changed=MOQOM_REGENERATE_PROTO");

    if std::env::var_os("MOQOM_REGENERATE_PROTO").is_none() {
        return;
    }

    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../proto")
        .canonicalize()
        .expect("the proto directory is where it is expected");
    let out = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src/generated");
    std::fs::create_dir_all(&out).expect("creating the generated directory");

    let files = [
        root.join("moqom/admin/v1/common.proto"),
        root.join("moqom/admin/v1/rooms.proto"),
        root.join("moqom/admin/v1/moderation.proto"),
        root.join("moqom/admin/v1/tokens.proto"),
        root.join("moqom/admin/v1/events.proto"),
    ];
    for file in &files {
        println!("cargo:rerun-if-changed={}", file.display());
    }

    tonic_prost_build::configure()
        .build_server(true)
        .build_client(true)
        .out_dir(&out)
        .compile_protos(&files, &[root])
        .expect("compiling the admin protos");
}
