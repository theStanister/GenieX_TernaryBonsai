; Standalone InnoSetup script for the staged Windows release tree.
;
; This is the non-Bazel counterpart of geniex-cli-setup.iss: it references
; the explicit `output\` staging tree (produced by scripts\stage-output.cmd)
; instead of the Bazel-generated `ArtifactIss` manifest, so the installer can
; be compiled on a host where Bazel's windows_amd64 cgo toolchain is
; unavailable (no Developer Mode => no symlink forest).
;
; scripts\stage-output.cmd copies this file to output\geniex-cli-setup.iss
; and rewrites the two StagedRoot/OutputDir lines so the copy is relative to
; the staging tree. The in-repo geniex-cli-setup.iss + iss_manifest.bzl are
; kept in sync so the normal Bazel release path
; (`bazelisk build //cli/release/windows`) stays correct.
;
; Compile with:
;   ISCC.exe /DVersion=<version> cli\release\windows\geniex-cli-setup-staged.iss

#define MyAppName "GenieX CLI"
#define MyAppPublisher "GenieX"
#define MyAppExeName "geniex.exe"
#define MyAppIconSrc "geniex.ico"
#define MyAppIconName "geniex-" + Version + ".ico"
#define MyAppGuid "e9b30237-d65d-4a79-a7c0-f4e217e78f54"
#define LauncherTarget "powershell.exe"
#define LauncherArgs "-NoExit -Command geniex"
; __STAGED_ROOT__ and __OUTPUT_DIR__ are rewritten to "." by
; scripts\stage-output.cmd when it copies this script into output\.
#define StagedRoot "__STAGED_ROOT__"

