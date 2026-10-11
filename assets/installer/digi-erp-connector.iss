#define AppName "Digi ERP Connector"
#define AppPublisher "Digitrade"
#define AppExe "digi-erp-connector.exe"
#define ServiceExe "digi-erp-connectord.exe"
#define ServiceName "digi-erp-connectord"

#ifndef AppVersion
#define AppVersion "0.0.0"
#endif

#ifndef BuildDir
#define BuildDir "."
#endif

#ifndef OutputDir
#define OutputDir "."
#endif

[Setup]
; New AppId: this product installs side-by-side with the legacy erp-connector
; during migration and must never share its uninstall registration.
AppId={{3A7E2F41-9C6B-4D8A-BF1E-52D904A7C216}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
DefaultDirName={pf}\digi-erp-connector
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
OutputDir={#OutputDir}
OutputBaseFilename=digi-erp-connector-setup-{#AppVersion}
Compression=lzma
SolidCompression=yes
ArchitecturesAllowed=x64
ArchitecturesInstallIn64BitMode=x64
PrivilegesRequired=admin
UninstallDisplayIcon={app}\{#AppExe}
SetupIconFile={#SourcePath}\icon.ico

[Files]
Source: "{#BuildDir}\{#AppExe}"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BuildDir}\{#ServiceExe}"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourcePath}\icon.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourcePath}\launch-admin.vbs"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autodesktop}\{#AppName}"; Filename: "{sys}\wscript.exe"; Parameters: """{app}\launch-admin.vbs"""; WorkingDir: "{app}"; IconFilename: "{app}\icon.ico"

[Run]
Filename: "{cmd}"; Parameters: "/C sc.exe create {#ServiceName} binPath= ""{app}\{#ServiceExe}"" start= auto >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe description {#ServiceName} ""Digi ERP Connector API Service"" >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe start {#ServiceName} >nul 2>&1 & exit /b 0"; Flags: runhidden

; No [UninstallRun]: stopping, waiting for STOPPED and deleting the service are
; done in that order by CurUninstallStepChanged in the Code section below.
; "sc stop" returns at once, and an [UninstallRun] "sc delete" right after it
; would run while the daemon is still draining.

[Code]
// Stopping the service is graceful: the daemon lets a receipt being written
// finish and writes every queued order before it exits, which can take up to
// 150 s (autostart.StopTimeout). Setup and the uninstaller only ask it to stop
// (sc stop) and wait up to 180 s for STOPPED - never replace or delete it under
// that work.
const
  StopWaitSeconds = 180;
  ScServiceDoesNotExist = 1060;

// QueryService runs "sc query" for the service. Exists is false when it is not
// installed. Stopped is true when its STATE is STOPPED.
procedure QueryService(var Exists: Boolean; var Stopped: Boolean);
var
  OutFile: String;
  Output: AnsiString;
  ResultCode: Integer;
begin
  Exists := False;
  Stopped := False;
  OutFile := ExpandConstant('{tmp}\sc-query.txt');
  DeleteFile(OutFile);
  if not Exec(ExpandConstant('{cmd}'), '/C sc.exe query {#ServiceName} > "' + OutFile + '" 2>&1',
              '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    Exit;
  if ResultCode = ScServiceDoesNotExist then
    Exit;
  Exists := True;
  if LoadStringFromFile(OutFile, Output) then
    Stopped := Pos('STOPPED', Output) > 0;
end;

// StopServiceAndWait asks the service to stop and waits up to StopWaitSeconds
// for STOPPED. True when it is stopped or not installed.
function StopServiceAndWait(): Boolean;
var
  Exists, Stopped: Boolean;
  ResultCode, Waited: Integer;
begin
  Result := True;
  QueryService(Exists, Stopped);
  if (not Exists) or Stopped then
    Exit;

  Exec(ExpandConstant('{cmd}'), '/C sc.exe stop {#ServiceName} >nul 2>&1', '', SW_HIDE,
       ewWaitUntilTerminated, ResultCode);

  Waited := 0;
  while Waited < StopWaitSeconds do
  begin
    Sleep(1000);
    Waited := Waited + 1;
    QueryService(Exists, Stopped);
    if (not Exists) or Stopped then
      Exit;
  end;
  Result := False;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  if not StopServiceAndWait() then
    Result := 'The {#ServiceName} service did not stop within ' + IntToStr(StopWaitSeconds) +
      ' seconds, so Setup has not replaced it.' + #13#10#13#10 +
      'It may still be finishing a receipt or writing queued orders. Wait until it shows as ' +
      'Stopped in Services (services.msc), or stop it there, then run Setup again.';
end;

// Uninstall: stop and wait first, then delete the service, then the files go.
// The uninstaller cannot be aborted at this point, so a service that will not
// stop is reported and deleted anyway (Windows removes it once it exits).
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ResultCode: Integer;
begin
  if CurUninstallStep <> usUninstall then
    Exit;
  if not StopServiceAndWait() then
  begin
    Log('{#ServiceName} did not stop within ' + IntToStr(StopWaitSeconds) + ' s; deleting it anyway');
    if not UninstallSilent() then
      MsgBox('The {#ServiceName} service did not stop within ' + IntToStr(StopWaitSeconds) +
        ' seconds. It will be removed once it stops; some files may stay behind until then.',
        mbInformation, MB_OK);
  end;
  Exec(ExpandConstant('{cmd}'), '/C sc.exe delete {#ServiceName} >nul 2>&1', '', SW_HIDE,
       ewWaitUntilTerminated, ResultCode);
end;
