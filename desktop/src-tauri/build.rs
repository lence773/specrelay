fn main() {
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "configure_database",
            "minimize_window",
            "toggle_maximize_window",
            "close_window",
        ]),
    ))
    .expect("failed to build SpecRelay desktop application");
}
