// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

const leaseLong = `Show or change who holds a session's control lease (docs/DESIGN.md §3). The lease decides which
client may change the program's execution: continue, next, step-in, step-out, pause, run-until,
and stop while the program is live. Reading (status, vars, eval, output, events, wait) and your
own breakpoints need no lease. Who you are is --as, else $EYEDBG_CLIENT, else agent.

The client that starts a session holds its lease first. The policy (set by 'eyedbg start
--lease-policy' or 'eyedbg lease policy') decides whether another client can take it:
  free            anyone takes it by running an execution command (the default);
  handoff         only its holder can give it away (release or grant);
  human-priority  like handoff, but a human may take it from an agent (not from another human),
                  and agents never take it from one another.
Refusals are LEASE_HELD (exit 2), naming the holder. 'eyedbg lease request' asks the holder for it
politely; 'eyedbg events --wait --kind lease' waits for the lease to change. A human's lease is
released when their editor's last connection to the session ('eyedbg dap') closes.

Without a subcommand, prints the lease, like 'eyedbg lease status'.`

const leaseExample = `  eyedbg lease                          # who holds it, under which policy
  eyedbg --as human:ijat lease take     # take it (if the policy allows)
  eyedbg lease grant human:ijat         # give it to someone
  eyedbg lease request --message "why"  # ask its holder for it
  eyedbg lease policy handoff           # only hand it over explicitly from now on`

// leaseOutputHelp is the output and exit-code part of every lease command's
// help.
const leaseOutputHelp = `

Returns at once and never affects the program. Output: "session ID: lease held by CLIENT (policy
P)" (CLIENT (connected, policy P) while its editor is connected) or "session ID: nobody holds the
lease (policy P)", then a 'requested by CLIENT: "MESSAGE"' line per pending request; {"schema",
"sessionId", "lease": {"policy", "holder", "since", "requests"}, "holderConnected"} in --json.
Exits 2 for LEASE_HELD or NO_SESSION.`

// leaseSpec describes one lease subcommand.
type leaseSpec struct {
	use, short, long, example, method string
	// args is how many arguments it takes (a client or a policy).
	args int
	// force adds --force; message adds --message.
	force, message bool
	// params builds the request from the session, the argument and --force
	// (or --message).
	params func(ref api.SessionRef, arg string, force bool, message string) any
}

func leaseSpecs() []leaseSpec {
	plain := func(ref api.SessionRef, _ string, force bool, _ string) any {
		return api.LeaseParams{SessionRef: ref, Force: force}
	}

	return []leaseSpec{
		{
			use: statusUse, short: "Show who holds the lease", method: api.MethodLeaseStatus, params: plain,
			long:    `Show who holds the session's control lease, since when (--json), and the lease policy.`,
			example: "  eyedbg lease status\n  eyedbg lease status --json",
		},
		{
			use: "take", short: "Take the lease", method: api.MethodLeaseTake, force: true, params: plain,
			long: `Take the lease, if the policy lets you take it from its holder (free: always; human-priority: a
human from an agent). Taking it when nobody holds it, or when you do, always works.

--force takes it whatever the policy. Use it only when the holder is gone for good (e.g. an agent
that crashed while holding it): it overrides a policy someone chose on purpose.`,
			example: "  eyedbg --as human:ijat lease take",
		},
		{
			use: "release", short: "Let go of the lease", method: api.MethodLeaseRelease, params: plain,
			long: `Let go of the lease if you hold it, so that nobody does: the next client to run an execution
command (or 'eyedbg lease take') gets it, whatever the policy. Does nothing if you don't hold it.`,
			example: "  eyedbg lease release",
		},
		{
			use: "grant <client>", short: "Give the lease to another client", method: api.MethodLeaseGrant, args: 1, force: true,
			params: func(ref api.SessionRef, to string, force bool, _ string) any {
				return api.LeaseGrantParams{SessionRef: ref, To: to, Force: force}
			},
			long: `Give the lease to CLIENT (agent, human, agent:NAME or human:NAME). Only its holder can, or anyone
while nobody holds it; --force lets anyone, whatever the policy.`,
			example: "  eyedbg lease grant human:ijat\n  eyedbg --as human:ijat lease grant agent:claude",
		},
		{
			use: "policy <free|handoff|human-priority>", short: "Set the lease policy", method: api.MethodLeasePolicy, args: 1, force: true,
			params: func(ref api.SessionRef, policy string, force bool, _ string) any {
				return api.LeasePolicyParams{SessionRef: ref, Policy: api.LeasePolicy(policy), Force: force}
			},
			long: `Set the session's lease policy (see 'eyedbg lease --help'). Only the lease's holder can, or
anyone while nobody holds it; --force lets anyone.`,
			example: "  eyedbg lease policy handoff\n  eyedbg lease policy free",
		},
		{
			use: "request", short: "Ask the lease's holder for it", method: api.MethodLeaseRequest, message: true,
			params: func(ref api.SessionRef, _ string, _ bool, message string) any {
				return api.LeaseRequestParams{SessionRef: ref, Message: message}
			},
			long: `Ask the lease's holder for it. It changes nothing else, under every policy: the request shows as
"requested by CLIENT" in 'eyedbg status', in every stop of every client and in 'eyedbg lease
status' until the lease changes hands, and as a lease event ('eyedbg events --kind lease'). The
holder decides with 'eyedbg lease grant CLIENT' or 'eyedbg lease release'. One request per
client: asking again replaces yours. --message (at most 200 characters) tells the holder why.
Asking while you hold the lease, or while nobody does, records nothing.`,
			example: `  eyedbg --as human:ijat lease request --message "let me step through parse()"
  eyedbg lease request`,
		},
	}
}

