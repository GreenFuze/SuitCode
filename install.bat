@echo off
:: install.bat — install SuitCode binaries from the Go module registry
::
:: Usage:
::   install.bat            install suitcode, coordinator, investigator
::   install.bat --tray     also install the desktop tray icon
::   install.bat --ci       CGo-free build (no tray, safe for headless servers)

setlocal EnableDelayedExpansion

set "REPO=github.com/GreenFuze/SuitCode"
set TRAY=0
set CI=0

:: Parse arguments.
:parse_args
if "%~1"=="" goto args_done
if "%~1"=="--tray" set TRAY=1
if "%~1"=="--ci"   set CI=1
shift
goto parse_args
:args_done

:: Verify Go is available.
where go >nul 2>&1
if errorlevel 1 (
    echo error: 'go' not found in PATH -- install Go 1.21+ from https://go.dev/dl/
    exit /b 1
)

for /f "tokens=3" %%v in ('go version') do set GO_VER=%%v
echo Using !GO_VER!

:: Core binaries.
echo.
echo Installing core binaries...

if "!CI!"=="1" (
    set CGO_ENABLED=0
    go install !REPO!/suitcode@latest    || exit /b 1
    go install !REPO!/coordinator@latest || exit /b 1
    go install !REPO!/investigator@latest || exit /b 1
) else (
    go install !REPO!/suitcode@latest    || exit /b 1
    go install !REPO!/coordinator@latest || exit /b 1
    go install !REPO!/investigator@latest || exit /b 1
)

echo   suitcode       OK
echo   coordinator    OK
echo   investigator   OK

:: Optional tray icon.
if "!TRAY!"=="1" (
    echo.
    echo Installing desktop tray icon...
    go install -tags systray !REPO!/tray@latest || exit /b 1
    echo   suitcode-tray  OK
)

:: PATH reminder.
echo.
echo Done^^! Binaries are in: %USERPROFILE%\go\bin
echo.
echo Get started:
echo   suitcode . warmup
echo   suitcode . context --files ^<file^> --budget 8000

endlocal
