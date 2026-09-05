!include "MUI2.nsh"
!include "nsDialogs.nsh"
!include "LogicLib.nsh"
!include "WinVer.nsh"
!include "x64.nsh"

!ifndef VARIANT
  !define VARIANT "full"
!endif

!define PRODUCT_NAME "HomeAgent"
!define PRODUCT_PUBLISHER "HomeAgent Team"
# 版本号由 makensis -DPRODUCT_VERSION=X.Y.Z 注入；缺省值仅供本地手工构建。
# 此前硬编码 0.8.0 而 release 已到 1.0.0，装出来的包在「添加/删除程序」里
# 会显示错误版本（DisplayVersion 也取自这个宏）。
!ifndef PRODUCT_VERSION
  !define PRODUCT_VERSION "1.1.0"
!endif

!if "${VARIANT}" == "full"
  !define PRODUCT_DISPLAY_NAME "HomeAgent 完整版"
  !define OUTPUT_FILE "HomeAgent_v${PRODUCT_VERSION}_Full_win64.exe"
  !define HAS_CORE 1
  !define HAS_WAITER 1
  !define HAS_GUI 1
  !define HAS_CREDENTIALS 1
!else if "${VARIANT}" == "server"
  !define PRODUCT_DISPLAY_NAME "HomeAgent 服务端"
  !define OUTPUT_FILE "HomeAgent_v${PRODUCT_VERSION}_Server_win64.exe"
  !define HAS_CORE 1
  !define HAS_WAITER 0
  !define HAS_GUI 0
  !define HAS_CREDENTIALS 1
!else if "${VARIANT}" == "client"
  !define PRODUCT_DISPLAY_NAME "HomeAgent 客户端"
  !define OUTPUT_FILE "HomeAgent_v${PRODUCT_VERSION}_Client_win64.exe"
  !define HAS_CORE 0
  !define HAS_WAITER 1
  !define HAS_GUI 1
  !define HAS_CREDENTIALS 0
!else
  !error "Unknown variant: ${VARIANT}"
!endif

Name "${PRODUCT_DISPLAY_NAME}"
OutFile "..\..\build\${OUTPUT_FILE}"
InstallDir "$PROGRAMFILES64\${PRODUCT_NAME}"
InstallDirRegKey HKLM "Software\${PRODUCT_NAME}" ""
RequestExecutionLevel admin
BrandingText "HomeAgent Installer"
SetCompressor /SOLID lzma
ShowInstDetails show
ShowUninstDetails show

Var apiKey
Var webuiUsername
Var webuiPassword
Var hwndApiKey
Var hwndUsername
Var hwndPassword
Var autoStart
Var startNow
Var hwndAutoStart
Var hwndStartNow

Function GenKey
  nsExec::ExecToStack 'powershell -NoProfile -C "[System.Guid]::NewGuid().ToString($\'N$\')"'
  Pop $0
  Pop $1
  ${If} $1 == ""
    StrCpy $1 "homeagent"
  ${Else}
    StrCpy $1 $1 32
  ${EndIf}
  Push $1
FunctionEnd

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY

!if "${HAS_CREDENTIALS}" == "1"
Page custom pageApiKeys pageApiKeysLeave
Page custom pageCredentials pageCredentialsLeave
!endif

!if "${HAS_CORE}" == "1"
Page custom pageStartupOptions pageStartupOptionsLeave
!endif

!insertmacro MUI_PAGE_INSTFILES

Page custom pageFinishSummary

!insertmacro MUI_LANGUAGE "SimpChinese"
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  !insertmacro MUI_LANGDLL_DISPLAY
  StrCpy $autoStart "1"
  StrCpy $startNow "1"
!if "${HAS_CREDENTIALS}" == "0"
  Call GenKey
  Pop $apiKey
!endif
FunctionEnd

!if "${HAS_CORE}" == "1"

Function pageStartupOptions
  !insertmacro MUI_HEADER_TEXT "启动选项" "设置 HomeAgent 后端的启动方式"
  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}
  ${NSD_CreateLabel} 0 5u 100% 20u "HomeAgent 后端 (homed) 是持续运行的服务进程。$\r$\n请选择启动方式:"
  Pop $0
  ${NSD_CreateCheckBox} 10u 35u 100% 12u "开机自动启动后端 (添加到注册表启动项)"
  Pop $hwndAutoStart
  ${If} $autoStart == "1"
    ${NSD_Check} $hwndAutoStart
  ${EndIf}
  ${NSD_CreateCheckBox} 10u 55u 100% 12u "安装完成后立即启动后端"
  Pop $hwndStartNow
  ${If} $startNow == "1"
    ${NSD_Check} $hwndStartNow
  ${EndIf}
  ${NSD_CreateLabel} 10u 80u 100% 20u "如果选择开机自启动，homed 将在每次登录 Windows 时自动运行。$\r$\n你也可以稍后从开始菜单手动启动。"
  Pop $0
  nsDialogs::Show
