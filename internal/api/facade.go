// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

// MethodFacadeOpen switches an authenticated connection to the Debug Adapter
// Protocol, joined to one session as one client (docs/adr/0012). Once its
// result line is written the connection speaks DAP (Content-Length framing)
// in both directions for the rest of its life: there is no way back to
// JSON-RPC. An error leaves the connection speaking JSON-RPC.
const MethodFacadeOpen = "facade.open"

// FacadeVersion is the version of the DAP facade's rules (what it handles,
// forwards and refuses); [FacadeOpenResult] reports it. Version 2 adds
// presence, the eyedbg/* custom messages and shared breakpoints
// (docs/adr/0014); version 3 adds launch connections ([FacadeLaunch]): a
// DAP launch that starts the session.
const FacadeVersion = 3

// FacadeOpenParams are the params of [MethodFacadeOpen]: the session to
// join and the client every DAP request of the connection acts as. With
// Launch the connection joins no session: its DAP launch starts one (the
// session id must then be empty).
type FacadeOpenParams struct {
	SessionRef

	Launch *FacadeLaunch `json:"launch,omitempty"`
}

// FacadeLaunch is what a launch connection's DAP launch starts with besides
// its arguments: what 'eyedbg start' adds from the caller's environment.
// ClientDir is the caller's working directory (absolute): relative paths in
// the launch arguments resolve against it. VirtualEnv is the caller's
// $VIRTUAL_ENV, NoRecord its EYEDBG_NO_RECORD=1, DotnetAdapter its
// EYEDBG_DOTNET_ADAPTER.
type FacadeLaunch struct {
	ClientDir     string `json:"clientDir"`
	VirtualEnv    string `json:"virtualEnv,omitempty"`
	NoRecord      bool   `json:"noRecord,omitempty"`
	DotnetAdapter string `json:"dotnetAdapter,omitempty"`
}

// FacadeOpenResult is the result of [MethodFacadeOpen]. After it, the
// connection speaks DAP. SessionID is empty for a launch connection.
type FacadeOpenResult struct {
	SessionID     string `json:"sessionId"`
	FacadeVersion int    `json:"facadeVersion"`
}
