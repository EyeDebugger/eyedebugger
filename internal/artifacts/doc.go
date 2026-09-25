// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package artifacts keeps the files eyedbg makes from a program's memory —
// dumps now — in a private directory: <home>/dumps, where <home> is
// $EYEDBG_HOME or ~/.eyedbg (adapters.HomeDir). The directory is created
// 0700, must be a real directory (not a symlink) and, on Unix, owned by the
// user; its files are 0600 (docs/adr/0016, P2-M7). On Windows the user
// profile's ACL is the boundary, as for the runtime directory.
//
// Dumps get fresh names eyedbg chose (Name), are pruned after MaxAge and
// beyond the newest Keep (Prune: eyedbg's names and regular files only),
// and are placed at a user's --out path without ever replacing anything
// there (Place).
package artifacts
