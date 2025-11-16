# srt-to-webrtc

Minimal SRT-to-WHIP forwarder written in Go. Listens for SRT (message API) on `PORT` or `7000`, sniffs incoming RTP, and forwards via WHIP using the SRT streamid as the bearer token.

## Requirements
- Go 1.25+
- libsrt with headers (`libsrt-dev` on Debian/Ubuntu)
- Network access to your WHIP endpoint

## Install
```
go mod download
```

## Run
```
go run . -whip https://your-whip-endpoint
```

SRT sender connects to `srt://<host>:7000?streamid=<key>`. The `<key>` is sent as `Authorization: Bearer <key>` in the WHIP request.
