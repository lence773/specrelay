fn main() {
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "configure_database",
            "get_database_connection",
            "open_database_configuration",
            "minimize_window",
            "toggle_maximize_window",
            "close_window",
        ]),
    ))
    .expect("failed to build SpecRelay desktop application");
}
