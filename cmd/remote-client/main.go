// Command remote-client is the remote.network consumer client: a local
// OpenAI-compatible endpoint that sends each request straight to the
// serving node over QUIC when it can, and through a router when it can't.
package main

import "remote.network/client/internal/cli"

func main() {
	cli.Execute()
}