FunctionEnd

Function pageStartupOptionsLeave
  ${NSD_GetState} $hwndAutoStart $autoStart
  ${NSD_GetState} $hwndStartNow $startNow
FunctionEnd

!endif

!if "${HAS_CREDENTIALS}" == "1"

Function pageApiKeys
  !insertmacro MUI_HEADER_TEXT "生成 API 密钥" "请复制此密钥，安装完成后将无法再次查看"
  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}
  Call GenKey
  Pop $apiKey
  ${NSD_CreateLabel} 0 5u 100% 12u "API Key (用于 WebUI 和 GUI 认证):"
  Pop $0
  ${NSD_CreateText} 0 20u 300u 12u $apiKey
  Pop $hwndApiKey
  ${NSD_CreateLabel} 0 45u 100% 30u "请用鼠标选中文本框中的密钥并复制 (Ctrl+C)。$\r$\n此密钥同时用于:$\r$\n  • Web 管理界面 (http://localhost:8080) 的 API 认证$\r$\n  • 桌面 GUI 应用的自动连接配置"
  Pop $0
  nsDialogs::Show
FunctionEnd

Function pageApiKeysLeave
  ${NSD_GetText} $hwndApiKey $apiKey
FunctionEnd

Function pageCredentials
  !insertmacro MUI_HEADER_TEXT "WebUI 登录设置" "设置 Web 管理界面的登录账号和密码"
  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}
  StrCpy $webuiUsername "admin"
  ${NSD_CreateLabel} 0 5u 70u 12u "用户名:"
  Pop $0
  ${NSD_CreateText} 85u 5u 180u 12u $webuiUsername
  Pop $hwndUsername
  ${NSD_CreateLabel} 0 25u 70u 12u "密码:"
  Pop $0
  ${NSD_CreatePassword} 85u 25u 180u 12u ""
  Pop $hwndPassword
  ${NSD_CreateLabel} 0 50u 100% 20u "这些凭证用于登录 Web 管理界面 http://localhost:8080"
  Pop $0
  nsDialogs::Show
FunctionEnd

Function pageCredentialsLeave
  ${NSD_GetText} $hwndUsername $webuiUsername
  ${NSD_GetText} $hwndPassword $webuiPassword
  ${If} $webuiPassword == ""
    StrCpy $webuiPassword "homeagent"
  ${EndIf}
FunctionEnd

!endif

Function pageFinishSummary
  !insertmacro MUI_HEADER_TEXT "安装完成" "以下为安装的关键信息，请截图或记录"
  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}
  ${NSD_CreateLabel} 0 5u 100% 12u "API Key:      $apiKey"
  Pop $0
  ${NSD_CreateLabel} 0 20u 100% 12u "GUI 预配置:   已自动写入连接配置"
  Pop $0
  ${NSD_CreateLabel} 0 35u 100% 12u "WebUI 地址:   http://localhost:8080"
  Pop $0
!if "${HAS_CREDENTIALS}" == "1"
  ${NSD_CreateLabel} 0 50u 100% 12u "WebUI 用户名: $webuiUsername"
  Pop $0
  ${NSD_CreateLabel} 0 65u 100% 12u "WebUI 密码:   $webuiPassword"
  Pop $0
!endif
  nsDialogs::Show
FunctionEnd

Section "Install" SEC_INSTALL
  SetOutPath "$INSTDIR"
  CreateDirectory "$INSTDIR\data"
  CreateDirectory "$INSTDIR\data\log"
  CreateDirectory "$INSTDIR\data\plugins"
  CreateDirectory "$INSTDIR\data\adapters"

!if "${HAS_CORE}" == "1"
  File "..\..\build\initconfig.exe"
  File "..\..\build\homed.exe"
!endif

!if "${HAS_WAITER}" == "1"
  File "..\..\build\waiter.exe"
!endif

!if "${HAS_GUI}" == "1"
  SetOutPath "$INSTDIR\homeagent-gui-win32-x64"
  File /r "..\..\build\homeagent-gui-win32-x64\*.*"
  SetOutPath "$INSTDIR"
!endif

!if "${HAS_CORE}" == "1"
  DetailPrint "初始化配置数据库..."
  nsExec::Exec '"$INSTDIR\initconfig.exe" -data "$INSTDIR\data" -username "$webuiUsername" -password "$webuiPassword" -apikey "$apiKey"'
  Pop $0
  ${If} $0 != 0
    DetailPrint "警告: 数据库初始化可能未成功完成"
  ${EndIf}
!endif

