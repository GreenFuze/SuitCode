@echo off
:: dev-install.bat — build and install SuitCode from local source.
::
:: Use this during development instead of `go install ./...` so the correct
:: build flags are always applied (especially -ldflags "-H windowsgui" for the
:: coordinator, which prevents a console window from appearing on Windows).
::
:: Usage:
::   dev-install.bat           build + go install all three binaries
::   dev-install.bat --restart also kill any running coordinator and restart it
::
:: What each binary needs:
::   suitcode      — plain build, console subsystem (CLI tool)
::   coordinator   — -tags systray -ldflags "-H windowsgui"  (GUI subsystem, no console)
::   investigator  — plain build, console subsystem (spawned with CREATE_NO_WINDOW)

setlocal EnableDelayedExpansion

set RESTART=0

:parse_args
if "%~1"=="" goto args_done
if "%~1"=="--restart" set RESTART=1
shift
goto parse_args
:args_done

:: Verify Go is available.
where go >nul 2>&1
if errorlevel 1 (
    echo error: 'go' not found in PATH
    exit /b 1
)

echo Building and installing from local source...
echo.

:: suitcode CLI — standard console binary.
echo   go install ./suitcode/
go install ./suitcode/
if errorlevel 1 ( echo FAILED: suitcode && exit /b 1 )

:: coordinator — must be windowsgui (no console) and include the systray tag.
:: WARNING: omitting -ldflags "-H windowsgui" produces a console-subsystem
:: binary that shows a terminal window when launched from Explorer or auto-start.
echo   go install -tags systray -ldflags "-H windowsgui" ./coordinator/
go install -tags systray -ldflags "-H windowsgui" ./coordinator/
if errorlevel 1 ( echo FAILED: coordinator && exit /b 1 )

:: investigator — standard console binary; coordinator spawns it with
:: CREATE_NO_WINDOW so no console window appears even without the GUI subsystem.
echo   go install ./investigator/
go install ./investigator/
if errorlevel 1 ( echo FAILED: investigator && exit /b 1 )

echo.
echo   suitcode       OK
echo   coordinator    OK  (windowsgui, tray icon included)
echo   investigator   OK
echo.

:: Optionally kill the running coordinator and restart with the new binary.
if "!RESTART!"=="1" (
    echo Restarting coordinator...
    taskkill /F /IM coordinator.exe >nul 2>&1
    taskkill /F /IM investigator.exe >nul 2>&1
    timeout /t 1 /nobreak >nul
    start "" "%USERPROFILE%\go\bin\coordinator.exe"
    echo   coordinator restarted from %%USERPROFILE%%\go\bin\coordinator.exe
    echo.
)

echo Done^^! Binaries are in: %USERPROFILE%\go\bin

endlocal
