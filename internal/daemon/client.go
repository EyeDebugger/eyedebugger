// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// Client is an authenticated connection to the daemon. It is not safe for
// concurrent use.
type Client struct {
	conn   net.Conn
	c      *api.Conn
	nextID int64

	// Hello is the daemon's answer to the handshake.
	Hello api.HelloResult
}

// Dial connects to a running daemon and authenticates. It never starts one:
// if none is running it returns an *api.Error with [api.CodeDaemonNotRunning].
func Dial(ctx context.Context, p Paths, info version.Info) (*Client, error) {
	token, err := os.ReadFile(p.Token)
	if err != nil {
		return nil, notRunning(err)
	}

	var d net.Dialer

	conn, err := d.DialContext(ctx, "unix", p.Socket)
	if err != nil {
		return nil, notRunning(err)
	}

	cl := &Client{conn: conn, c: api.NewConn(conn, conn)}

	err = cl.Call(ctx, api.MethodHello, api.HelloParams{
		Token:           string(token),
		ProtocolVersion: api.ProtocolVersion,
		ClientVersion:   info.Version,
	}, &cl.Hello)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	return cl, nil
}

// Call sends one request and decodes its result into result (nil to discard).
// Daemon-side failures are returned as *api.Error.
func (cl *Client) Call(ctx context.Context, method string, params, result any) error {
	stop := context.AfterFunc(ctx, func() { _ = cl.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if deadline, ok := ctx.Deadline(); ok {
		_ = cl.conn.SetDeadline(deadline)
	}

	cl.nextID++

	req, err := api.NewRequest(cl.nextID, method, params)
	if err != nil {
		return err
	}

	if err := cl.c.Write(req); err != nil {
		return cl.callError(ctx, method, err)
	}

	var resp api.Response
	if err := cl.c.Read(&resp); err != nil {
		return cl.callError(ctx, method, err)
	}

	if resp.ID != req.ID && resp.Error == nil {
		return fmt.Errorf("%s: response id %d does not match request id %d", method, resp.ID, req.ID)
	}

	return resp.Decode(result)
}

// Close closes the connection.
func (cl *Client) Close() error {
	if err := cl.conn.Close(); err != nil {
		return fmt.Errorf("close daemon connection: %w", err)
	}

	return nil
}

func (cl *Client) callError(ctx context.Context, method string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", method, ctxErr)
	}

	return fmt.Errorf("%s: %w", method, err)
}

func notRunning(cause error) error {
	return errors.Join(api.NewError(api.CodeDaemonNotRunning, "the eyedbg daemon is not running",
		"run 'eyedbg daemon start', or any command that needs it, to start it"), cause)
}