!if "${HAS_GUI}" == "1"
  DetailPrint "配置 GUI 连接..."
  CreateDirectory "$APPDATA\homeagent-gui"
  FileOpen $0 "$APPDATA\homeagent-gui\connections.json" w
  FileWrite $0 '{$\r$\n  "connections": [$\r$\n    {$\r$\n      "id": "local",$\r$\n      "name": "本地",$\r$\n      "url": "http://localhost:8080",$\r$\n      "apiKey": "$apiKey"$\r$\n    }$\r$\n  ],$\r$\n  "currentId": "local"$\r$\n}'
  FileClose $0
  ; 同时写入 GUI 包目录作为备用（适配 UAC 提升后 $APPDATA 异常的情况）
  CreateDirectory "$INSTDIR\homeagent-gui-win32-x64\resources\app"
  FileOpen $0 "$INSTDIR\homeagent-gui-win32-x64\resources\app\connections.json" w
  FileWrite $0 '{$\r$\n  "connections": [$\r$\n    {$\r$\n      "id": "local",$\r$\n      "name": "本地",$\r$\n      "url": "http://localhost:8080",$\r$\n      "apiKey": "$apiKey"$\r$\n    }$\r$\n  ],$\r$\n  "currentId": "local"$\r$\n}'
  FileClose $0
!endif

!if "${HAS_WAITER}" == "1"
  DetailPrint "配置 CLI 连接..."
  FileOpen $0 "$INSTDIR\waiter.yaml" w
  FileWrite $0 "socket: $\"$INSTDIR\data\cli.sock$\"$\r$\napi_key: $apiKey$\r$\ndefault: local$\r$\nconnections:$\r$\n  - name: local$\r$\n    socket: $\"$INSTDIR\data\cli.sock$\"$\r$\n    api_key: $apiKey$\r$\n"
  FileClose $0
!endif

  DetailPrint "创建快捷方式..."
  CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
!if "${HAS_CORE}" == "1"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\HomeAgent Server.lnk" "$INSTDIR\homed.exe" '-data "$INSTDIR\data"' "$INSTDIR\homed.exe" 0
!endif
!if "${HAS_WAITER}" == "1"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\HomeAgent CLI.lnk" "$INSTDIR\waiter.exe" "" "$INSTDIR\waiter.exe" 0
!endif
!if "${HAS_GUI}" == "1"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\HomeAgent GUI.lnk" "$INSTDIR\homeagent-gui-win32-x64\homeagent-gui.exe" "" "$INSTDIR\homeagent-gui-win32-x64\homeagent-gui.exe" 0
!endif

  DetailPrint "设置环境变量..."
  WriteRegStr HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "HOMEAGENT_DATA" "$INSTDIR\data"
  WriteRegStr HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "HOMEAGENT_SOCKET" "$INSTDIR\data\cli.sock"

!if "${HAS_CORE}" == "1"
  ${If} $autoStart == "1"
    DetailPrint "设置开机自启动..."
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "HomeAgent" '"$INSTDIR\homed.exe" -data "$INSTDIR\data"'
  ${EndIf}
!endif

  DetailPrint "写入注册表..."
  WriteRegStr HKLM "Software\${PRODUCT_NAME}" "" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayName" "${PRODUCT_DISPLAY_NAME}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "UninstallString" "$INSTDIR\Uninstall.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "Publisher" "${PRODUCT_PUBLISHER}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayVersion" "${PRODUCT_VERSION}"
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "NoRepair" 1

  WriteUninstaller "$INSTDIR\Uninstall.exe"

!if "${HAS_CORE}" == "1"
  ${If} $startNow == "1"
    DetailPrint "启动 HomeAgent 后端..."
    Exec '"$INSTDIR\homed.exe" -data "$INSTDIR\data"'
  ${EndIf}
!endif
SectionEnd

Section "Uninstall"
!if "${HAS_CORE}" == "1"
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "HomeAgent"
!endif
  Delete "$INSTDIR\Uninstall.exe"
  Delete "$INSTDIR\initconfig.exe"
  Delete "$INSTDIR\homed.exe"
  Delete "$INSTDIR\waiter.exe"
  Delete "$INSTDIR\waiter.yaml"
  RMDir /r "$INSTDIR\data"
  RMDir /r "$INSTDIR\homeagent-gui-win32-x64"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${PRODUCT_NAME}\HomeAgent Server.lnk"
  Delete "$SMPROGRAMS\${PRODUCT_NAME}\HomeAgent CLI.lnk"
  Delete "$SMPROGRAMS\${PRODUCT_NAME}\HomeAgent GUI.lnk"
  RMDir "$SMPROGRAMS\${PRODUCT_NAME}"
  DeleteRegValue HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "HOMEAGENT_DATA"
  DeleteRegValue HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "HOMEAGENT_SOCKET"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"
  DeleteRegKey HKLM "Software\${PRODUCT_NAME}"
SectionEnd
