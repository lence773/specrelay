// A Tauri desktop app must be linked as a GUI process on Windows. Without
// this, Windows allocates a visible console for SpecRelay.exe even though it
// is a graphical app. Keep consoles enabled for debug builds so `tauri dev`
// still has useful diagnostics.
#![cfg_attr(
    all(target_os = "windows", not(debug_assertions)),
    windows_subsystem = "windows"
)]

use std::{
    env,
    ffi::OsString,
    fs,
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex,
    },
    thread,
    time::Duration,
};

use percent_encoding::percent_decode_str;
use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Manager, RunEvent, WebviewUrl, WebviewWindow, WebviewWindowBuilder};
use tauri_plugin_shell::{
    process::{CommandChild, CommandEvent},
    ShellExt,
};
use url::Url;
use uuid::Uuid;

const STARTUP_INTERVAL: Duration = Duration::from_millis(500);
const SHUTDOWN_ATTEMPTS: usize = 100;
const SHUTDOWN_INTERVAL: Duration = Duration::from_millis(250);

struct RunningBackend {
    child: CommandChild,
    api_port: u16,
    shutdown_token: String,
}

struct BackendState {
    process: Mutex<Option<RunningBackend>>,
    exit_in_progress: AtomicBool,
}

/// Startup must wait for an external PostgreSQL migration without an arbitrary
/// deadline. The sidecar event stream lets the desktop distinguish a real
/// startup failure from a slow but healthy schema upgrade.
#[derive(Default)]
struct BackendStartupDiagnostics {
    terminated: AtomicBool,
    migration_failed: AtomicBool,
    database_unavailable: AtomicBool,
}

#[derive(Debug, Deserialize)]
struct BackendMigrationLog {
    msg: Option<String>,
    migration_event: Option<String>,
    migration_version: Option<i64>,
    migration_name: Option<String>,
}

#[derive(Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct RuntimeConfig {
    #[serde(default)]
    database_url: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct DatabaseConnectionInput {
    host: String,
    port: u16,
    database: String,
    username: String,
    password: String,
    ssl_mode: String,
}

