// Package mem provides memory-size parsing and host-memory measurement for
// admission control (spec 1 §8.1).
//
// Split out of broker because gen_tenant_env validates capacity.mem with the
// same parser — a second copy would be a second definition of what "6GiB"
// means, and the two would drift.
package mem

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ErrNoMemAvailable is returned (wrapped) when the meminfo file was read but has
// no MemAvailable line, so callers can tell "absent" from "unreadable".
var ErrNoMemAvailable = errors.New("no MemAvailable line")

var unitBytes = map[string]uint64{
	"B":   1,
	"KIB": 1 << 10,
	"MIB": 1 << 20,
	"GIB": 1 << 30,
	"TIB": 1 << 40,
	"KB":  1e3,
	"MB":  1e6,
	"GB":  1e9,
	"TB":  1e12,
}

// maxBytes is the first refused size, 2^63 (8 EiB). Python's unbounded ints
// never overflow, but a uint64 conversion out of range is implementation-
// defined (MaxUint64 on arm64, 2^63 on amd64), and an admission ceiling that
// wrapped would admit every launch. Stopping at 2^63 rather than 2^64 also
// guarantees the sum of any two parsed sizes (need + floor) still fits.
const maxBytes = 1 << 63

var sizeRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([A-Za-z]*)$`)

// ParseMemBytes converts "4GiB" to 4294967296. It returns an error on
// anything else, including a size of 2^63 bytes or more.
//
// Digits and whitespace are ASCII-only, where Python's \d and \s also match
// e.g. Arabic-Indic digits and NBSP. That is deliberate hardening: such input
// is rejected (fail closed) rather than read.
//
// SI units are accepted as well as IEC because a hand-written tenants.yml may
// use them, and silently reading "6GB" as 6GiB would overstate a ceiling by
// ~7%. A bare number is bytes. Negative and unit-only strings are rejected
// rather than coerced — a ceiling that parsed as 0 would make try_reserve
// admit everything.
func ParseMemBytes(value string) (uint64, error) {
	s := strings.TrimSpace(value)
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("not a memory size: %q", value)
	}
	unit := strings.ToUpper(m[2])
	if unit == "" {
		unit = "B"
	}
	mult, ok := unitBytes[unit]
	if !ok {
		return 0, fmt.Errorf("unknown memory unit %q in %q", m[2], value)
	}
	number, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("not a memory size: %q", value)
	}
	// Compare in float space: converting first would be the very overflow this
	// guards against. NaN cannot come from the regex but is refused regardless.
	f := number * float64(mult)
	if math.IsNaN(f) || math.IsInf(f, 0) || f >= maxBytes {
		return 0, fmt.Errorf("memory size out of range: %q", value)
	}
	return uint64(f), nil
}

// AvailableBytes reports host memory available right now, from MemAvailable in
// /proc/meminfo.
//
// MemAvailable, never MemFree and never free's "used" (spec 0 §7.4): on metal
// `free` reports ~38 GiB used against ~21.4 GiB of actual VM RSS. The
// difference is page cache the kernel hands back under pressure, so "used"
// would refuse launches that are perfectly safe.
//
// It returns an error rather than a sentinel: callers must treat "we could not
// measure" as a refusal, not as unlimited headroom.
func AvailableBytes(meminfo string) (uint64, error) {
	content, err := os.ReadFile(meminfo)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("%s: malformed MemAvailable line", meminfo)
			}
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%s: malformed MemAvailable line: %w", meminfo, err)
			}
			if kb > math.MaxUint64/1024 {
				return 0, fmt.Errorf("%s: MemAvailable %d kB overflows", meminfo, kb)
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("%s has %w", meminfo, ErrNoMemAvailable)
}
