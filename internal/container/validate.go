// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Limits of the names that reach a docker invocation.
const (
	maxRefLen     = 128
	maxFullIDLen  = 64
	minShortIDLen = 12
	maxContextLen = 128
	maxHostLen    = 1024
	maxServiceLen = 63
	maxVersionLen = 64
)

// ValidateRef checks a container reference: an id (12 to 64 lowercase hex
// digits) or a name (a letter or digit, then letters, digits, '_', '.' and
// '-', at most 128 characters). A leading '-' can't pass.
func ValidateRef(ref string) error {
	if isHexID(ref) || (len(ref) <= maxRefLen && grammar(ref, isAlnum, isNameChar)) {
		return nil
	}

	return invalid("container", ref, "a container id (12 to 64 hex digits) or a name of letters, digits, '_', '.' and '-' starting with a letter or digit")
}

// ValidateContext checks a docker context name: a letter or digit, then
// letters, digits, '_', '.', '+' and '-', at most 128 characters.
func ValidateContext(name string) error {
	if len(name) <= maxContextLen && grammar(name, isAlnum, func(r byte) bool { return isNameChar(r) || r == '+' }) {
		return nil
	}

	return invalid("docker context", name, "letters, digits, '_', '.', '+' and '-' starting with a letter or digit")
}

// ValidateHost checks a docker host (a DOCKER_HOST value): at most 1024
// bytes, a URL ("://"), no whitespace or control characters.
func ValidateHost(host string) error {
	ok := len(host) <= maxHostLen && strings.Contains(host, "://") && !strings.HasPrefix(host, "-")

	for i := 0; ok && i < len(host); i++ {
		ok = host[i] > ' ' && host[i] != 0x7f
	}

	if !ok {
		return invalid("docker host", host, "a URL such as unix:///var/run/docker.sock or tcp://host:2376, without spaces")
	}

	return nil
}

// ValidateService checks a compose service name: a letter or digit, then
// letters, digits, '_', '.' and '-', at most 63 characters.
func ValidateService(name string) error {
	if len(name) <= maxServiceLen && grammar(name, isAlnum, isNameChar) {
		return nil
	}

	return invalid("service", name, "letters, digits, '_', '.' and '-' starting with a letter or digit")
}

// ValidateSpec checks every name of spec that could reach a docker
// invocation or a result: the engine, the container reference, the compose
// names and the process id.
func ValidateSpec(spec *api.ContainerSpec) error {
	if err := ValidateRef(spec.Ref); err != nil {
		return err
	}

	if spec.Engine.Host != "" {
		if err := ValidateHost(spec.Engine.Host); err != nil {
			return err
		}
	}

	if spec.Engine.Context != "" {
		if err := ValidateContext(spec.Engine.Context); err != nil {
			return err
		}
	}

	if spec.Service != "" {
		if err := ValidateService(spec.Service); err != nil {
			return err
		}
	}

	if spec.Project != "" {
		if err := api.CheckGroup(spec.Project); err != nil {
			return err
		}
	}

	if spec.PID < 0 {
		return api.NewError(api.CodeInvalidRequest, "invalid process id", "pass --pid N from 1")
	}

	return nil
}

// validVersion reports whether v can name the adapter's directory in a
// container.
func validVersion(v string) bool {
	return len(v) <= maxVersionLen && grammar(v, isAlnum, isNameChar)
}

// isHexID reports whether s is a container id: 12 to 64 lowercase hex digits.
func isHexID(s string) bool {
	if len(s) < minShortIDLen || len(s) > maxFullIDLen {
		return false
	}

	for i := range len(s) {
		if !isHex(s[i]) {
			return false
		}
	}

	return true
}

// isFullID reports whether s is a full container id: 64 lowercase hex digits.
func isFullID(s string) bool { return len(s) == maxFullIDLen && isHexID(s) }

// grammar reports whether s is non-empty, starts with a byte first accepts
// and goes on with bytes rest accepts.
func grammar(s string, first, rest func(byte) bool) bool {
	if s == "" || !first(s[0]) {
		return false
	}

	for i := 1; i < len(s); i++ {
		if !rest(s[i]) {
			return false
		}
	}

	return true
}

func isHex(b byte) bool { return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') }

func isAlnum(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isNameChar(b byte) bool { return isAlnum(b) || b == '_' || b == '.' || b == '-' }

// invalid is the INVALID_REQUEST for a name that failed its grammar; the
// value is shown cut and without control characters.
func invalid(what, value, want string) error {
	return api.NewError(api.CodeInvalidRequest, "invalid "+what+" "+show(value, 40), "use "+want)
}

// show is s quoted for a message: cut to n bytes, control characters shown
// as spaces, invalid UTF-8 replaced.
func show(s string, n int) string {
	if len(s) > n {
		s = s[:n] + "..."
	}

	return `"` + strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(s, "?")) + `"`
}
