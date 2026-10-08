; Inno Setup script for Mirrin on Windows.
; Build: iscc packaging\windows\mirrin.iss  (after `make dist-portable` produced dist\mirrin-windows-amd64.exe;
; the release workflow wraps that same build, see docs/maintainers-release.md)
#define AppVersion GetEnv("VERSION")
#if AppVersion == ""
#define AppVersion "0.1.0"
#endif
[Setup]
AppId={{7E1C6E4E-4C5C-4A5E-9C2B-MIRRIN01}
AppName=Mirrin
AppVersion={#AppVersion}
AppPublisher=Mirrin contributors
AppPublisherURL=https://github.com/MavrkAI/Mirrin
DefaultDirName={autopf}\Mirrin
DefaultGroupName=Mirrin
DisableProgramGroupPage=yes
OutputDir=..\..\dist
OutputBaseFilename=Mirrin-{#AppVersion}-windows-setup
Compression=lzma
SolidCompression=yes
PrivilegesRequired=lowest
ChangesEnvironment=yes
; Sign with signtool when SIGNTOOL is set: SignTool=signtool
[Files]
Source: "..\..\dist\mirrin-windows-amd64.exe"; DestDir: "{app}"; DestName: "mirrin.exe"; Flags: ignoreversion
; The licences travel with the program (`mirrin licenses` prints them too).
Source: "..\..\LICENSE"; DestDir: "{app}"; DestName: "LICENSE.txt"; Flags: ignoreversion
Source: "..\..\THIRD_PARTY_NOTICES"; DestDir: "{app}"; DestName: "THIRD_PARTY_NOTICES.txt"; Flags: ignoreversion
[Icons]
Name: "{group}\Mirrin"; Filename: "{app}\mirrin.exe"; Parameters: "tray"
[Tasks]
Name: "startup"; Description: "Start Mirrin when I sign in (system tray)"; GroupDescription: "Startup:"
Name: "path"; Description: "Add mirrin to my PATH"; GroupDescription: "Command line:"
[Registry]
Root: HKCU; Subkey: "Environment"; ValueType: expandsz; ValueName: "Path"; ValueData: "{olddata};{app}"; Tasks: path; Check: NeedsAddPath('{app}')
[UninstallDelete]
; The sign-in entry written below (`mirrin service uninstall` removes it too).
Type: files; Name: "{userstartup}\Mirrin.cmd"
[Run]
; Through cmd /K, so the window stays open for the introduction and next steps
; instead of closing the moment setup finishes.
Filename: "{cmd}"; Parameters: "/K """"{app}\mirrin.exe"" init"""; Description: "Set up your twin now"; Flags: postinstall nowait skipifsilent
[Code]
function NeedsAddPath(Param: string): boolean;
var OrigPath: string;
begin
  if not RegQueryStringValue(HKCU, 'Environment', 'Path', OrigPath) then begin Result := True; exit; end;
  Result := Pos(';' + Param + ';', ';' + OrigPath + ';') = 0;
end;

// StartupScript is what `mirrin service install` writes to the Startup
// folder (internal/service/startup.go, startupScript): it starts
// `mirrin.exe tray` at sign-in with MIRRIN_HOME and MIRRIN_SERVICE set.
function StartupScript(Exe, Home: string): string;
begin
  StringChangeEx(Exe, '%', '%%', True);
  StringChangeEx(Home, '%', '%%', True);
  Result := '@echo off' + #13#10 +
    'chcp 65001 >nul' + #13#10 +
    'rem Starts Mirrin in the system tray when you sign in. `mirrin service uninstall` removes this.' + #13#10 +
    'set "MIRRIN_HOME=' + Home + '"' + #13#10 +
    'set "MIRRIN_SERVICE=1"' + #13#10 +
    'start "" /min "' + Exe + '" tray' + #13#10;
end;

// TwinHome is where the twin lives: MIRRIN_HOME, else %USERPROFILE%\.mirrin.
function TwinHome(): string;
begin
  Result := GetEnv('MIRRIN_HOME');
  if Result = '' then Result := ExpandConstant('{%USERPROFILE}') + '\.mirrin';
end;

// WriteStartup puts the sign-in entry in the Startup folder. It is UTF-8,
// as the script's chcp 65001 expects.
procedure WriteStartup();
var Dir: string; Lines: TArrayOfString;
begin
  Dir := ExpandConstant('{userstartup}');
  ForceDirectories(Dir);
  SetArrayLength(Lines, 1);
  Lines[0] := StartupScript(ExpandConstant('{app}\mirrin.exe'), TwinHome());
  SaveStringsToUTF8FileWithoutBOM(Dir + '\Mirrin.cmd', Lines, False);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if (CurStep = ssPostInstall) and WizardIsTaskSelected('startup') then
    WriteStartup();
end;
