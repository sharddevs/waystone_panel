// SPDX-License-Identifier: AGPL-3.0-only

// Package minecraft knows about Minecraft Java server files: it validates
// and merges server.properties and writes eula.txt. It runs in the
// unprivileged agent and only touches the server directory through the
// game-neutral worker. Anything it reads from there is untrusted, since the
// game server can change it.
package minecraft

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrInvalid means a value we were asked to set failed validation.
	ErrInvalid = errors.New("invalid minecraft setting")
	// ErrUnsupported means an existing file has something we won't try to
	// interpret, like an escaped key or a managed key whose line continues.
	// Nothing was changed.
	ErrUnsupported = errors.New("unsupported file content")
)

// Properties holds the server.properties values Waystone manages.
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

// Validate checks that every field of p is in range. Errors wrap ErrInvalid
// and name the field.
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

	// Printable ASCII without backslash means the motd never needs escaping
	// in a properties file.
	for i := 0; i < len(p.Motd); i++ {
		b := p.Motd[i]

		if b < 0x20 || b > 0x7e || b == '\\' {
			return fmt.Errorf("%w: motd contains an invalid character at byte %d",
				ErrInvalid, i)
		}
	}

	return nil
}

// The keys Waystone manages, in the order MergeProperties appends missing
// ones. online-mode is always written as "true". The rest are fixed too
// (6H5): the game listens on every address, and its remote consoles and
// management ports stay off, since nothing in Waystone guards them.
const (
	KeyServerPort       = "server-port"
	KeyMotd             = "motd"
	KeyMaxPlayers       = "max-players"
	KeyOnlineMode       = "online-mode"
	KeyServerIP         = "server-ip"
	KeyEnableRcon       = "enable-rcon"
	KeyEnableQuery      = "enable-query"
	KeyManagementServer = "management-server-enabled"
	KeyJMXMonitoring    = "enable-jmx-monitoring"
)

var managedOrder = []string{
	KeyServerPort, KeyMotd, KeyMaxPlayers, KeyOnlineMode,
	KeyServerIP, KeyEnableRcon, KeyEnableQuery, KeyManagementServer, KeyJMXMonitoring,
}

func managedValues(p Properties) map[string]string {
	return map[string]string{
		KeyServerPort:       strconv.FormatUint(uint64(p.ServerPort), 10),
		KeyMotd:             p.Motd,
		KeyMaxPlayers:       strconv.FormatUint(uint64(p.MaxPlayers), 10),
		KeyOnlineMode:       "true",
		KeyServerIP:         "",
		KeyEnableRcon:       "false",
		KeyEnableQuery:      "false",
		KeyManagementServer: "false",
		KeyJMXMonitoring:    "false",
	}
}

// continuesLine reports whether a properties line continues onto the next
// one. In Java's format that's an odd number of trailing backslashes (an
// even number is just escaped backslashes). A trailing "\r" is ignored.
func continuesLine(line string) bool {
	line = strings.TrimSuffix(line, "\r")

	count := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		count++
	}

	return count%2 == 1
}

// propertyKey returns the key of a properties line, using a simplified
// version of Java's rules. Blank lines and comments return "".
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

// MergeProperties returns the new server.properties: existing (nil if there
// is no file yet) with each managed key set once to its value from p. p
// should already have passed Validate.
//
// The first line for a managed key is rewritten in place and any later
// duplicates are dropped. Keys that never showed up are appended at the end.
// Every other line, including comments and continued values, is kept as is.
//
// A key with a backslash is refused with ErrUnsupported, because Java would
// unescape it and "server\-port" could be a managed key in disguise. So is a
// managed key whose line continues onto the next.
func MergeProperties(existing []byte, p Properties) ([]byte, error) {
	values := managedValues(p)

	seen := make(map[string]bool)
	var output []string

	text := string(existing)
	if text != "" {
		// Without this a final newline would give us an extra empty line.
		text = strings.TrimSuffix(text, "\n")
		lines := strings.Split(text, "\n")

		continuation := false

		for _, line := range lines {
			// Lines that continue a value belong to it, so they're never
			// treated as key lines.
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

				// Later duplicates of a managed key are dropped.
				continue
			}

			output = append(output, line)

			// Comment lines never continue, but any other nonblank line can,
			// even one with an empty key.
			trimmed := strings.TrimLeft(line, " \t\f")
			if trimmed != "" && trimmed[0] != '#' && trimmed[0] != '!' {
				continuation = continuesLine(line)
			}
		}
	}

	for _, key := range managedOrder {
		if !seen[key] {
			output = append(output, key+"="+values[key])
		}
	}

	return []byte(strings.Join(output, "\n") + "\n"), nil
}
