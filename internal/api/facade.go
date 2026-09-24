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
// forwards and refuses); [FacadeOpenResult] reports it.
const FacadeVersion = 1

// FacadeOpenParams are the params of [MethodFacadeOpen]: the session to
// join and the client every DAP request of the connection acts as.
type FacadeOpenParams struct {
	SessionRef
}

// FacadeOpenResult is the result of [MethodFacadeOpen]. After it, the
// connection speaks DAP.
type FacadeOpenResult struct {
	SessionID     string `json:"sessionId"`
	FacadeVersion int    `json:"facadeVersion"`
}
