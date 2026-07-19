!include "MUI2.nsh"
!include "nsDialogs.nsh"
!include "LogicLib.nsh"
!include "x64.nsh"
!include "WinVer.nsh"

!define PRODUCT_NAME "HomeAgent Toolchain"
!define PRODUCT_PUBLISHER "HomeAgent Team"
!define PRODUCT_VERSION "0.7.1"
!define PRODUCT_DISPLAY_NAME "HomeAgent 工具链"
!define OUTPUT_FILE "HomeAgent_v${PRODUCT_VERSION}_Toolchain_win64.exe"
!define SDK_VERSION "v0.7.1"

Name "${PRODUCT_DISPLAY_NAME} v${PRODUCT_VERSION}"
OutFile "${OUTPUT_FILE}"
InstallDir "$PROGRAMFILES64\${PRODUCT_NAME}"
InstallDirRegKey HKLM "Software\${PRODUCT_NAME}" ""
RequestExecutionLevel admin
BrandingText "HomeAgent Toolchain Installer"
SetCompressor /SOLID lzma
ShowInstDetails show
ShowUninstDetails show

Var hasGit
Var sdkInstallOk

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
Page custom pageConfirm pageConfirmLeave
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_LANGUAGE "SimpChinese"
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  !insertmacro MUI_LANGDLL_DISPLAY
  StrCpy $hasGit "0"
  StrCpy $sdkInstallOk "0"
FunctionEnd

Function pageConfirm
  !insertmacro MUI_HEADER_TEXT "确认安装" "将安装 HomeAgent 工具链并自动下载 SDK ${SDK_VERSION}"
  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}
  ${NSD_CreateLabel} 0 5u 100% 12u "将安装以下组件:"
  Pop $0
  ${NSD_CreateLabel} 15u 20u 100% 12u "• plugindev.exe  — 插件开发工具"
  Pop $0
  ${NSD_CreateLabel} 15u 35u 100% 12u "• SDK ${SDK_VERSION} — 将从远程仓库自动下载"
  Pop $0
  ${NSD_CreateLabel} 0 60u 100% 20u "SDK 需要 Git 客户端。如果未安装 Git，请先安装:$\r$\nhttps://git-scm.com/downloads"
  Pop $0
  nsDialogs::Show
FunctionEnd

Function pageConfirmLeave
FunctionEnd

Section "Install" SEC_INSTALL
  SetOutPath "$INSTDIR"
  
  DetailPrint "复制工具链文件..."
  File "plugindev.exe"

  DetailPrint "创建快捷方式..."
  CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\plugindev.lnk" "$INSTDIR\plugindev.exe" "" "$INSTDIR\plugindev.exe" 0

  DetailPrint "配置环境变量..."
  ; Add to system PATH
  ReadRegStr $0 HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "PATH"
  ${If} $0 != ""
    ${If} $0 != "*$INSTDIR*"
      StrCpy $0 "$0;$INSTDIR"
      WriteRegStr HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "PATH" $0
    ${EndIf}
  ${Else}
    WriteRegStr HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "PATH" "$INSTDIR"
  ${EndIf}
  WriteRegStr HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "HOMEAGENT_SDK_DIR" "$INSTDIR\sdk"
  WriteRegStr HKLM "Software\${PRODUCT_NAME}" "" "$INSTDIR"

  DetailPrint "检测 Git 客户端..."
  nsExec::ExecToStack '"git" --version'
  Pop $0
  Pop $1
  ${If} $0 == 0
    StrCpy $hasGit "1"
    DetailPrint "Git 已安装: $1"
  ${Else}
    DetailPrint "未检测到 Git，将跳过 SDK 自动下载"
    DetailPrint "安装完成后请手动运行: plugindev sdk install ${SDK_VERSION}"
  ${EndIf}

  ${If} $hasGit == "1"
    DetailPrint "正在下载 SDK ${SDK_VERSION}..."
    nsExec::ExecToStack '"$INSTDIR\plugindev.exe" sdk install ${SDK_VERSION}'
    Pop $0
    Pop $1
    ${If} $0 == 0
      StrCpy $sdkInstallOk "1"
      DetailPrint "SDK ${SDK_VERSION} 下载完成"
      DetailPrint "正在激活 SDK ${SDK_VERSION}..."
      nsExec::Exec '"$INSTDIR\plugindev.exe" sdk use ${SDK_VERSION}'
      Pop $0
    ${Else}
      DetailPrint "SDK 下载失败 (错误码: $0)"
      DetailPrint "请手动运行: plugindev sdk install ${SDK_VERSION}"
    ${EndIf}
  ${EndIf}

  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayName" "${PRODUCT_DISPLAY_NAME} v${PRODUCT_VERSION}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "UninstallString" "$INSTDIR\Uninstall.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "Publisher" "${PRODUCT_PUBLISHER}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "DisplayVersion" "${PRODUCT_VERSION}"
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" "NoRepair" 1

  WriteUninstaller "$INSTDIR\Uninstall.exe"
SectionEnd

Section "Uninstall"
  Delete "$INSTDIR\Uninstall.exe"
  Delete "$INSTDIR\plugindev.exe"
  RMDir /r "$INSTDIR\sdk"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${PRODUCT_NAME}\plugindev.lnk"
  RMDir "$SMPROGRAMS\${PRODUCT_NAME}"
  DeleteRegValue HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "HOMEAGENT_SDK_DIR"
  DeleteRegKey HKLM "Software\Microsoft\CurrentVersion\Uninstall\${PRODUCT_NAME}"
  DeleteRegKey HKLM "Software\${PRODUCT_NAME}"
SectionEnd
