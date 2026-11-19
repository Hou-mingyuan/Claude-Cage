Var CageInstallDirectory
Var CageOptionError
Var UpdateMode
Var DesktopShortcut
Var StartMenuShortcuts
Var DiagnosticFile

; NSIS consumes /D= and everything after it before setting $CMDLINE.
; Parse the original command line before touching the destination.
Function ParseCageInstallOptions
  StrCpy $CageInstallDirectory ""
  StrCpy $CageOptionError ""
  StrCpy $UpdateMode "0"
  StrCpy $DiagnosticFile ""
  System::Call 'kernel32::GetCommandLineW() p .r0'
  System::Call 'kernel32::lstrlenW(p r0) i .r1'
  ${If} $1 >= ${NSIS_MAX_STRLEN}
    StrCpy $CageOptionError "The installer command line is too long."
    Return
  ${EndIf}
  System::Call 'kernel32::GetCommandLineW() w .r0'
  StrCpy $1 0 ; offset in raw command line
  StrCpy $2 1 ; first token is the executable
  ${Do}
    ${Do}
      StrCpy $3 $0 1 $1
      ${If} $3 != " "
      ${AndIf} $3 != "$\t"
        ${ExitDo}
      ${EndIf}
      IntOp $1 $1 + 1
    ${Loop}
    ${If} $3 == ""
      Return
    ${EndIf}
    StrCpy $6 $0 3 $1
    ${If} $2 == 0
    ${AndIf} $6 == "/D="
      IntOp $1 $1 + 3
      StrCpy $CageInstallDirectory $0 "" $1
      StrCpy $7 $CageInstallDirectory 2 1
      ${If} $7 != ":\"
        Goto invalid_directory
      ${EndIf}
      ; A Windows drive path needs no quotes or forward slashes. A slash
      ; also identifies any option incorrectly placed after /D=.
      ${Do}
        StrCpy $3 $0 1 $1
        ${If} $3 == ""
          Return
        ${EndIf}
        ${If} $3 == "/"
        ${OrIf} $3 == '"'
        ${OrIf} $3 == "$\t"
          Goto invalid_directory
        ${EndIf}
        IntOp $1 $1 + 1
      ${Loop}
    ${EndIf}
    StrCpy $4 0 ; inside quotes
    StrCpy $5 "" ; current token
    ${Do}
      StrCpy $3 $0 1 $1
      ${If} $3 == ""
        ${ExitDo}
      ${ElseIf} $3 == '"'
        IntOp $4 $4 ^ 1
      ${Else}
        ${If} $4 == 0
          ${If} $3 == " "
          ${OrIf} $3 == "$\t"
            ${ExitDo}
          ${EndIf}
        ${EndIf}
        StrCpy $5 "$5$3"
      ${EndIf}
      IntOp $1 $1 + 1
    ${Loop}
    ${If} $4 != 0
      StrCpy $CageOptionError "An installer argument has an unclosed quote."
      Return
    ${EndIf}
    ${If} $2 == 1
      StrCpy $2 0
    ${ElseIf} $5 == "/UPDATE"
      StrCpy $UpdateMode "1"
      SetSilent silent
    ${ElseIf} $5 == "/S"
      SetSilent silent
    ${ElseIf} $5 != "/NCRC"
      StrCpy $6 $5 17
      StrCpy $7 $5 19
      StrCpy $8 $5 13
      ${If} $6 == "/DESKTOPSHORTCUT="
        StrCpy $DesktopShortcut $5 "" 17
        ${If} $DesktopShortcut != "0"
        ${AndIf} $DesktopShortcut != "1"
          StrCpy $CageOptionError "DESKTOPSHORTCUT must be 0 or 1."
          Return
        ${EndIf}
      ${ElseIf} $7 == "/STARTMENUSHORTCUT="
        StrCpy $StartMenuShortcuts $5 "" 19
        ${If} $StartMenuShortcuts != "0"
        ${AndIf} $StartMenuShortcuts != "1"
          StrCpy $CageOptionError "STARTMENUSHORTCUT must be 0 or 1."
          Return
        ${EndIf}
      ${ElseIf} $8 == "/DIAGNOSTICS="
        StrCpy $DiagnosticFile $5 "" 13
      ${Else}
        StrCpy $6 $5 3
        ${If} $6 == "/D="
          Goto invalid_directory
        ${EndIf}
        StrCpy $CageOptionError "Unknown installer option. Quote diagnostic paths containing spaces."
        Return
      ${EndIf}
    ${EndIf}
  ${Loop}
  invalid_directory:
    StrCpy $CageOptionError "/D= must be the last option. Give an unquoted absolute Windows path with backslashes, such as /D=D:\Cage Builds\Cage."
FunctionEnd
