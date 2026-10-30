@echo off
REM Assemble the self-contained Windows release tree under output\.
REM
REM   output\
REM     geniex.exe          CLI
REM     geniex.dll          SDK runtime (loaded by the CLI through cgo)
REM     llama_cpp\          runtime plugin folder the SDK registry scans
REM       geniex_plugin.dll llama_cpp plugin (drop-in)
REM       llama.dll, ggml*.dll, mtmd.dll, llama-common.dll
REM     plugins\llama_cpp\  drop-in plugin subfolder (same content)
REM     lib\                runtime DLLs (llama / ggml / mtmd)
REM     include\            SDK headers
REM     run.cmd             launcher (sets the working directory to the root)
REM     geniex-cli-setup.iss InnoSetup script for the staged tree
REM     geniex.ico          installer icon
REM
REM Layout rationale: sdk/src/registry.cpp resolves the plugin root to the
REM directory that holds geniex.dll (get_shared_lib_dir) and then scans its
REM *immediate child directories* for a file literally named
REM "geniex_plugin.dll". The plugin itself (sdk/plugins/llama_cpp/src/
REM plugin.cpp) then resolves its backend directory to
REM "<plugin root>/llama_cpp" for the llama/ggml DLLs. Hence the plugin has
REM to live in <root>/llama_cpp/geniex_plugin.dll.
REM
REM Usage:  scripts\stage-output.cmd
REM
REM Requires the CLI at build-cli\geniex.exe and the installed SDK at
REM sdk\pkg-geniex (see sdk\build-x64.cmd install).

setlocal

set "ROOT=%~dp0.."
set "OUT=%ROOT%\output"
set "PKG=%ROOT%\sdk\pkg-geniex"
set "CLI=%ROOT%\build-cli\geniex.exe"

if not exist "%CLI%" (
  echo ERROR: %CLI% not found - build the CLI first.
  exit /b 1
)
if not exist "%PKG%\lib\geniex.dll" (
  echo ERROR: %PKG%\lib\geniex.dll not found - install the SDK first.
  exit /b 1
)

if exist "%OUT%" rd /s /q "%OUT%"
mkdir "%OUT%"  || exit /b 1
mkdir "%OUT%\lib" || exit /b 1
mkdir "%OUT%\llama_cpp" || exit /b 1
mkdir "%OUT%\plugins\llama_cpp" || exit /b 1
mkdir "%OUT%\include" || exit /b 1

copy /y "%CLI%" "%OUT%\geniex.exe" >nul || goto :err
copy /y "%PKG%\lib\geniex.dll" "%OUT%\geniex.dll" >nul || goto :err
copy /y "%PKG%\lib\llama_cpp\*.dll" "%OUT%\llama_cpp\" >nul || goto :err
copy /y "%PKG%\lib\llama_cpp\*.dll" "%OUT%\plugins\llama_cpp\" >nul || goto :err
REM lib\ gets the llama/ggml runtime only. geniex_plugin.dll is deliberately
REM NOT duplicated here: the registry registers the first plugin it finds per
REM plugin id (std::map::emplace does not overwrite), so a copy in lib\ would
REM shadow the canonical one in llama_cpp\.
copy /y "%PKG%\lib\llama_cpp\llama*.dll" "%OUT%\lib\" >nul || goto :err
copy /y "%PKG%\lib\llama_cpp\ggml*.dll" "%OUT%\lib\" >nul || goto :err
copy /y "%PKG%\lib\llama_cpp\mtmd.dll" "%OUT%\lib\" >nul || goto :err
copy /y "%PKG%\include\*" "%OUT%\include\" >nul || goto :err

REM InnoSetup script for the staged tree. The placeholders are rewritten so
REM the copied script resolves every Source/OutputDir relative to output\.
powershell -NoProfile -Command ^
  "(Get-Content -Raw '%ROOT%\cli\release\windows\geniex-cli-setup-staged.iss') " ^
  "-replace '__STAGED_ROOT__', '.' -replace '__OUTPUT_DIR__', '.' " ^
  "| Set-Content -Encoding ASCII '%OUT%\geniex-cli-setup.iss'" || goto :err
copy /y "%ROOT%\cli\release\windows\geniex.ico" "%OUT%\geniex.ico" >nul || goto :err

(
  echo @echo off
  echo REM Launcher for the staged GenieX release tree.
  echo REM Runs the CLI with the working directory set to the tree root so the
  echo REM SDK registry finds ^<root^>\llama_cpp\geniex_plugin.dll.
  echo setlocal
  echo cd /d "%%~dp0"
  echo geniex.exe %%*
  echo exit /b %%ERRORLEVEL%%
) > "%OUT%\run.cmd"

echo.
echo Staged %OUT%
dir /s /b "%OUT%"
exit /b 0

:err
echo ERROR: staging failed (is a previous geniex.exe still running?)
exit /b 1
