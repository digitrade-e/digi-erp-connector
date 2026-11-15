#define AppName "Digi ERP Connector"
#define AppPublisher "Digitrade"
#define AppExe "digi-erp-connector.exe"
#define ServiceExe "digi-erp-connectord.exe"
#define ProvisionExe "digi-erp-connector-provision.exe"
#define ServiceName "digi-erp-connectord"
; The outbound link to Digitrade's relay (plan 083): its own service, so restarting the
; connector (options, upgrade, restore) never cuts it.
#define LinkExe "digi-erp-connector-link.exe"
#define LinkService "digi-erp-connector-link"

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
; The headless configuration tool digi-mcp runs over WinRM (plan 068).
Source: "{#BuildDir}\{#ProvisionExe}"; DestDir: "{app}"; Flags: ignoreversion
; The link: outbound HTTPS to link.digitrade-admin.org only (plan 083).
Source: "{#BuildDir}\{#LinkExe}"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourcePath}\icon.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourcePath}\launch-admin.vbs"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autodesktop}\{#AppName}"; Filename: "{sys}\wscript.exe"; Parameters: """{app}\launch-admin.vbs"""; WorkingDir: "{app}"; IconFilename: "{app}\icon.ico"

[Run]
Filename: "{cmd}"; Parameters: "/C sc.exe create {#ServiceName} binPath= ""{app}\{#ServiceExe}"" start= auto >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe description {#ServiceName} ""Digi ERP Connector API Service"" >nul 2>&1 & exit /b 0"; Flags: runhidden
; Restart after a failure - a crash or a non-zero exit, e.g. a data directory
; the daemon refused to start on (exit code 2, reason in the Event Log): after
; 1 min, then 5 min, then every 30 min (the count resets after a day).
Filename: "{cmd}"; Parameters: "/C sc.exe failure {#ServiceName} reset= 86400 actions= restart/60000/restart/300000/restart/1800000 >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe failureflag {#ServiceName} 1 >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe start {#ServiceName} >nul 2>&1 & exit /b 0"; Flags: runhidden

; The link service: LocalSystem, automatic, the same recovery as the daemon. Its Event Log
; source is registered so every command it runs shows in the Application log.
Filename: "{cmd}"; Parameters: "/C sc.exe create {#LinkService} binPath= ""{app}\{#LinkExe}"" start= auto >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe description {#LinkService} ""Digi ERP Connector link to Digitrade (outbound HTTPS only)"" >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe failure {#LinkService} reset= 86400 actions= restart/60000/restart/300000/restart/1800000 >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe failureflag {#LinkService} 1 >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C powershell.exe -NoProfile -NonInteractive -Command ""if (-not [Diagnostics.EventLog]::SourceExists('{#LinkService}')) {{ New-EventLog -LogName Application -Source '{#LinkService}' }"" >nul 2>&1 & exit /b 0"; Flags: runhidden
; Pairing (a code from Digitrade, typed on the page below or passed as /PAIRCODE=...) runs in
; CurStepChanged(ssPostInstall); a link already running picks the code up within 15 s.
Filename: "{cmd}"; Parameters: "/C sc.exe start {#LinkService} >nul 2>&1 & exit /b 0"; Flags: runhidden

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
procedure QueryService(const Name: String; var Exists: Boolean; var Stopped: Boolean);
var
  OutFile: String;
  Output: AnsiString;
  ResultCode: Integer;
