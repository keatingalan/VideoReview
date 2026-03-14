
::go install github.com/akavel/rsrc@latest
rsrc -manifest walk.manifest -ico app.ico -o rsrc.syso

@set GOOS=windows
@set GOARCH=amd64
go build -ldflags="-H windowsgui" -o CaptureScoreGenMessages.exe .
