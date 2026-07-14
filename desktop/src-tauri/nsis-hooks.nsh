; Prevent an in-use sidecar from producing NSIS's low-level
; "Error opening file for writing" dialog during an upgrade. The user can
; explicitly decide whether to stop the running desktop app before files are
; replaced, so active local CLI work is never terminated without consent.
!macro NSIS_HOOK_PREINSTALL
  nsExec::ExecToStack '"$SYSDIR\tasklist.exe" /FI "IMAGENAME eq SpecRelay.exe" /NH'
  Pop $0
  Pop $1
  StrCmp $1 "INFO: No tasks are running which match the specified criteria." check_sidecar
  Goto prompt_to_stop

  check_sidecar:
  nsExec::ExecToStack '"$SYSDIR\tasklist.exe" /FI "IMAGENAME eq specrelay.exe" /NH'
  Pop $0
  Pop $1
  StrCmp $1 "INFO: No tasks are running which match the specified criteria." installation_ready

  prompt_to_stop:
  MessageBox MB_ICONEXCLAMATION|MB_YESNO "SpecRelay 正在运行，无法安全覆盖更新文件。$\r$\n$\r$\n选择“是”将关闭 SpecRelay 及其本地后端后继续安装；正在运行的 CLI 任务会停止。$\r$\n选择“否”会取消安装，您可以先在应用中关闭 SpecRelay 后重新运行安装包。" IDYES stop_specrelay
  Abort

  stop_specrelay:
  nsExec::ExecToStack '"$SYSDIR\taskkill.exe" /IM "SpecRelay.exe" /T /F'
  Pop $0
  Pop $1
  nsExec::ExecToStack '"$SYSDIR\taskkill.exe" /IM "specrelay.exe" /T /F'
  Pop $0
  Pop $1
  Sleep 800

  installation_ready:
!macroend
