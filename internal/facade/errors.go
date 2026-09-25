// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"errors"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// errorID is the fixed DAP error message id of a code (append-only):
// clients match variables.code, never the id or the text.
func errorID(code api.Code) int {
	switch code {
	case api.CodeInvalidRequest:
		return 7001
	case api.CodeNoSession:
		return 7002
	case api.CodeNotStopped:
		return 7003
	case api.CodeNotRunning:
		return 7004
	case api.CodeSessionExited:
		return 7005
	case api.CodeLeaseHeld:
		return 7006
	case api.CodeNotOwner:
		return 7007
	case api.CodeUnsupported:
		return 7008
	case api.CodeSideEffects:
		return 7009
	case api.CodeAdapterFailed:
		return 7010
	case api.CodeInternal:
		return 7099
	case api.CodeUnknownMethod, api.CodeUnauthorized, api.CodeDaemonNotRunning, api.CodeDaemonStart,
		api.CodeVersionMismatch, api.CodeSessionsActive, api.CodeAdapterMissing, api.CodeBuildFailed,
		api.CodeAttachFailed, api.CodeNoTestHost, api.CodeAnchorNotFound, api.CodeAnchorAmbiguous,
		// Side-helper codes ('eyedbg dotnet'): never a facade answer.
		api.CodeHelperNotFound, api.CodeHelperMismatch, api.CodeHelperFailed, api.CodeNotDotnet,
		api.CodeDiagnosticsDisabled, api.CodeDiagnosticsTimeout, api.CodeDumpUnsupported,
		api.CodeDumpRuntimeMissing, api.CodeDumpFailed:
		return 7000
	default:
		return 7000
	}
}

// internalError is what an error that isn't an *api.Error becomes: its
// text stays in the daemon.
func internalError(command string) *api.Error {
	return api.NewError(api.CodeInternal, "eyedbg failed to handle "+command, "see 'eyedbg daemon logs'")
}

// errorResponse turns err into a DAP error response's message (the stable
// code) and body: format is the message and hint, variables carry the code
// and, for LEASE_HELD, the lease holder. showUser asks the client to show
// it (execution requests).
func errorResponse(command string, err error, holder string, showUser bool) (message string, body *godap.ErrorMessage) {
	e, ok := errors.AsType[*api.Error](err)
	if !ok {
		e = internalError(command)
	}

	format := e.Message
	if e.Hint != "" {
		format += " — " + e.Hint
	}

	vars := map[string]string{"code": string(e.Code)}
	if e.Code == api.CodeLeaseHeld && holder != "" {
		vars["holder"] = holder
	}

	return string(e.Code), &godap.ErrorMessage{Id: errorID(e.Code), Format: format, Variables: vars, ShowUser: showUser}
}