/// Non-sensitive connection fields returned to the packaged bootstrap page.
/// The password is deliberately never sent back to the webview.
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct DatabaseConnectionSummary {
    host: String,
    port: u16,
    database: String,
    username: String,
    ssl_mode: String,
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .manage(BackendState {
            process: Mutex::new(None),
            exit_in_progress: AtomicBool::new(false),
        })
        .invoke_handler(tauri::generate_handler![
            configure_database,
            get_database_connection,
            open_database_configuration,
            minimize_window,
            toggle_maximize_window,
            close_window,
        ])
        .setup(|app| {
            let window =
                WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                    .title("SpecRelay")
                    .decorations(false)
                    .inner_size(1360.0, 860.0)
                    .min_inner_size(980.0, 680.0)
                    .center()
                    .build()?;

            match load_runtime_config(&app.handle()) {
                Ok(Some(runtime)) => {
                    let _ = show_loading(
                        &window,
                        "正在连接已保存的 PostgreSQL 数据库…",
                        "连接成功后会验证数据库迁移；如有新版本表结构，会先安全升级再启动服务。",
                    );
                    if let Err(error) = start_backend(&app.handle(), &window, &runtime) {
                        eprintln!("SpecRelay startup failed: {error}");
                        show_database_setup_error(&window, &error)?;
                    }
                }
                Ok(None) => {}
                Err(error) => {
                    eprintln!("SpecRelay configuration load failed: {error}");
                    show_database_setup_error(&window, &error)?;
                }
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to build SpecRelay desktop application")
        .run(|app, event| match event {
            RunEvent::ExitRequested { api, .. } => {
                if begin_application_exit(app) {
                    api.prevent_exit();
                    show_shutdown_status(app);
                    // Let the webview paint the status before the orderly stop
                    // performs its short, bounded cleanup work on this thread.
                    thread::sleep(Duration::from_millis(80));
                    graceful_stop_backend(app);
                    app.exit(0);
                }
            }
            RunEvent::Exit => force_stop_backend(app),
            _ => {}
        });
}

#[tauri::command]
fn configure_database(
    app: AppHandle,
    window: WebviewWindow,
    input: DatabaseConnectionInput,
) -> Result<(), String> {
    let runtime = RuntimeConfig {
        database_url: build_database_url(input)?,
    };
    // Keep the exact previous configuration if a new connection cannot start.
    // This makes a failed edit recoverable after restarting the desktop app.
    let previous_runtime_config = read_runtime_config_snapshot(&app)?;
    save_runtime_config(&app, &runtime)?;
    show_loading(
        &window,
        "正在连接 PostgreSQL…",
        "连接成功后会检查数据库结构；如有新版本迁移，会先安全升级再启动服务。",
    )
    .map_err(|error| format!("无法更新启动界面：{error}"))?;

    if let Err(error) = start_backend(&app, &window, &runtime) {
        let _ = restore_runtime_config(&app, previous_runtime_config);
        let _ = show_database_setup_error(&window, &error);
        return Err(error);
    }
    Ok(())
}

#[tauri::command]
fn get_database_connection(app: AppHandle) -> Result<Option<DatabaseConnectionSummary>, String> {
    load_runtime_config(&app)?
        .map(database_connection_summary)
        .transpose()
}

#[tauri::command]
fn open_database_configuration(app: AppHandle, window: WebviewWindow) -> Result<(), String> {
    show_database_reconfiguration_status(&window)
        .map_err(|error| format!("无法显示数据库设置状态：{error}"))?;
    // Let the current page paint the safety message before stopping the local
    // sidecar. Only this desktop instance's backend is stopped; PostgreSQL and
    // any Docker containers are never touched.
    thread::sleep(Duration::from_millis(80));
    graceful_stop_backend(&app);
    window
        .eval("window.location.replace('tauri://localhost/index.html?mode=reconfigure');")
        .map_err(|error| format!("无法打开数据库连接设置：{error}"))
}

#[tauri::command]
fn minimize_window(window: WebviewWindow) -> Result<(), String> {
    window.minimize().map_err(|error| error.to_string())
}

#[tauri::command]
fn toggle_maximize_window(window: WebviewWindow) -> Result<(), String> {
    let maximized = window.is_maximized().map_err(|error| error.to_string())?;
    if maximized {
        window.unmaximize().map_err(|error| error.to_string())
    } else {
        window.maximize().map_err(|error| error.to_string())
    }
}

#[tauri::command]
fn close_window(window: WebviewWindow) -> Result<(), String> {
    window.close().map_err(|error| error.to_string())
}

fn start_backend(
    app: &AppHandle,
    window: &WebviewWindow,
    runtime: &RuntimeConfig,
) -> Result<(), String> {
    graceful_stop_backend(app);

    let app_data_dir = app
        .path()
        .app_data_dir()
        .map_err(|error| format!("无法确定应用数据目录：{error}"))?;
    fs::create_dir_all(&app_data_dir).map_err(|error| format!("无法创建应用数据目录：{error}"))?;

    let resources = app
        .path()
        .resource_dir()
        .map_err(|error| format!("无法定位安装包资源：{error}"))?;
    let frontend_dir = resource_path(&resources, "resources/frontend")?;
    if !frontend_dir.join("index.html").is_file() {
        return Err("安装包中缺少前端资源。请重新安装或重新执行打包脚本。".into());
    }

    let api_port = available_loopback_port()?;
    let access_token = random_token();
    let mcp_token = random_token();
    let shutdown_token = random_token();
    let instance_id = Uuid::new_v4().to_string();
    let backend = app
        .shell()
        .sidecar("specrelay")
        .map_err(|error| format!("无法找到宿主机后端程序：{error}"))?
        .env("DATABASE_URL", &runtime.database_url)
        .env("HTTP_ADDR", format!("127.0.0.1:{api_port}"))
        .env(
            "DATA_DIR",
            app_data_dir.join("data").to_string_lossy().to_string(),
        )
        .env("PUBLIC_DIR", frontend_dir.to_string_lossy().to_string())
        .env("ACCESS_TOKEN", &access_token)
        .env("MCP_TOKEN", &mcp_token)
        .env("SHUTDOWN_TOKEN", &shutdown_token)
        .env("INSTANCE_ID", &instance_id)
        .env("WORKER_CONCURRENCY", "2")
        // GUI launchers generally do not load the user's shell startup files.
        // Preserve the inherited PATH and add conventional local CLI locations
        // (notably nvm) so the backend can execute the Codex/Claude command
        // selected in project settings.
        .env("PATH", local_cli_search_path())
        .spawn()
        .map_err(|error| format!("无法启动宿主机后端：{error}"))?;

    let (mut receiver, child) = backend;
    let diagnostics = Arc::new(BackendStartupDiagnostics::default());
    let output_diagnostics = Arc::clone(&diagnostics);
    let output_window = window.clone();
    tauri::async_runtime::spawn(async move {
        while let Some(event) = receiver.recv().await {
            match event {
                CommandEvent::Stdout(line) => {
                    let line = String::from_utf8_lossy(&line);
                    eprintln!("[specrelay] {line}");
                    record_startup_output(&output_diagnostics, &line);
                }
                CommandEvent::Stderr(line) => {
                    let line = String::from_utf8_lossy(&line);
                    eprintln!("[specrelay] {line}");
                    record_startup_output(&output_diagnostics, &line);
                    update_database_migration_loading(&output_window, &line);
                }
                CommandEvent::Error(error) => {
                    eprintln!("[specrelay] sidecar error: {error}");
                }
                CommandEvent::Terminated(payload) => {
                    output_diagnostics.terminated.store(true, Ordering::SeqCst);
                    eprintln!("[specrelay] stopped: {payload:?}");
                }
                _ => {}
            }
        }
    });
    set_backend(app, child, api_port, shutdown_token)?;

    if let Err(error) = wait_for_backend(api_port, &diagnostics) {
        graceful_stop_backend(app);
        return Err(error);
    }

    let launch_url = format!("http://127.0.0.1:{api_port}/?token={access_token}");
    window
        .eval(&format!(
            "window.location.replace({});",
            json_string(&launch_url)
        ))
        .map_err(|error| format!("无法打开本地应用页面：{error}"))
}

/// Builds a PATH suitable for a desktop-launched backend.
///
/// A desktop app commonly starts without `.bashrc`/`.zshrc`, so tools installed
/// by npm, nvm, Volta, Cargo, or asdf are otherwise invisible to the sidecar.
/// The configured command remains authoritative; this only makes the normal
/// user-level installation directories discoverable.
fn local_cli_search_path() -> OsString {
    let inherited = env::var_os("PATH").unwrap_or_default();
    let mut paths = Vec::new();

    if let Some(home) = env::var_os("HOME").map(PathBuf::from) {
        add_path_if_directory(&mut paths, home.join(".local/bin"));
        add_path_if_directory(&mut paths, home.join(".npm-global/bin"));
        add_path_if_directory(&mut paths, home.join(".volta/bin"));
        add_path_if_directory(&mut paths, home.join(".cargo/bin"));
        add_path_if_directory(&mut paths, home.join(".asdf/shims"));

        let nvm_versions = home.join(".nvm/versions/node");
        if let Ok(entries) = fs::read_dir(nvm_versions) {
            let mut versions = entries
                .flatten()
                .map(|entry| entry.path())
                .collect::<Vec<_>>();
            versions.sort();
            for version in versions.into_iter().rev() {
                add_path_if_directory(&mut paths, version.join("bin"));
            }
        }
    }

    #[cfg(target_os = "windows")]
    {
        if let Some(app_data) = env::var_os("APPDATA").map(PathBuf::from) {
            add_path_if_directory(&mut paths, app_data.join("npm"));
        }
        if let Some(local_app_data) = env::var_os("LOCALAPPDATA").map(PathBuf::from) {
            add_path_if_directory(&mut paths, local_app_data.join("Volta/bin"));
        }
    }

    for path in env::split_paths(&inherited) {
        if !paths.iter().any(|candidate| candidate == &path) {
            paths.push(path);
        }
    }
    env::join_paths(paths).unwrap_or(inherited)
}

fn add_path_if_directory(paths: &mut Vec<PathBuf>, path: PathBuf) {
    if path.is_dir() && !paths.iter().any(|candidate| candidate == &path) {
        paths.push(path);
    }
}

fn set_backend(
    app: &AppHandle,
    child: CommandChild,
    api_port: u16,
    shutdown_token: String,
) -> Result<(), String> {
    let state = app
        .try_state::<BackendState>()
        .ok_or_else(|| "桌面后端状态尚未初始化。".to_string())?;
    let mut backend = state
        .process
        .lock()
        .map_err(|_| "桌面后端状态被意外锁定。".to_string())?;
    *backend = Some(RunningBackend {
        child,
        api_port,
        shutdown_token,
    });
    Ok(())
}

fn load_runtime_config(app: &AppHandle) -> Result<Option<RuntimeConfig>, String> {
    let config_path = runtime_config_path(app)?;
    if !config_path.exists() {
        return Ok(None);
    }

    let raw = fs::read(&config_path).map_err(|error| format!("无法读取数据库连接配置：{error}"))?;
    let config: RuntimeConfig =
        serde_json::from_slice(&raw).map_err(|error| format!("数据库连接配置格式无效：{error}"))?;
    if config.database_url.trim().is_empty() {
        return Ok(None);
    }
    validate_database_url(&config.database_url)?;
    Ok(Some(config))
}

fn save_runtime_config(app: &AppHandle, runtime: &RuntimeConfig) -> Result<(), String> {
    let config_path = runtime_config_path(app)?;
    let raw = serde_json::to_vec_pretty(runtime)
        .map_err(|error| format!("无法编码数据库连接配置：{error}"))?;
    fs::write(&config_path, raw).map_err(|error| format!("无法保存数据库连接配置：{error}"))?;
    restrict_permissions(&config_path);
    Ok(())
}

fn read_runtime_config_snapshot(app: &AppHandle) -> Result<Option<Vec<u8>>, String> {
    let config_path = runtime_config_path(app)?;
    if !config_path.exists() {
        return Ok(None);
    }
    fs::read(&config_path)
        .map(Some)
        .map_err(|error| format!("无法备份当前数据库连接配置：{error}"))
}

fn restore_runtime_config(app: &AppHandle, previous: Option<Vec<u8>>) -> Result<(), String> {
    let config_path = runtime_config_path(app)?;
    match previous {
        Some(raw) => {
            fs::write(&config_path, raw)
                .map_err(|error| format!("无法回滚数据库连接配置：{error}"))?;
            restrict_permissions(&config_path);
            Ok(())
        }
        None => {
            if config_path.exists() {
                fs::remove_file(&config_path)
                    .map_err(|error| format!("无法回滚数据库连接配置：{error}"))?;
            }
            Ok(())
        }
    }
}

fn database_connection_summary(
    runtime: RuntimeConfig,
) -> Result<DatabaseConnectionSummary, String> {
    let url = Url::parse(&runtime.database_url)
        .map_err(|error| format!("保存的 PostgreSQL 连接地址无效：{error}"))?;
    let host = url
        .host_str()
        .ok_or_else(|| "保存的数据库连接缺少主机地址。".to_string())?
        .to_string();
    let database = decode_database_component(url.path().trim_start_matches('/'), "数据库名称")?;
    let username = decode_database_component(url.username(), "用户名")?;
    if database.is_empty() || username.is_empty() {
        return Err("保存的数据库连接缺少数据库名称或用户名。".into());
    }
    Ok(DatabaseConnectionSummary {
        host,
        port: url.port().unwrap_or(5432),
        database,
        username,
        ssl_mode: url
            .query_pairs()
            .find(|(key, _)| key == "sslmode")
            .map(|(_, value)| value.into_owned())
            .unwrap_or_else(|| "disable".to_string()),
    })
}

fn decode_database_component(value: &str, label: &str) -> Result<String, String> {
    percent_decode_str(value)
        .decode_utf8()
        .map(|value| value.into_owned())
        .map_err(|_| format!("保存的数据库连接中的{label}不是有效的 UTF-8 文本。"))
}

fn runtime_config_path(app: &AppHandle) -> Result<PathBuf, String> {
    let app_data_dir = app
        .path()
        .app_data_dir()
        .map_err(|error| format!("无法确定应用数据目录：{error}"))?;
    fs::create_dir_all(&app_data_dir).map_err(|error| format!("无法创建应用数据目录：{error}"))?;
    Ok(app_data_dir.join("desktop.json"))
}

fn build_database_url(input: DatabaseConnectionInput) -> Result<String, String> {
    let host = input.host.trim();
    let database = input.database.trim();
    let username = input.username.trim();
    let ssl_mode = input.ssl_mode.trim();

    if host.is_empty() || database.is_empty() || username.is_empty() {
        return Err("请填写数据库主机、数据库名称和用户名。".into());
    }
    if input.port == 0 {
        return Err("数据库端口必须在 1 到 65535 之间。".into());
    }
    if !matches!(
        ssl_mode,
        "disable" | "prefer" | "require" | "verify-ca" | "verify-full"
    ) {
        return Err("SSL 模式无效。".into());
    }

    let mut url = Url::parse("postgresql://localhost")
        .map_err(|error| format!("无法创建 PostgreSQL 连接地址：{error}"))?;
    url.set_host(Some(host))
        .map_err(|_| "数据库主机地址无效。IPv6 地址请不要包含方括号。".to_string())?;
    url.set_port(Some(input.port))
        .map_err(|_| "数据库端口无效。".to_string())?;
    url.set_username(username)
        .map_err(|_| "数据库用户名无效。".to_string())?;
    url.set_password(Some(&input.password))
        .map_err(|_| "数据库密码包含无法使用的字符。".to_string())?;
    url.set_path(database);
    url.query_pairs_mut().append_pair("sslmode", ssl_mode);
    Ok(url.into())
}

fn validate_database_url(value: &str) -> Result<(), String> {
    let url =
        Url::parse(value).map_err(|error| format!("保存的 PostgreSQL 连接地址无效：{error}"))?;
    if !matches!(url.scheme(), "postgres" | "postgresql") || url.host_str().is_none() {
        return Err("保存的数据库连接不是有效的 PostgreSQL 地址。".into());
    }
    Ok(())
}

#[cfg(unix)]
fn restrict_permissions(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    let _ = fs::set_permissions(path, fs::Permissions::from_mode(0o600));
}

#[cfg(not(unix))]
fn restrict_permissions(_path: &Path) {}

fn resource_path(resources: &Path, relative: &str) -> Result<PathBuf, String> {
    let direct = resources.join(relative);
    if direct.exists() {
        return Ok(direct);
    }
    let basename = Path::new(relative)
        .file_name()
        .ok_or_else(|| format!("无效资源路径：{relative}"))?;
    let fallback = resources.join(basename);
    if fallback.exists() {
        return Ok(fallback);
    }
    Err(format!("安装包中缺少资源：{relative}"))
}

fn random_token() -> String {
    format!("{}{}", Uuid::new_v4().simple(), Uuid::new_v4().simple())
}

fn available_loopback_port() -> Result<u16, String> {
    TcpListener::bind("127.0.0.1:0")
        .map_err(|error| format!("无法分配本机端口：{error}"))?
        .local_addr()
        .map(|address| address.port())
        .map_err(|error| format!("无法读取本机端口：{error}"))
}

fn wait_for_backend(port: u16, diagnostics: &BackendStartupDiagnostics) -> Result<(), String> {
    loop {
        if backend_is_ready(port) {
            return Ok(());
        }
        if diagnostics.terminated.load(Ordering::SeqCst) {
            return Err(startup_failure_message(diagnostics));
        }
        thread::sleep(STARTUP_INTERVAL);
    }
}

fn record_startup_output(diagnostics: &BackendStartupDiagnostics, line: &str) {
    let lower = line.to_ascii_lowercase();
    if lower.contains("migration integrity check failed")
        || lower.contains("apply migration ")
        || lower.contains("commit migration ")
        || lower.contains("prepare schema migration integrity metadata")
    {
        diagnostics.migration_failed.store(true, Ordering::SeqCst);
    }
    if lower.contains("database ping:")
        || lower.contains("failed to connect")
        || lower.contains("connection refused")
        || lower.contains("password authentication failed")
    {
        diagnostics
            .database_unavailable
            .store(true, Ordering::SeqCst);
    }
}

fn startup_failure_message(diagnostics: &BackendStartupDiagnostics) -> String {
    if diagnostics.migration_failed.load(Ordering::SeqCst) {
        return "数据库结构升级失败，应用未启动且本次迁移已回滚。请检查 PostgreSQL 权限、数据库空间及迁移文件完整性；修复后重新打开应用即可重试。".into();
    }
    if diagnostics.database_unavailable.load(Ordering::SeqCst) {
        return "无法连接 PostgreSQL 数据库。请检查数据库服务、网络、账号密码及 SSL 设置后重试。"
            .into();
    }
    "本机后端在启动完成前退出。请检查 PostgreSQL 连接和桌面端后端日志后重试。".into()
}

fn update_database_migration_loading(window: &WebviewWindow, line: &str) {
    let Ok(event) = serde_json::from_str::<BackendMigrationLog>(line.trim()) else {
        return;
    };
    if event.msg.as_deref() != Some("database migration") {
        return;
    }
    let (title, detail) = match event.migration_event.as_deref() {
        Some("waiting_for_lock") => (
            "正在等待数据库更新锁…",
            "另一台 SpecRelay 可能正在更新同一个数据库。为避免重复修改表结构，当前应用会安全等待。",
        ),
        Some("verifying") => (
            "正在检查数据库结构…",
            "正在验证已应用的数据库迁移并检查是否需要升级。",
        ),
        Some("applying") => {
            let version = event
                .migration_version
                .map(|value| value.to_string())
                .unwrap_or_else(|| "新的".into());
            let name = event.migration_name.as_deref().unwrap_or("数据库迁移");
            let detail = format!(
                "正在应用数据库更新 {version}（{name}）。请保持应用打开，不会删除现有数据。"
            );
            let _ = show_loading(window, "正在升级数据库结构…", &detail);
            return;
        }
        Some("applied") => ("数据库结构已更新…", "正在继续检查其余更新并恢复本机服务。"),
        Some("complete") => (
            "数据库检查完成…",
            "正在启动本机服务并恢复已保存的任务状态。",
        ),
        _ => return,
    };
    let _ = show_loading(window, title, detail);
}

fn backend_is_ready(port: u16) -> bool {
    let address = format!("127.0.0.1:{port}");
    let Ok(socket_address) = address.parse::<std::net::SocketAddr>() else {
        return false;
    };
    let Ok(mut stream) = TcpStream::connect_timeout(&socket_address, Duration::from_millis(400))
    else {
        return false;
    };
    let _ = stream.set_read_timeout(Some(Duration::from_millis(500)));
    let _ = stream.set_write_timeout(Some(Duration::from_millis(500)));
    if stream
        .write_all(
            format!("GET /readyz HTTP/1.1\r\nHost: {address}\r\nConnection: close\r\n\r\n")
                .as_bytes(),
        )
        .is_err()
    {
        return false;
    }
    let mut response = [0_u8; 128];
    match stream.read(&mut response) {
        Ok(size) => {
            response[..size].starts_with(b"HTTP/1.1 200")
                || response[..size].starts_with(b"HTTP/1.0 200")
        }
        Err(_) => false,
    }
}

fn show_loading(window: &WebviewWindow, title: &str, detail: &str) -> tauri::Result<()> {
    window.eval(&format!(
        "window.__SpecRelayDesktopState={{mode:'loading',title:{},detail:{}}};window.SpecRelayDesktop&&window.SpecRelayDesktop.showLoading({}, {});",
        json_string(title),
        json_string(detail),
        json_string(title),
        json_string(detail),
    ))
}

fn show_database_setup_error(window: &WebviewWindow, error: &str) -> tauri::Result<()> {
    window.eval(&format!(
        "window.__SpecRelayDesktopState={{mode:'configuration',message:{}}};window.SpecRelayDesktop&&window.SpecRelayDesktop.showConfiguration({});",
        json_string(error),
        json_string(error),
    ))
}

fn show_database_reconfiguration_status(window: &WebviewWindow) -> tauri::Result<()> {
    // The hosted React UI and the packaged bootstrap page have different DOMs.
    // A self-contained overlay works for either page while the existing backend
    // performs its orderly shutdown and persists task state.
    window.eval(
        r#"(() => {
          const id = '__specrelay_database_reconfiguration__';
          if (document.getElementById(id)) return;
          const overlay = document.createElement('div');
          overlay.id = id;
          overlay.setAttribute('role', 'status');
          overlay.setAttribute('aria-live', 'assertive');
          overlay.style.cssText = 'position:fixed;inset:0;z-index:2147483647;display:flex;align-items:center;justify-content:center;padding:24px;background:rgba(8,12,15,.94);color:#f2f7f4;font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;';
          overlay.innerHTML = '<div style="max-width:460px;text-align:center"><div style="width:34px;height:34px;margin:0 auto 18px;border:3px solid #466052;border-top-color:#9be6bf;border-radius:50%;animation:specrelay-db-reconfigure .8s linear infinite"></div><div style="font-size:19px;font-weight:700">正在切换数据库连接设置…</div><div style="margin-top:10px;color:#b8c8bf;font-size:14px;line-height:1.65">正在安全停止本桌面端启动的后端与 CLI 任务，并保存当前执行状态。不会停止或修改 PostgreSQL / Docker 数据库。</div></div>';
          const style = document.createElement('style');
          style.textContent = '@keyframes specrelay-db-reconfigure{to{transform:rotate(360deg)}}';
          document.head.appendChild(style);
          document.documentElement.appendChild(overlay);
        })();"#,
    )
}

fn json_string(value: &str) -> String {
    serde_json::to_string(value).expect("string serialization cannot fail")
}

fn show_shutdown_status(app: &AppHandle) {
    let Some(window) = app.get_webview_window("main") else {
        return;
    };
    // The application may already be displaying the hosted React UI rather
    // than the bootstrap page. Injecting this small self-contained overlay
    // keeps the close flow understandable in either state.
    let _ = window.eval(
        r#"(() => {
          const id = '__specrelay_safe_shutdown__';
          let overlay = document.getElementById(id);
          if (!overlay) {
            overlay = document.createElement('div');
            overlay.id = id;
            overlay.setAttribute('role', 'status');
            overlay.setAttribute('aria-live', 'assertive');
            overlay.style.cssText = 'position:fixed;inset:0;z-index:2147483647;display:flex;align-items:center;justify-content:center;padding:24px;background:rgba(8,12,15,.92);color:#f2f7f4;font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;';
            overlay.innerHTML = '<div style="max-width:440px;text-align:center"><div style="width:34px;height:34px;margin:0 auto 18px;border:3px solid #466052;border-top-color:#9be6bf;border-radius:50%;animation:specrelay-safe-stop .8s linear infinite"></div><div style="font-size:19px;font-weight:700">正在安全停止后台任务…</div><div style="margin-top:10px;color:#b8c8bf;font-size:14px;line-height:1.65">正在保存执行状态，并关闭本机 CLI 进程。请不要强制结束应用。</div></div>';
            const style = document.createElement('style');
            style.textContent = '@keyframes specrelay-safe-stop{to{transform:rotate(360deg)}}';
            document.head.appendChild(style);
            document.documentElement.appendChild(overlay);
          }
        })();"#,
    );
}

