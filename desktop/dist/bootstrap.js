(() => {
  const setup = document.querySelector('#setup')
  const loading = document.querySelector('#loading')
  const form = document.querySelector('#database-form')
  const submit = document.querySelector('#submit')
  const error = document.querySelector('#error')
  const loadingTitle = document.querySelector('#loading-title')
  const loadingDetail = document.querySelector('#loading-detail')
  const setupEyebrow = document.querySelector('#setup-eyebrow')
  const setupTitle = document.querySelector('#setup-title')
  const setupLead = document.querySelector('#setup-lead')
  const setupNotice = document.querySelector('#setup-notice')
  const passwordHelper = document.querySelector('#password-helper')
  const password = document.querySelector('#password')
  const reconfiguring = new URLSearchParams(window.location.search).get('mode') === 'reconfigure'

  const invoke = (command, payload) => {
    const api = window.__TAURI__ && window.__TAURI__.core
    if (!api || typeof api.invoke !== 'function') {
      return Promise.reject(new Error('桌面端通信接口未就绪。请通过 SpecRelay 桌面安装包打开此页面。'))
    }
    return api.invoke(command, payload)
  }

  const setConfigurationMode = (hasSavedConnection) => {
    const editing = reconfiguring || hasSavedConnection
    setupEyebrow.textContent = editing ? '桌面端设置' : '首次启动设置'
    setupTitle.textContent = editing ? '更新 PostgreSQL 数据库连接' : '连接您的 PostgreSQL 数据库'
    setupLead.textContent = editing
      ? '请修改此桌面端后续启动时使用的 PostgreSQL 连接。已保存的主机、端口、数据库、用户名和 SSL 模式会预填；密码始终不会回显。'
      : 'SpecRelay 桌面端不会安装、携带或启动数据库。请填写一个由您管理且当前可访问的 PostgreSQL 数据库连接。'
    setupNotice.textContent = editing
      ? '已安全停止本桌面端启动的后端与 CLI 任务。保存并连接成功后会使用新配置重新启动；不会停止、删除或修改 PostgreSQL / Docker 数据库。'
      : '保存后会启动本机后端服务。若数据库尚未初始化，服务会自动创建数据表并执行迁移；不会删除已有数据。'
    passwordHelper.textContent = editing
      ? '为保护凭据，原密码不会回显。请填写新连接对应的密码（如果该用户没有密码，可留空）。'
      : '密码不会被后台接口回传，也不会在项目页面中显示。'
  }

  const showConfiguration = (message = '') => {
    loading.classList.remove('show')
    setup.classList.add('show')
    submit.disabled = false
    submit.textContent = '保存并连接数据库'
    error.textContent = message
    error.style.display = message ? 'block' : 'none'
  }

  const showLoading = (title, detail) => {
    setup.classList.remove('show')
    loading.classList.add('show')
    loadingTitle.textContent = title || '正在启动 SpecRelay…'
    loadingDetail.textContent = detail || '正在连接数据库并启动本机服务。'
  }

  const applySavedConnection = (connection) => {
    if (!connection) {
      setConfigurationMode(false)
      return
    }
    document.querySelector('#host').value = connection.host || ''
    document.querySelector('#port').value = String(connection.port || 5432)
    document.querySelector('#database').value = connection.database || ''
    document.querySelector('#username').value = connection.username || ''
    document.querySelector('#sslMode').value = connection.sslMode || 'disable'
    password.value = ''
    setConfigurationMode(true)
  }

  const loadSavedConnection = async () => {
    try {
      applySavedConnection(await invoke('get_database_connection'))
    } catch (_) {
      // Startup can be here precisely because a saved configuration is invalid.
      // Keep the form usable and avoid exposing any connection details in errors.
      setConfigurationMode(reconfiguring)
    }
  }

  window.SpecRelayDesktop = { showConfiguration, showLoading }
  const initialState = window.__SpecRelayDesktopState
  if (initialState?.mode === 'loading') showLoading(initialState.title, initialState.detail)
  if (initialState?.mode === 'configuration') showConfiguration(initialState.message)
  void loadSavedConnection()

  form.addEventListener('submit', async (event) => {
    event.preventDefault()
    const values = new FormData(form)
    const port = Number(values.get('port'))
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      showConfiguration('请输入 1 到 65535 之间的数据库端口。')
      return
    }
    const input = {
      host: String(values.get('host') || '').trim(),
      port,
      database: String(values.get('database') || '').trim(),
      username: String(values.get('username') || '').trim(),
      password: String(values.get('password') || ''),
      sslMode: String(values.get('sslMode') || 'disable'),
    }
    submit.disabled = true
    submit.textContent = '正在连接…'
    error.style.display = 'none'
    try {
      showLoading('正在连接 PostgreSQL…', '连接成功后，SpecRelay 会自动检查并初始化所需的数据表和迁移。')
      await invoke('configure_database', { input })
    } catch (reason) {
      showConfiguration(reason instanceof Error ? reason.message : String(reason))
    }
  })

  document.querySelector('#minimize').addEventListener('click', () => invoke('minimize_window').catch(() => {}))
  document.querySelector('#maximize').addEventListener('click', () => invoke('toggle_maximize_window').catch(() => {}))
  document.querySelector('#close').addEventListener('click', () => invoke('close_window').catch(() => window.close()))
})()
