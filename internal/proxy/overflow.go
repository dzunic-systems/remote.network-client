package proxy

import "strings"

// A prompt past a node's context window comes back as a pre-token
// inference.error. It is not final: a node with a wider window may fit
// it. The router owns that choice (it knows every card's window and
// fails over by width), so the client hands an overflowed request to the
// router at once rather than retrying the desk's remaining candidates
// blind.

// codeContextLengthExceeded is the router's code for a window overflow —
// the router (private side) carries the twin of this detector; keep the
// two in step, and move them to a shared home when one exists.
const codeContextLengthExceeded = "context_length_exceeded"

// overflowError reports whether a pre-token refusal is a window overflow.
func overflowError(code, msg string) bool {
	if code == codeContextLengthExceeded {
		return true
	}
	return strings.Contains(msg, "maximum context length") || // vLLM, OpenAI-compatible
		strings.Contains(msg, "exceeds the available context size") || // llama.cpp
		strings.Contains(msg, "prompt is too long") // Anthropic-compatible
}
