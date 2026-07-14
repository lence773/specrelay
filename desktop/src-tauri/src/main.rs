use std::{
    fs,
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicBool, Ordering},
        Mutex,
    },
    thread,
    time::Duration,
};

use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Manager, RunEvent, WebviewUrl, WebviewWindow, WebviewWindowBuilder};
use tauri_plugin_shell::{
    process::{CommandChild, CommandEvent},
    ShellExt,
};
use url::Url;
use uuid::Uuid;

const STARTUP_ATTEMPTS: usize = 120;
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

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .manage(BackendState {
            process: Mutex::new(None),
            exit_in_progress: AtomicBool::new(false),
        })
        .invoke_handler(tauri::generate_handler![
            configure_database,
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
                        "连接成功后会自动检查并初始化所需的数据表。",
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
    save_runtime_config(&app, &runtime)?;
    show_loading(
        &window,
        "正在连接 PostgreSQL…",
        "首次连接时，SpecRelay 会自动创建所需的数据表并执行数据库迁移。",
    )
    .map_err(|error| format!("无法更新启动界面：{error}"))?;

    if let Err(error) = start_backend(&app, &window, &runtime) {
        let _ = show_database_setup_error(&window, &error);
        return Err(error);
    }
    Ok(())
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
        .spawn()
        .map_err(|error| format!("无法启动宿主机后端：{error}"))?;

    let (mut receiver, child) = backend;
    tauri::async_runtime::spawn(async move {
        while let Some(event) = receiver.recv().await {
            match event {
                CommandEvent::Stdout(line) => {
                    eprintln!("[specrelay] {}", String::from_utf8_lossy(&line))
                }
                CommandEvent::Stderr(line) => {
                    eprintln!("[specrelay] {}", String::from_utf8_lossy(&line))
                }
                CommandEvent::Error(error) => eprintln!("[specrelay] sidecar error: {error}"),
                CommandEvent::Terminated(payload) => eprintln!("[specrelay] stopped: {payload:?}"),
                _ => {}
            }
        }
    });
    set_backend(app, child, api_port, shutdown_token)?;

    if let Err(error) = wait_for_backend(api_port) {
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

fn wait_for_backend(port: u16) -> Result<(), String> {
    for _ in 0..STARTUP_ATTEMPTS {
        if backend_is_ready(port) {
            return Ok(());
        }
        thread::sleep(STARTUP_INTERVAL);
    }
    Err("等待后端就绪超时。请确认 PostgreSQL 连接信息正确且数据库可访问，然后重试。".into())
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
