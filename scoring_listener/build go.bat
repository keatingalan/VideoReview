
::go install github.com/akavel/rsrc@latest
rsrc -manifest rsrc.manifest -ico app.ico -o rsrc.syso

@set GOOS=windows
@set GOARCH=amd64
go build -ldflags="-H windowsgui" 