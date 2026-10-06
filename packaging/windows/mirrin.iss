; Inno Setup script for Mirrin on Windows.
; Build: iscc packaging\windows\mirrin.iss  (after `make dist-portable` produced dist\mirrin-windows-amd64.exe;
; the release workflow wraps that same build, see docs/maintainers-release.md)
#define AppVersion GetEnv("VERSION")
#if AppVersion == ""
#define AppVersion "0.1.0"
#endif
[Setup]
; The AppId dates from before the rename and stays: this setup upgrades an
; AntBot install in place (same folder and Start menu group), and [Code]
; below tidies away the old program, shortcut and sign-in entry.
AppId={{7E1C6E4E-4C5C-4A5E-9C2B-ANTBOT01}
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
const
  // Uninstall key of openHuman, Mirrin's first name (a different AppId;
  // AntBot's is this setup's own).
  OldAppKey = 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{7E1C6E4E-4C5C-4A5E-9C2B-OPENHUMAN01}_is1';
  // How AntBot's sign-in entry names the twin's home.
  OldStartupHome = 'set "ANTBOT_HOME='; // rename:keep

function NeedsAddPath(Param: string): boolean;
var OrigPath: string;
begin
  if not RegQueryStringValue(HKCU, 'Environment', 'Path', OrigPath) then begin Result := True; exit; end;
  Result := Pos(';' + Param + ';', ';' + OrigPath + ';') = 0;
end;

// OldUninstaller is the uninstaller of a pre-rename openHuman install, or ''.
function OldUninstaller(): string;
var S: string;
begin
  Result := '';
  if RegQueryStringValue(HKCU, OldAppKey, 'UninstallString', S) or
     RegQueryStringValue(HKLM, OldAppKey, 'UninstallString', S) then
    Result := RemoveQuotes(S);
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

// StartupHome is the twin's home that AntBot's sign-in entry names, or ''.
function StartupHome(): string;
var Lines: TArrayOfString; I: Integer;
begin
  Result := '';
  if not LoadStringsFromFile(ExpandConstant('{userstartup}\AntBot.cmd'), Lines) then exit; // rename:keep
  for I := 0 to GetArrayLength(Lines) - 1 do
    if (Pos(OldStartupHome, Lines[I]) = 1) and (Length(Lines[I]) > Length(OldStartupHome) + 1) then begin
      Result := Copy(Lines[I], Length(OldStartupHome) + 1, Length(Lines[I]) - Length(OldStartupHome) - 1);
      StringChangeEx(Result, '%%', '%', True);
      exit;
    end;
end;

// TwinHome is where the twin lives: MIRRIN_HOME, else the ANTBOT_HOME an
// AntBot user set (in the environment, or in AntBot's sign-in entry), else
// %USERPROFILE%\.mirrin. An AntBot twin in .antbot moves to .mirrin when
// Mirrin first starts, and a home that names .antbot counts as that one.
function TwinHome(): string;
begin
  Result := GetEnv('MIRRIN_HOME');
  if Result = '' then Result := GetEnv('ANTBOT_HOME'); // rename:keep
  if Result = '' then Result := StartupHome();
  if Result = '' then Result := ExpandConstant('{%USERPROFILE}') + '\.mirrin';
end;

// WriteStartup puts the sign-in entry in the Startup folder, in place of
// AntBot's entry (which starts the older antbot.exe) and the shortcut
// earlier versions made (which started the tray without the service's
// environment): two entries would start two trays.
// It is UTF-8, as the script's chcp 65001 expects.
procedure WriteStartup();
var Dir: string; Lines: TArrayOfString;
begin
  Dir := ExpandConstant('{userstartup}');
  ForceDirectories(Dir);
  SetArrayLength(Lines, 1);
  Lines[0] := StartupScript(ExpandConstant('{app}\mirrin.exe'), TwinHome());
  SaveStringsToUTF8FileWithoutBOM(Dir + '\Mirrin.cmd', Lines, False);
  DeleteFile(Dir + '\AntBot.lnk'); // rename:keep
  DeleteFile(Dir + '\AntBot.cmd'); // rename:keep
end;

// Before installing, stop AntBot and openHuman, and remove openHuman. Their
// trays would otherwise start at sign-in beside Mirrin's, and while they run
// Mirrin can't move the twin's files to .mirrin. AntBot's sign-in entry
// becomes Mirrin's (the owner chose to start it at sign-in), and its program
// and shortcut, which this setup upgraded in place, go.
procedure CurStepChanged(CurStep: TSetupStep);
var Uninstaller: string; Code: Integer;
begin
  if CurStep = ssPostInstall then begin
    if WizardIsTaskSelected('startup') or FileExists(ExpandConstant('{userstartup}\AntBot.cmd')) then // rename:keep
      WriteStartup();
    exit;
  end;
  if CurStep <> ssInstall then exit;
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /IM antbot.exe', '', SW_HIDE, ewWaitUntilTerminated, Code); // rename:keep
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /IM openhuman.exe', '', SW_HIDE, ewWaitUntilTerminated, Code);
  DeleteFile(ExpandConstant('{app}\antbot.exe')); // rename:keep
  DeleteFile(ExpandConstant('{group}\AntBot (MAVRK).lnk')); // rename:keep
  DeleteFile(ExpandConstant('{group}\Mirrin (MAVRK).lnk')); // the shortcut's name before the default persona was Mirrin
  Uninstaller := OldUninstaller();
  if (Uninstaller <> '') and FileExists(Uninstaller) then
    Exec(Uninstaller, '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART', '', SW_HIDE, ewWaitUntilTerminated, Code);
end;
