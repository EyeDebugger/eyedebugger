// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"time"
)

// ProtocolVersion is the version of the native API. Bump it on any
// incompatible change to a method other than [MethodHello], whose shape is
// frozen.
const ProtocolVersion = 1

// Method names.
const (
	// MethodHello authenticates a connection and exchanges versions. It must
	// be the first request on every connection.
	MethodHello = "hello"
	// MethodDaemonStatus reports the daemon's state.
	MethodDaemonStatus = "daemon.status"
	// MethodDaemonStop asks the daemon to exit.
	MethodDaemonStop = "daemon.stop"
)

// HelloParams are the params of [MethodHello].
type HelloParams struct {
	// Token is the contents of the per-user token file.
	Token string `json:"token"`
	// ProtocolVersion is the client's [ProtocolVersion].
	ProtocolVersion int `json:"protocolVersion"`
	// ClientVersion is the client binary's build version, for logs.
	ClientVersion string `json:"clientVersion"`
}

// HelloResult is the result of [MethodHello].
type HelloResult struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	PID             int    `json:"pid"`
	Sessions        int    `json:"sessions"`
}

// StatusResult is the result of [MethodDaemonStatus].
type StatusResult struct {
	PID         int       `json:"pid"`
	Version     string    `json:"version"`
	Commit      string    `json:"commit"`
	StartedAt   time.Time `json:"startedAt"`
	Sessions    int       `json:"sessions"`
	Dir         string    `json:"dir"`
	Socket      string    `json:"socket"`
	IdleTimeout Duration  `json:"idleTimeout"`
	// IdleExitAt is when the daemon exits if nothing changes; nil while
	// sessions exist or when the idle timeout is disabled.
	IdleExitAt *time.Time `json:"idleExitAt,omitempty"`
}

// StopParams are the params of [MethodDaemonStop].
type StopParams struct {
	// Force ends live sessions (killing their debuggees) instead of refusing.
	Force bool `json:"force"`
}

// StopResult is the result of [MethodDaemonStop].
type StopResult struct {
	// EndedSessions is how many sessions the stop ended.
	EndedSessions int `json:"endedSessions"`
}

// Duration is a time.Duration that encodes as a Go duration string ("30m0s").
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}

	*d = Duration(v)

	return nil
}
