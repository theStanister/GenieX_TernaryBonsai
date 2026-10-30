@echo off
cd /d %~dp0
geniex.exe %*
echo EXITCODE=%ERRORLEVEL%
