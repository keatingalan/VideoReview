.PHONY: all server listener clean

all: server listener

# Build the video server for the current platform.
server:
	cd video_server && go build -o ../WAG-Video-Review$(if $(filter windows,$(GOOS)),.exe,) .

# Build the scoring listener — Windows only (requires lxn/walk).
# Run: make listener  (from a Windows machine or cross-compile with GOOS=windows)
listener:
	cd scoring_listener && \
		rsrc -manifest rsrc.manifest -ico app.ico -o rsrc.syso && \
		GOOS=windows GOARCH=amd64 go build -ldflags="-H windowsgui" -o ../CaptureScoreGen.exe .

clean:
	rm -f WAG-Video-Review WAG-Video-Review.exe CaptureScoreGen.exe \
		video_server/rsrc.syso scoring_listener/rsrc.syso
