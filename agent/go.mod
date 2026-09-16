module github.com/uptimemesh/agent

go 1.27

require github.com/uptimemesh/shared v0.0.0

require (
	github.com/blues/jsonata-go v1.5.4 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/uptimemesh/shared => ../shared
