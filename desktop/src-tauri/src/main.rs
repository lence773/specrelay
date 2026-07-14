use std::{
    fs,
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    path::{Path, PathBuf},
    process::Command,
    sync::Mutex,
    thread,
    time::Duration,
};

use serde::{Deserialize, Serialize};
use tauri::{Manager, RunEvent, WebviewUrl, WebviewWindow, WebviewWindowBuilder};
use tauri_plugin_shell::{
    process::{CommandChild, CommandEvent},
    ShellExt,
};
use uuid::Uuid;

const COMPOSE_PROJECT: &str = "specrelay-desktop";
const DATABASE_NAME: &str = "specrelay";
const DATABASE_USER: &str = "specrelay";
const STARTUP_ATTEMPTS: usize = 120;
const STARTUP_INTERVAL: Duration = Duration::from_millis(500);

struct BackendState(Mutex<Option<CommandChild>>);

#[derive(Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct RuntimeConfig {
    database_password: String,
    database_port: u16,
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .setup(|app| {
            let window =
                WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                    .title("SpecRelay")
                    .inner_size(1360.0, 860.0)
                    .min_inner_size(980.0, 680.0)
                    .center()
                    .build()?;

            if let Err(error) = start(app, &window) {
                eprintln!("SpecRelay startup failed: {error}");
                show_startup_error(&window, &error)?;
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to build SpecRelay desktop application")
        .run(|app, event| {
            if matches!(event, RunEvent::ExitRequested { .. } | RunEvent::Exit) {
                stop_backend(app);
            }
        });
}

fn start(app: &tauri::App, window: &WebviewWindow) -> Result<(), String> {
    let app_data_dir = app
        .path()
        .app_data_dir()
        .map_err(|error| format!("无法确定应用数据目录：{error}"))?;
    fs::create_dir_all(&app_data_dir).map_err(|error| format!("无法创建应用数据目录：{error}"))?;

    let runtime = load_or_create_runtime_config(&app_data_dir)?;
    let resources = app
        .path()
        .resource_dir()
        .map_err(|error| format!("无法定位安装包资源：{error}"))?;
    let compose_file = resource_path(&resources, "resources/postgres.compose.yml")?;
    let frontend_dir = resource_path(&resources, "resources/frontend")?;
    if !frontend_dir.join("index.html").is_file() {
        return Err("安装包中缺少前端资源。请重新安装或重新执行打包脚本。".into());
    }

    start_database(&compose_file, &runtime)?;

    let api_port = available_loopback_port()?;
    let access_token = random_token();
    let mcp_token = random_token();
    let backend = app
        .shell()
        .sidecar("specrelay")
        .map_err(|error| format!("无法找到宿主机后端程序：{error}"))?
        .env("DATABASE_URL", database_url(&runtime))
        .env("HTTP_ADDR", format!("127.0.0.1:{api_port}"))
        .env(
            "DATA_DIR",
            app_data_dir.join("data").to_string_lossy().to_string(),
        )
        .env("PUBLIC_DIR", frontend_dir.to_string_lossy().to_string())
        .env("ACCESS_TOKEN", &access_token)
        .env("MCP_TOKEN", &mcp_token)
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
    app.manage(BackendState(Mutex::new(Some(child))));

    if let Err(error) = wait_for_backend(api_port) {
        stop_backend(app);
        return Err(error);
    }

    let launch_url = format!("http://127.0.0.1:{api_port}/?token={access_token}");
    let script = format!("window.location.replace({});", json_string(&launch_url));
    window
        .eval(&script)
        .map_err(|error| format!("无法打开 SpecRelay 页面：{error}"))?;
    Ok(())
}

fn resource_path(resource_dir: &Path, relative: &str) -> Result<PathBuf, String> {
    let direct = resource_dir.join(relative);
    if direct.exists() {
        return Ok(direct);
    }
    // Tauri preserves the resource directory structure on supported desktop
    // targets. This fallback makes unpacked development builds easier to inspect.
    let basename = Path::new(relative)
        .file_name()
        .ok_or_else(|| "无效的资源路径".to_string())?;
    let fallback = resource_dir.join(basename);
    if fallback.exists() {
        Ok(fallback)
    } else {
        Err(format!("安装包资源不存在：{}", direct.display()))
    }
}

fn load_or_create_runtime_config(app_data_dir: &Path) -> Result<RuntimeConfig, String> {
    let config_path = app_data_dir.join("runtime.json");
    if config_path.is_file() {
        let raw = fs::read(&config_path).map_err(|error| format!("无法读取运行配置：{error}"))?;
        let config = serde_json::from_slice::<RuntimeConfig>(&raw)
            .map_err(|error| format!("运行配置格式无效：{error}"))?;
        if !config.database_password.is_empty() && config.database_port != 0 {
            return Ok(config);
        }
    }
    let config = RuntimeConfig {
        database_password: random_token(),
        database_port: available_loopback_port()?,
    };
    let raw =
        serde_json::to_vec_pretty(&config).map_err(|error| format!("无法编码运行配置：{error}"))?;
    fs::write(&config_path, raw).map_err(|error| format!("无法保存运行配置：{error}"))?;
    restrict_permissions(&config_path);
    Ok(config)
}

#[cfg(unix)]
fn restrict_permissions(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    let _ = fs::set_permissions(path, fs::Permissions::from_mode(0o600));
}

#[cfg(not(unix))]
fn restrict_permissions(_path: &Path) {}

fn random_token() -> String {
    format!("{}{}", Uuid::new_v4().simple(), Uuid::new_v4().simple())
}

fn database_url(runtime: &RuntimeConfig) -> String {
    format!(
        "postgresql://{DATABASE_USER}:{}@127.0.0.1:{}/{DATABASE_NAME}?sslmode=disable",
        runtime.database_password, runtime.database_port
    )
}

fn start_database(compose_file: &Path, runtime: &RuntimeConfig) -> Result<(), String> {
    let output = Command::new("docker")
        .args([
            "compose",
            "--project-name",
            COMPOSE_PROJECT,
            "--file",
            &compose_file.to_string_lossy(),
            "up",
            "--detach",
            "--wait",
        ])
        .env("POSTGRES_PASSWORD", &runtime.database_password)
        .env("POSTGRES_PORT", runtime.database_port.to_string())
        .output()
        .map_err(|error| {
            format!(
                "无法执行 Docker Compose（请安装并启动 Docker Desktop 或 Docker Engine）：{error}"
            )
        })?;
    if output.status.success() {
        return Ok(());
    }
    let stderr = String::from_utf8_lossy(&output.stderr).trim().to_string();
    let stdout = String::from_utf8_lossy(&output.stdout).trim().to_string();
    let details = if stderr.is_empty() { stdout } else { stderr };
    Err(format!("PostgreSQL 数据库未能启动。{details}"))
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
    Err("等待后端就绪超时。请确认 Docker 数据库正常运行，并查看终端中的 [specrelay] 日志。".into())
}

fn backend_is_ready(port: u16) -> bool {
    let address = format!("127.0.0.1:{port}");
    let Ok(mut stream) =
        TcpStream::connect_timeout(&address.parse().unwrap(), Duration::from_millis(400))
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

fn show_startup_error(window: &WebviewWindow, error: &str) -> tauri::Result<()> {
    let message = json_string(error);
    window.eval(&format!(
        "document.title='SpecRelay 启动失败';const main=document.querySelector('main');main.replaceChildren();const h=document.createElement('h1');h.textContent='SpecRelay 无法启动';const p=document.createElement('p');p.textContent={message};const hint=document.createElement('p');hint.style.marginTop='1rem';hint.innerHTML='请确认 <code>docker compose version</code> 可用、Docker 已启动，然后重新打开应用。';main.append(h,p,hint);"
    ))
}

fn json_string(value: &str) -> String {
    serde_json::to_string(value).expect("string serialization cannot fail")
}

fn stop_backend(app: &tauri::AppHandle) {
    if let Some(state) = app.try_state::<BackendState>() {
        if let Ok(mut child) = state.0.lock() {
            if let Some(child) = child.take() {
                let _ = child.kill();
            }
        }
    }
}
