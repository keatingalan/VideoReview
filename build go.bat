
@echo off
@echo Building scoring listener
::go install github.com/akavel/rsrc@latest
rsrc -manifest scoring_listener\rsrc.manifest -ico scoring_listener\app.ico -o scoring_listener\rsrc.syso

@set GOOS=windows
@set GOARCH=amd64
go build -ldflags="-H windowsgui" .\scoring_listener\ 

@echo Building video server for Windows
::Copy so that executable will be available for download from the server later
copy scoring-listener.exe video_server\static\scoring-listener.exe

rsrc -manifest video_server\rsrc.manifest -ico video_server\app.ico -o video_server\rsrc.syso

go build .\video_server\

:: Build for linux also
@echo Building video server for Linux
@set GOOS=linux
@set GOARCH=amd64
go build .\video_server\