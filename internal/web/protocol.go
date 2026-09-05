// Package web serves the browser control surface (MKSIP-1001).
//
// The browser controls this client; it does not carry the audio. Microphone
// and speakers stay on the machine running the client, so a warm transfer
// driven from the browser still needs the operator at that machine to talk to
// the consulted party. The UI says so on the page.
package web

import "sipclient/internal/control"

// Message types on the socket.
const (
	// Server to client.
	TypeState  = "state"
	TypeEvent  = "event"
	TypeResult = "result"
	// Client to server.
	TypeCommand = "command"
)

// StateMessage carries the full client state. It is sent on connect and after
// every change, so the browser never polls and never has to merge deltas.
type StateMessage struct {
	Type  string        `json:"type"`
	State control.State `json:"state"`
}

// EventMessage mirrors a notification the REPL prints above its prompt.
type EventMessage struct {
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Channel int    `json:"channel,omitempty"`
	Text    string `json:"text"`
}

// Command is a request from the browser. Every command carries a client
// generated ID and receives exactly one ResultMessage.
type Command struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Action string `json:"action"`

	Channel int    `json:"channel,omitempty"`
	Target  string `json:"target,omitempty"`
	Digits  string `json:"digits,omitempty"`
	Code    int    `json:"code,omitempty"`
	// Transferee and Consult name the legs of an attended transfer. Zero means
	// "infer it", matching the REPL's bare `xfer`.
	Transferee int  `json:"transferee,omitempty"`
	Consult    int  `json:"consult,omitempty"`
	All        bool `json:"all,omitempty"`
	On         bool `json:"on,omitempty"`
}

// ResultMessage reports the outcome of one command.
type ResultMessage struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	OK   bool   `json:"ok"`
	// Text is a human sentence for the UI to show on success.
	Text string `json:"text,omitempty"`
	// Error is the same message the REPL would print, which already names the
	// channel and its state (FR-9.6).
	Error string `json:"error,omitempty"`
}