begin
  Exists := False;
  Stopped := False;
  OutFile := ExpandConstant('{tmp}\sc-query.txt');
  DeleteFile(OutFile);
  if not Exec(ExpandConstant('{cmd}'), '/C sc.exe query ' + Name + ' > "' + OutFile + '" 2>&1',
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
function StopServiceAndWait(const Name: String): Boolean;
var
  Exists, Stopped: Boolean;
  ResultCode, Waited: Integer;
begin
  Result := True;
  QueryService(Name, Exists, Stopped);
  if (not Exists) or Stopped then
    Exit;

  Exec(ExpandConstant('{cmd}'), '/C sc.exe stop ' + Name + ' >nul 2>&1', '', SW_HIDE,
       ewWaitUntilTerminated, ResultCode);

  Waited := 0;
  while Waited < StopWaitSeconds do
  begin
    Sleep(1000);
    Waited := Waited + 1;
    QueryService(Name, Exists, Stopped);
    if (not Exists) or Stopped then
      Exit;
  end;
  Result := False;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  // The link first: it may be running a command that is restarting the daemon.
  if not StopServiceAndWait('{#LinkService}') then
  begin
    Result := 'The {#LinkService} service did not stop within ' + IntToStr(StopWaitSeconds) +
      ' seconds, so Setup has not replaced it. Stop it in Services (services.msc), then run Setup again.';
    Exit;
  end;
  if not StopServiceAndWait('{#ServiceName}') then
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
  // The link first, then the daemon. The link's key and link.yaml stay with the data
  // directory, as the daemon's config does; deleting the folder removes them.
  StopServiceAndWait('{#LinkService}');
  Exec(ExpandConstant('{cmd}'), '/C sc.exe delete {#LinkService} >nul 2>&1', '', SW_HIDE,
       ewWaitUntilTerminated, ResultCode);
  // The pairing goes with the link (plan 083 section 3.2): a later reinstall pairs again, never
  // silently takes back a box Digitrade could command.
  DeleteFile(ExpandConstant('{commonappdata}\digi-erp-connector\link.yaml'));
  DeleteFile(ExpandConstant('{commonappdata}\digi-erp-connector\secrets\link_box_keys.bin'));
  DeleteFile(ExpandConstant('{commonappdata}\digi-erp-connector\secrets\link_min_key_epoch.bin'));
  DelTree(ExpandConstant('{commonappdata}\digi-erp-connector\link'), True, True, True);
  if not StopServiceAndWait('{#ServiceName}') then
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

// --- Pairing with Digitrade (plan 083) -------------------------------------------------
// The code comes from Digitrade (ABCDE-FGHJK, single use, 30 minutes): typed on this page,
// or /PAIRCODE=... for a silent install. Empty = pair later with
// "digi-erp-connector-link.exe pair <code>". Setup never asks for a SQL password, opens a
// port or touches the firewall: everything after pairing is Digitrade's, over the link.
var
  PairPage: TInputQueryWizardPage;

procedure InitializeWizard();
begin
  PairPage := CreateInputQueryPage(wpSelectDir, 'Connect to Digitrade',
    'Pairing code',
    'If Digitrade gave you a pairing code, enter it here. The server then needs only outbound ' +
    'HTTPS (port 443) to link.digitrade-admin.org - nothing is opened inbound. Leave it empty to pair later.');
  PairPage.Add('Pairing code (like ABCDE-FGHJK):', False);
  PairPage.Values[0] := ExpandConstant('{param:PAIRCODE|}');
end;

function PairCode(): String;
begin
  Result := Trim(ExpandConstant('{param:PAIRCODE|}'));
  if (PairPage <> nil) and (Trim(PairPage.Values[0]) <> '') then
    Result := Trim(PairPage.Values[0]);
end;

// Only letters, digits and dashes reach the command line below (the code is typed or
// passed as /PAIRCODE=, and cmd.exe would run whatever follows an & in it).
function ValidPairCode(const Code: String): Boolean;
var
  I: Integer;
begin
  Result := (Length(Code) >= 10) and (Length(Code) <= 16);
  for I := 1 to Length(Code) do
    if not (((Code[I] >= 'A') and (Code[I] <= 'Z')) or ((Code[I] >= 'a') and (Code[I] <= 'z')) or
            ((Code[I] >= '0') and (Code[I] <= '9')) or (Code[I] = '-')) then
      Result := False;
end;

// The preflight (plan 083 section 3.4) runs when the pairing page is left, before anything
// is installed, from a temporary copy of the link: it finds the proxy the way the service
// will and says what to fix. A STOP does not end setup - the connector works without the
// link - but the person is asked first; a silent install only logs it.
function RunPreflight(): Boolean;
var
  OutFile: String;
  Output: AnsiString;
  ResultCode: Integer;
begin
  Result := True;
  ExtractTemporaryFile('{#LinkExe}');
  OutFile := ExpandConstant('{tmp}\link-preflight.txt');
  if not Exec(ExpandConstant('{cmd}'), '/C ""' + ExpandConstant('{tmp}\{#LinkExe}') + '" preflight > "' + OutFile +
              '" 2>&1"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    ResultCode := -1;
  LoadStringFromFile(OutFile, Output);
  Log('link preflight: exit ' + IntToStr(ResultCode) + #13#10 + String(Output));
  if WizardSilent() then
    Exit;
  case ResultCode of
    0: ;
    3: MsgBox('Connection check:' + #13#10#13#10 + String(Output), mbInformation, MB_OK);
  else
    Result := MsgBox('Connection check:' + #13#10#13#10 + String(Output) + #13#10 + 'Install anyway?',
      mbConfirmation, MB_YESNO) = IDYES;
  end;
end;

// Leaving the pairing page: a code that cannot be one is retyped there (a silent install
// logs it in CurStepChanged instead), then the preflight runs.
function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;
  if (PairPage = nil) or (CurPageID <> PairPage.ID) then
    Exit;
  if (PairCode() <> '') and (not ValidPairCode(PairCode())) and (not WizardSilent()) then
  begin
    MsgBox('That is not a pairing code: ten letters and digits, like ABCDE-FGHJK. ' +
      'Correct it, or leave it empty to pair later.', mbError, MB_OK);
    Result := False;
    Exit;
  end;
  Result := RunPreflight();
end;

// After the files are in and before [Run] starts the services: store the code. The
// fingerprint it prints is what the person reads to Digitrade to confirm the pairing.
procedure CurStepChanged(CurStep: TSetupStep);
var
  Code, OutFile: String;
  Output: AnsiString;
  ResultCode: Integer;
begin
  if CurStep <> ssPostInstall then
    Exit;
  Code := PairCode();
  if Code = '' then
    Exit;
  if not ValidPairCode(Code) then
  begin
    Log('link pair: the code has characters a pairing code never has; not stored');
    if not WizardSilent() then
      MsgBox('That is not a pairing code (ten letters and digits, like ABCDE-FGHJK). ' +
        'You can pair later: digi-erp-connector-link.exe pair <code>', mbError, MB_OK);
    Exit;
  end;
  OutFile := ExpandConstant('{tmp}\link-pair.txt');
  if not Exec(ExpandConstant('{cmd}'), '/C ""' + ExpandConstant('{app}\{#LinkExe}') + '" pair ' + Code +
              ' > "' + OutFile + '" 2>&1"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    ResultCode := -1;
  LoadStringFromFile(OutFile, Output);
  Log('link pair: exit ' + IntToStr(ResultCode) + ': ' + String(Output));
  if not WizardSilent() then
  begin
    if ResultCode = 0 then
      MsgBox(String(Output) + #13#10 + 'Read this fingerprint to Digitrade. They confirm it, and the server is connected.',
        mbInformation, MB_OK)
    else
      MsgBox('The pairing code was not stored:' + #13#10#13#10 + String(Output) + #13#10 +
        'You can pair later: digi-erp-connector-link.exe pair <code>', mbError, MB_OK);
  end;
end;
