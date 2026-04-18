
@echo off
setlocal enabledelayedexpansion

REM Set GOPATH if not already set
if not defined GOPATH (
    for /f "tokens=*" %%i in ('go env GOPATH') do set GOPATH=%%i
)

REM Define goversioninfo path
set GOVERSIONINFO=%GOPATH%\bin\goversioninfo.exe

REM Install goversioninfo for Windows if needed
set GOOS=windows
set GOARCH=amd64
if not exist "!GOVERSIONINFO!" (
    echo Installing goversioninfo for Windows...
    call go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
    if not exist "!GOVERSIONINFO!" (
        echo ERROR: Failed to install goversioninfo. Please run:
        echo   go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
        exit /b 1
    )
)

@echo Generating resources with go generate
go generate .\scoring_listener\
if errorlevel 1 exit /b 1

go generate .\video_server\
if errorlevel 1 exit /b 1

@echo Building scoring listener
@set GOOS=windows
@set GOARCH=amd64
go build -ldflags="-H windowsgui" .\scoring_listener\ 

@echo Building video server for Windows
:: copy so it gets included in the resources and compiled into the exe
copy scoring-listener.exe video_server\static\scoring-listener.exe

go build .\video_server\




:: Build for linux also
@echo Building video server for Linux
@set GOOS=linux
@set GOARCH=amd64
go build .\video_server\