fn begin_application_exit(app: &AppHandle) -> bool {
    app.try_state::<BackendState>()
        .map(|state| {
            state
                .exit_in_progress
                .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                .is_ok()
        })
        .unwrap_or(true)
}

fn graceful_stop_backend(app: &AppHandle) {
    let backend = take_backend(app);
    let Some(backend) = backend else {
        return;
    };

    let requested = request_backend_shutdown(backend.api_port, &backend.shutdown_token);
    if requested && wait_for_backend_stop(backend.api_port) {
        return;
    }

    if requested {
        eprintln!("SpecRelay graceful backend shutdown timed out; forcing sidecar termination.");
    } else {
        eprintln!(
            "SpecRelay graceful backend shutdown request failed; forcing sidecar termination."
        );
    }
    let _ = backend.child.kill();
}

fn force_stop_backend(app: &AppHandle) {
    if let Some(backend) = take_backend(app) {
        let _ = backend.child.kill();
    }
}

fn take_backend(app: &AppHandle) -> Option<RunningBackend> {
    app.try_state::<BackendState>().and_then(|state| {
        state
            .process
            .lock()
            .ok()
            .and_then(|mut backend| backend.take())
    })
}

fn request_backend_shutdown(port: u16, token: &str) -> bool {
    let address = format!("127.0.0.1:{port}");
    let Ok(mut stream) = TcpStream::connect_timeout(
        &address.parse().expect("loopback address must be valid"),
        Duration::from_secs(2),
    ) else {
        return false;
    };
    let _ = stream.set_read_timeout(Some(Duration::from_secs(3)));
    let _ = stream.set_write_timeout(Some(Duration::from_secs(3)));
    let request = format!(
        "POST /internal/shutdown HTTP/1.1\r\nHost: {address}\r\nX-SpecRelay-Shutdown-Token: {token}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"
    );
    if stream.write_all(request.as_bytes()).is_err() {
        return false;
    }
    let mut response = [0_u8; 128];
    matches!(stream.read(&mut response), Ok(size) if response[..size].starts_with(b"HTTP/1.1 202") || response[..size].starts_with(b"HTTP/1.0 202"))
}

fn wait_for_backend_stop(port: u16) -> bool {
    let address = format!("127.0.0.1:{port}");
    let Ok(address) = address.parse::<std::net::SocketAddr>() else {
        return true;
    };
    for _ in 0..SHUTDOWN_ATTEMPTS {
        if TcpStream::connect_timeout(&address, Duration::from_millis(200)).is_err() {
            return true;
        }
        thread::sleep(SHUTDOWN_INTERVAL);
    }
    false
}
