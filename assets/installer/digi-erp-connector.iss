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

[UninstallRun]
Filename: "{cmd}"; Parameters: "/C sc.exe stop {#ServiceName} >nul 2>&1 & exit /b 0"; Flags: runhidden
Filename: "{cmd}"; Parameters: "/C sc.exe delete {#ServiceName} >nul 2>&1 & exit /b 0"; Flags: runhidden
