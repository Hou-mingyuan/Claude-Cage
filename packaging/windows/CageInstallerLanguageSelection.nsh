; Use the Windows display language instead of the user's number/date locale.
!macro CageSelectInstallerLanguage
  Push $0
  System::Call 'kernel32::GetUserDefaultUILanguage() i .r0'
  IntOp $0 $0 & 0xffff
  StrCpy $LANGUAGE ${LANG_SIMPCHINESE}
  ${If} $0 = 0x0404
  ${OrIf} $0 = 0x0c04
  ${OrIf} $0 = 0x1404
  ${OrIf} $0 = 0x7c04
    StrCpy $LANGUAGE ${LANG_TRADCHINESE}
  ${Else}
    IntOp $0 $0 & 0x03ff
    ; IntOp writes decimal text, and the LogicLib switch compares strings.
    ${Switch} $0
      ${Case} 25
        StrCpy $LANGUAGE ${LANG_RUSSIAN}
        ${Break}
      ${Case} 1
        StrCpy $LANGUAGE ${LANG_ARABIC}
        ${Break}
      ${Case} 41
        StrCpy $LANGUAGE ${LANG_FARSI}
        ${Break}
      ${Case} 18
        StrCpy $LANGUAGE ${LANG_KOREAN}
        ${Break}
      ${Case} 10
        StrCpy $LANGUAGE ${LANG_SPANISH}
        ${Break}
      ${Case} 85
        StrCpy $LANGUAGE ${LANG_BURMESE}
        ${Break}
      ${Case} 35
        StrCpy $LANGUAGE ${LANG_BELARUSIAN}
        ${Break}
      ${Case} 99
        StrCpy $LANGUAGE ${LANG_PASHTO}
        ${Break}
      ${Case} 140
        StrCpy $LANGUAGE ${LANG_DARI}
        ${Break}
      ${Case} 94
        StrCpy $LANGUAGE ${LANG_AMHARIC}
        ${Break}
      ${Case} 119
        StrCpy $LANGUAGE ${LANG_SOMALI}
        ${Break}
      ${Case} 115
        StrCpy $LANGUAGE ${LANG_TIGRINYA}
        ${Break}
      ${Case} 12
        StrCpy $LANGUAGE ${LANG_FRENCH}
        ${Break}
    ${EndSwitch}
  ${EndIf}
  Pop $0
!macroend
