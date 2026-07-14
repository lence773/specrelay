(() => {
  const setup = document.querySelector('#setup')
  const loading = document.querySelector('#loading')
  const form = document.querySelector('#database-form')
  const submit = document.querySelector('#submit')
  const error = document.querySelector('#error')
  const loadingTitle = document.querySelector('#loading-title')
  const loadingDetail = document.querySelector('#loading-detail')

  const invoke = (command, payload) => {
    const api = window.__TAURI__ && window.__TAURI__.core
    if (!api || typeof api.invoke !== 'function') {
      return Promise.reject(new Error('桌面端通信接口未就绪。请通过 SpecRelay 桌面安装包打开此页面。'))
    }
    return api.invoke(command, payload)
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

  window.SpecRelayDesktop = { showConfiguration, showLoading }
  const initialState = window.__SpecRelayDesktopState
  if (initialState?.mode === 'loading') showLoading(initialState.title, initialState.detail)
  if (initialState?.mode === 'configuration') showConfiguration(initialState.message)

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
      showLoading('正在连接 PostgreSQL…', '首次连接时，SpecRelay 会自动创建所需的数据表并执行数据库迁移。')
      await invoke('configure_database', { input })
    } catch (reason) {
      showConfiguration(reason instanceof Error ? reason.message : String(reason))
    }
  })

  document.querySelector('#minimize').addEventListener('click', () => invoke('minimize_window').catch(() => {}))
  document.querySelector('#maximize').addEventListener('click', () => invoke('toggle_maximize_window').catch(() => {}))
  document.querySelector('#close').addEventListener('click', () => invoke('close_window').catch(() => window.close()))
})()