func newLeaseCommand(info version.Info, g *globals) *cobra.Command {
	specs := leaseSpecs()

	cmd := &cobra.Command{
		Use:     "lease",
		Short:   "Show or change who may drive a session's execution",
		Long:    leaseLong + leaseOutputHelp + sessionHelp,
		Example: leaseExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLease(cmd, info, g, specs[0], "", false, "")
		},
	}

	for _, spec := range specs {
		cmd.AddCommand(newLeaseSubcommand(info, g, spec))
	}

	return cmd
}

func newLeaseSubcommand(info version.Info, g *globals, spec leaseSpec) *cobra.Command {
	var (
		force   bool
		message string
	)

	cmd := &cobra.Command{
		Use:     spec.use,
		Short:   spec.short,
		Long:    spec.long + leaseOutputHelp + sessionHelp,
		Example: spec.example,
		Args:    cobra.ExactArgs(spec.args),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := ""
			if len(args) > 0 {
				arg = args[0]
			}

			if spec.method == api.MethodLeasePolicy {
				if _, err := api.ParseLeasePolicy(arg); err != nil || arg == "" {
					return fmt.Errorf("invalid lease policy %q: want free, handoff or human-priority", arg)
				}
			}

			return runLease(cmd, info, g, spec, arg, force, message)
		},
	}

	if spec.force {
		cmd.Flags().BoolVar(&force, "force", false, "whatever the lease policy (see this command's help)")
	}

	if spec.message {
		cmd.Flags().StringVar(&message, "message", "", "why you ask, for the holder (at most 200 characters)")
	}

	return cmd
}

func runLease(cmd *cobra.Command, info version.Info, g *globals, spec leaseSpec, arg string, force bool, message string) error {
	ref := g.ref()

	var res api.LeaseResult
	if err := call(cmd, info, daemonCallTimeout, spec.method, spec.params(ref, arg, force, message), &res); err != nil {
		return err
	}

	if spec.method == api.MethodLeaseRequest && !g.json {
		me, _ := api.ParseClient(ref.Client) // the daemon accepted it

		return writeLeaseRequest(cmd.OutOrStdout(), res, me.ID)
	}

	return writeLease(cmd.OutOrStdout(), res, g.json)
}

// writeLeaseRequest writes the text result of 'lease request' by client
// me: as writeLease, except when there was nobody to ask.
func writeLeaseRequest(w io.Writer, res api.LeaseResult, me string) error {
	switch res.Lease.Holder {
	case "":
		return writeText(w, fmt.Sprintf("session %s: nobody holds the lease (policy %s): 'eyedbg lease take' takes it\n",
			res.SessionID, res.Lease.Policy))
	case me:
		return writeText(w, fmt.Sprintf("session %s: you hold the lease (policy %s)\n", res.SessionID, res.Lease.Policy))
	default:
		return writeLease(w, res, false)
	}
}

func writeLease(w io.Writer, res api.LeaseResult, asJSON bool) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema          int `json:"schema"`
			api.LeaseResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
		}{jsonSchemaVersion, res})
	}

	var b strings.Builder

	switch {
	case res.Lease.Holder == "":
		fmt.Fprintf(&b, "session %s: nobody holds the lease (policy %s)\n", res.SessionID, res.Lease.Policy)
	case res.HolderConnected:
		fmt.Fprintf(&b, "session %s: lease held by %s (connected, policy %s)\n", res.SessionID, res.Lease.Holder, res.Lease.Policy)
	default:
		fmt.Fprintf(&b, "session %s: lease held by %s (policy %s)\n", res.SessionID, res.Lease.Holder, res.Lease.Policy)
	}

	for _, r := range res.Lease.Requests {
		b.WriteString("  requested by " + leaseRequestText(r) + "\n")
	}

	return writeText(w, b.String())
}

// leaseRequestText is a pending lease request on one line: the client,
// and its message quoted.
func leaseRequestText(r api.LeaseRequest) string {
	if r.Message == "" {
		return r.Client
	}

	return r.Client + ": " + quoteLine(r.Message)
}

// quoteLine quotes a client's text on one line, cut to the event line
// limit.
func quoteLine(s string) string {
	return strconv.Quote(cutLine(s))
}
