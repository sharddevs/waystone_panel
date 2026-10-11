// SPDX-License-Identifier: AGPL-3.0-only

// Package minecraft is the Minecraft Java profile: everything Waystone knows
// about this game's files. It runs in the agent, unprivileged, and reaches
// the server's directory only through the game-neutral worker (Files). The
// helper and the worker never learn what these files mean.
//
// Every file it reads came from the server directory, which the game server
// itself can change, so its content is untrusted input.
package minecraft

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrInvalid means a value Waystone was asked to set failed validation.
	ErrInvalid = errors.New("invalid minecraft setting")
	// ErrUnsupported means an existing file has content the profile refuses
	// to interpret (an escaped or continued managed key). Nothing was
	// changed.
	ErrUnsupported = errors.New("unsupported file content")
)

// Properties are the server.properties values Waystone manages.
type Properties struct {
	ServerPort uint32
	Motd       string
	MaxPlayers uint32
}

// Limits for Properties.
const (
	MinServerPort = 1024
	MaxServerPort = 65535
	MaxMotdBytes  = 64
	MinMaxPlayers = 1
	MaxMaxPlayers = 1000
)

// Validate checks p. Errors wrap ErrInvalid and say which field is wrong.
//   - ServerPort from MinServerPort to MaxServerPort.
//   - MaxPlayers from MinMaxPlayers to MaxMaxPlayers.
//   - Motd: at most MaxMotdBytes bytes; every byte printable ASCII
//     (0x20 to 0x7e) except backslash; no leading or trailing space.
//     An empty motd is allowed. These rules mean the value never needs
//     escaping in a properties file.
func (p Properties) Validate() error {
	if p.ServerPort < MinServerPort || p.ServerPort > MaxServerPort {
		return fmt.Errorf("%w: server-port must be between %d and %d",
			ErrInvalid, MinServerPort, MaxServerPort)
	}

	if p.MaxPlayers < MinMaxPlayers || p.MaxPlayers > MaxMaxPlayers {
		return fmt.Errorf("%w: max-players must be between %d and %d",
			ErrInvalid, MinMaxPlayers, MaxMaxPlayers)
	}

	if len(p.Motd) > MaxMotdBytes {
		return fmt.Errorf("%w: motd exceeds %d bytes",
			ErrInvalid, MaxMotdBytes)
	}

	if strings.TrimSpace(p.Motd) != p.Motd {
		return fmt.Errorf("%w: motd has leading or trailing whitespace",
			ErrInvalid)
	}

	for i := 0; i < len(p.Motd); i++ {
		b := p.Motd[i]

		if b < 0x20 || b > 0x7e || b == '\\' {
			return fmt.Errorf("%w: motd contains an invalid character at byte %d",
				ErrInvalid, i)
		}
	}

	return nil
}

// The managed keys, in the order MergeProperties appends missing ones.
// online-mode is always written as "true".
const (
	KeyServerPort = "server-port"
	KeyMotd       = "motd"
	KeyMaxPlayers = "max-players"
	KeyOnlineMode = "online-mode"
)

// managedValues returns the authoritative values for Waystone-managed keys.
func managedValues(p Properties) map[string]string {
	return map[string]string{
		KeyServerPort: strconv.FormatUint(uint64(p.ServerPort), 10),
		KeyMotd:       p.Motd,
		KeyMaxPlayers: strconv.FormatUint(uint64(p.MaxPlayers), 10),
		KeyOnlineMode: "true",
	}
}

// continuesLine reports whether a Java properties line continues
// onto the next line. An odd number of trailing backslashes means
// continuation. A trailing carriage return is ignored.
func continuesLine(line string) bool {
	line = strings.TrimSuffix(line, "\r")

	count := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		count++
	}

	return count%2 == 1
}

// propertyKey extracts a simplified Java properties key.
// Blank lines and comments have no key.
func propertyKey(line string) string {
	line = strings.TrimLeft(line, " \t\f")

	if line == "" || line[0] == '#' || line[0] == '!' {
		return ""
	}

	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '=', ':', ' ', '\t', '\f':
			return line[:i]
		}
	}

	return line
}

// MergeProperties returns the new content of server.properties: existing
// (nil when the file does not exist yet) with every managed key set to
// exactly one authoritative value. p has already passed Validate.
//
// Lines: split existing on "\n". Empty or nil existing has no lines, and a
// final "\n" does not make an extra empty line. Every line not described
// below is copied unchanged, in order.
//
// Which lines are key lines (Java properties rules, simplified):
//   - A line continues onto the next one if it ends with an odd number of
//     backslashes (ignore one trailing "\r" when counting). The lines it
//     continues onto are part of its value: copy them unchanged and never
//     treat them as key lines.
//   - Otherwise, skip leading spaces, tabs and form feeds. A line that is
//     then empty, or starts with '#' or '!', is a blank or comment line.
//     Comment lines never continue.
//   - Otherwise the key is everything up to the first '=', ':', space, tab
//     or form feed.
//
// Managed keys:
//   - The first line with a managed key is replaced, in place, by
//     "<key>=<value>". Every later line with the same key is removed.
//   - Managed keys that never appeared are appended, in the order of the
//     constants above, after all existing lines.
//   - Values: server-port and max-players as decimal numbers, motd as it
//     is, online-mode as "true" whatever the file said.
//
// Refuse, with an error wrapping ErrUnsupported and no output, when:
//   - a key contains a backslash (Java would unescape it, so "server\-port"
//     may be a managed key in disguise), or
//   - a managed key's line continues onto the next line.
//
// Output: the lines joined with "\n", ending with exactly one "\n".
func MergeProperties(existing []byte, p Properties) ([]byte, error) {
	values := managedValues(p)

	order := []string{
		KeyServerPort,
		KeyMotd,
		KeyMaxPlayers,
		KeyOnlineMode,
	}

	seen := make(map[string]bool)
	var output []string

	text := string(existing)
	if text != "" {
		text = strings.TrimSuffix(text, "\n")
		lines := strings.Split(text, "\n")

		continuation := false

		for _, line := range lines {
			if continuation {
				output = append(output, line)
				continuation = continuesLine(line)
				continue
			}

			key := propertyKey(line)

			if strings.Contains(key, "\\") {
				return nil, fmt.Errorf(
					"%w: escaped property key %q",
					ErrUnsupported, key,
				)
			}

			value, managed := values[key]

			if managed {
				if continuesLine(line) {
					return nil, fmt.Errorf(
						"%w: continued managed key %q",
						ErrUnsupported, key,
					)
				}

				if !seen[key] {
					output = append(output, key+"="+value)
					seen[key] = true
				}

				// Duplicate managed keys are deliberately removed.
				continue
			}

			output = append(output, line)

			// Unmanaged properties may have continued values.
			// Their following lines must not be parsed as keys.
			if key != "" {
				continuation = continuesLine(line)
			}
		}
	}

	for _, key := range order {
		if !seen[key] {
			output = append(output, key+"="+values[key])
		}
	}

	return []byte(strings.Join(output, "\n") + "\n"), nil
}