[Setup]
AppId={{{#MyAppGuid}}
AppName={#MyAppName}
AppVersion={#Version}
AppPublisher={#MyAppPublisher}
DefaultDirName={localappdata}\{#MyAppName}
DefaultGroupName={#MyAppName}
OutputDir=__OUTPUT_DIR__
OutputBaseFilename=geniex-cli-setup
Compression=lzma
SolidCompression=yes
WizardStyle=modern
ChangesEnvironment=yes
SetupIconFile={#MyAppIconSrc}
PrivilegesRequired=lowest
UninstallDisplayName={#MyAppName}
UninstallDisplayIcon={app}\{#MyAppIconName}
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

[Files]
; CLI + SDK runtime
Source: "{#StagedRoot}\geniex.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#StagedRoot}\geniex.dll"; DestDir: "{app}"; Flags: ignoreversion

; llama_cpp plugin subfolder. The SDK registry (sdk/src/registry.cpp) only
; discovers a plugin whose file is literally named "geniex_plugin.dll"
; inside an immediate child directory of the directory holding geniex.dll,
; and the plugin resolves its backend runtime to "<root>\llama_cpp".
Source: "{#StagedRoot}\llama_cpp\geniex_plugin.dll"; DestDir: "{app}\llama_cpp"; Flags: ignoreversion
Source: "{#StagedRoot}\llama_cpp\llama.dll"; DestDir: "{app}\llama_cpp"; Flags: ignoreversion
Source: "{#StagedRoot}\llama_cpp\llama-common.dll"; DestDir: "{app}\llama_cpp"; Flags: ignoreversion
Source: "{#StagedRoot}\llama_cpp\ggml.dll"; DestDir: "{app}\llama_cpp"; Flags: ignoreversion
Source: "{#StagedRoot}\llama_cpp\ggml-base.dll"; DestDir: "{app}\llama_cpp"; Flags: ignoreversion
Source: "{#StagedRoot}\llama_cpp\ggml-cpu.dll"; DestDir: "{app}\llama_cpp"; Flags: ignoreversion
Source: "{#StagedRoot}\llama_cpp\mtmd.dll"; DestDir: "{app}\llama_cpp"; Flags: ignoreversion

; Drop-in copy of the plugin, for users who want to lift it into an
; existing GenieX installation's plugins folder.
Source: "{#StagedRoot}\plugins\llama_cpp\*"; DestDir: "{app}\plugins\llama_cpp"; Flags: ignoreversion skipifsourcedoesntexist recursesubdirs createallsubdirs

; Runtime DLLs and SDK headers
Source: "{#StagedRoot}\lib\*"; DestDir: "{app}\lib"; Flags: ignoreversion skipifsourcedoesntexist recursesubdirs createallsubdirs
Source: "{#StagedRoot}\include\*"; DestDir: "{app}\include"; Flags: ignoreversion skipifsourcedoesntexist recursesubdirs createallsubdirs

; Launcher
Source: "{#StagedRoot}\run.cmd"; DestDir: "{app}"; Flags: ignoreversion

Source: "{#MyAppIconSrc}"; DestDir: "{app}"; DestName: "{#MyAppIconName}"; Flags: ignoreversion

[Registry]
Root: HKCU; Subkey: "SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\{#MyAppExeName}"; ValueType: string; ValueName: ""; ValueData: "{app}\{#MyAppExeName}"; Flags: uninsdeletekey
Root: HKCU; Subkey: "SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\{#MyAppExeName}"; ValueType: string; ValueName: "Path"; ValueData: "{app}"; Flags: uninsdeletekey
Root: HKCU; Subkey: "SOFTWARE\Classes\Applications\{#MyAppExeName}"; ValueType: string; ValueName: "FriendlyAppName"; ValueData: "{#MyAppName}"; Flags: uninsdeletekey
Root: HKCU; Subkey: "SOFTWARE\Classes\Applications\{#MyAppExeName}\DefaultIcon"; ValueType: string; ValueName: ""; ValueData: "{app}\{#MyAppIconName}"; Flags: uninsdeletekey

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Icons]
Name: "{app}\{#MyAppName}";        Filename: "{#LauncherTarget}"; Parameters: "{#LauncherArgs}"; WorkingDir: "{app}"; IconFilename: "{app}\{#MyAppIconName}"
Name: "{group}\{#MyAppName}";      Filename: "{#LauncherTarget}"; Parameters: "{#LauncherArgs}"; WorkingDir: "{app}"; IconFilename: "{app}\{#MyAppIconName}"
Name: "{userdesktop}\{#MyAppName}"; Filename: "{#LauncherTarget}"; Parameters: "{#LauncherArgs}"; WorkingDir: "{app}"; IconFilename: "{app}\{#MyAppIconName}"; Tasks: desktopicon

[UninstallRun]
Filename: "taskkill.exe"; Parameters: "/F /IM {#MyAppExeName}"; Flags: runhidden

[UninstallDelete]
Type: filesandordirs; Name: "{app}"

[Code]
const
  EnvironmentKey = 'Environment';

function InitializeSetup(): Boolean;
var
  UninstallKey, UninstallString, Dummy: String;
  ResultCode, Waited: Integer;
begin
  Result := True;
  UninstallKey := 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{{#MyAppGuid}}_is1';
  if not RegQueryStringValue(HKCU, UninstallKey, 'UninstallString', UninstallString) then
    Exit;

  { Bare MsgBox still renders (hidden) under /VERYSILENT and deadlocks setup;
    SuppressibleMsgBox returns Default in silent mode. }
  if SuppressibleMsgBox('Existing version detected.'#13#10 +
            'Please uninstall the existing version first.'#13#10#13#10 +
            'Uninstall now?', mbConfirmation, MB_YESNO, IDYES) <> IDYES then
  begin
    SuppressibleMsgBox('Installation aborted.', mbInformation, MB_OK, IDOK);
    Result := False;
    Exit;
  end;

  { /SUPPRESSMSGBOXES is required — /VERYSILENT alone does not silence the
    old uninstaller's own MsgBox calls. }
  if (not Exec(RemoveQuotes(UninstallString), '/VERYSILENT /NORESTART /SUPPRESSMSGBOXES', '', SW_HIDE, ewWaitUntilTerminated, ResultCode))
     or (ResultCode <> 0) then
  begin
    SuppressibleMsgBox(Format('Uninstall failed (ErrCode: %d).', [ResultCode]), mbError, MB_OK, IDOK);
    Result := False;
    Exit;
  end;

  Waited := 0;
  while RegQueryStringValue(HKCU, UninstallKey, 'UninstallString', Dummy) do
  begin
    if Waited >= 30000 then
    begin
      SuppressibleMsgBox('Timed out waiting for the previous version to finish uninstalling.', mbError, MB_OK, IDOK);
      Result := False;
      Exit;
    end;
    Sleep(500);
    Waited := Waited + 500;
  end;
end;

procedure EnvAddPath(Path: string);
var
  Paths: string;
begin
  if not RegQueryStringValue(HKCU, EnvironmentKey, 'Path', Paths) then
    Paths := '';
  if Pos(';' + Uppercase(Path) + ';', ';' + Uppercase(Paths) + ';') > 0 then
    Exit;
  Paths := Paths + ';' + Path + ';';
  RegWriteStringValue(HKCU, EnvironmentKey, 'Path', Paths);
end;

procedure EnvRemovePath(Path: string);
var
  Paths: string;
  P: Integer;
begin
  if not RegQueryStringValue(HKCU, EnvironmentKey, 'Path', Paths) then
    Exit;
  P := Pos(';' + Uppercase(Path) + ';', ';' + Uppercase(Paths) + ';');
  if P = 0 then
    Exit;
  Delete(Paths, P - 1, Length(Path) + 1);
  RegWriteStringValue(HKCU, EnvironmentKey, 'Path', Paths);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
    EnvAddPath(ExpandConstant('{app}'));
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
    EnvRemovePath(ExpandConstant('{app}'));
end;
