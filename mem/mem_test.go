package mem

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseMemBytesIECUnits(t *testing.T) {
	cases := map[string]uint64{
		"4GiB":   4 * 1024 * 1024 * 1024,
		"512MiB": 512 * 1024 * 1024,
		"2TiB":   2 * 1024 * 1024 * 1024 * 1024,
	}
	for in, want := range cases {
		got, err := ParseMemBytes(in)
		if err != nil {
			t.Fatalf("ParseMemBytes(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseMemBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

// Every entry of the unit table, so a swapped multiplier (KB as 1<<10, MiB as
// 1<<21) cannot survive.
func TestParseMemBytesEveryUnit(t *testing.T) {
	cases := map[string]uint64{
		"7B":   7,
		"3KiB": 3 * 1024,
		"3MiB": 3 * 1024 * 1024,
		"3GiB": 3 * 1024 * 1024 * 1024,
		"3TiB": 3 * 1024 * 1024 * 1024 * 1024,
		"3KB":  3_000,
		"3MB":  3_000_000,
		"3GB":  3_000_000_000,
		"3TB":  3_000_000_000_000,
	}
	for in, want := range cases {
		got, err := ParseMemBytes(in)
		if err != nil {
			t.Fatalf("ParseMemBytes(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseMemBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseMemBytesSIUnitsAreNotIEC(t *testing.T) {
	got, err := ParseMemBytes("6GB")
	if err != nil {
		t.Fatal(err)
	}
	if got != 6*1000*1000*1000 {
		t.Errorf("ParseMemBytes(6GB) = %d, want %d", got, 6*1000*1000*1000)
	}
	got, err = ParseMemBytes("6GiB")
	if err != nil {
		t.Fatal(err)
	}
	if got != 6*1024*1024*1024 {
		t.Errorf("ParseMemBytes(6GiB) = %d, want %d", got, 6*1024*1024*1024)
	}
}

func TestParseMemBytesBareNumberIsBytes(t *testing.T) {
	for in, want := range map[string]uint64{"1024": 1024, "4096": 4096} {
		got, err := ParseMemBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseMemBytes(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
}

func TestParseMemBytesCaseInsensitive(t *testing.T) {
	for _, in := range []string{"4gib", "4 GiB", "4GIB", "4Gib"} {
		got, err := ParseMemBytes(in)
		if err != nil || got != 4*1024*1024*1024 {
			t.Errorf("ParseMemBytes(%q) = %d, %v, want %d", in, got, err, 4*1024*1024*1024)
		}
	}
}

func TestParseMemBytesTrimsAndTruncates(t *testing.T) {
	cases := map[string]uint64{
		" 4GiB ":    4 * 1024 * 1024 * 1024,
		"\t4GiB\n":  4 * 1024 * 1024 * 1024,
		"1.5GiB":    1610612736,
		"0.5KiB":    512,
		"2.9":       2, // int() truncates; rounding would admit more than was asked for
		"1.999KiB":  2046,
		"4   GiB":   4 * 1024 * 1024 * 1024,
		"0":         0,
		"0.0GiB":    0,
		"00004GiB":  4 * 1024 * 1024 * 1024,
		"1000000KB": 1_000_000_000,
	}
	for in, want := range cases {
		got, err := ParseMemBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseMemBytes(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
}

func TestParseMemBytesRejectsGarbage(t *testing.T) {
	bad := []string{
		"", "   ", "banana", "4XB", "GiB", "-4GiB", "+4GiB", ".5GiB", "4.GiB",
		"1e3", "0x10", "4GiB4", "4 G iB", "NaN", "Inf", "4GiB!",
		// ASCII-only on purpose: Python's \d and \s also accept these, so this is
		// stricter than the spec and fails closed (see ParseMemBytes).
		"٤GiB", "４GiB", "4 GiB", "4　GiB", "4\vGiB",
	}
	for _, in := range bad {
		if got, err := ParseMemBytes(in); err == nil {
			t.Errorf("ParseMemBytes(%q) = %d, want an error", in, got)
		}
	}
}

// A value that does not fit is refused, never converted: float64->uint64 is
// implementation-defined out of range (MaxUint64 on arm64, 1<<63 on amd64), and
// a ceiling that wrapped would admit every launch.
func TestParseMemBytesRejectsOutOfRange(t *testing.T) {
	bad := []string{
		"8388608TiB",              // 2^63, the first refused value
		"16777216TiB",             // 2^64 exactly
		"17179869184GiB",          // 2^64
		"99999999999TiB",          // far beyond
		"18446744073709551615",    // MaxUint64 as bytes (float rounds up to 2^64)
		"9223372036854775807",     // MaxInt64 as bytes (float rounds up to 2^63)
		"99999999999999999999GiB", // 20 digits
		"1" + strings.Repeat("0", 30) + "B",
		strings.Repeat("9", 400),        // ParseFloat overflows to +Inf
		strings.Repeat("9", 400) + "TB", // likewise with a unit
		"1" + strings.Repeat("0", 400) + ".5GiB",
	}
	for _, in := range bad {
		if got, err := ParseMemBytes(in); err == nil {
			t.Errorf("ParseMemBytes(%.40q) = %d, want an error", in, got)
		}
	}
}

// The largest values still accepted keep Python's exact results, and the sum of
// any two accepted values fits a uint64, so broker admission arithmetic
// (need + floor) cannot wrap.
func TestParseMemBytesLargestAccepted(t *testing.T) {
	cases := map[string]uint64{
		"8388607TiB":          8388607 * (1 << 40),
		"9223372036854774784": 9223372036854774784, // largest float64 below 2^63
	}
	for in, want := range cases {
		got, err := ParseMemBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseMemBytes(%q) = %d, %v, want %d", in, got, err, want)
		}
		if got+got < got {
			t.Errorf("%q: sum of two accepted values wraps", in)
		}
	}
}

func writeMeminfo(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAvailableBytesReadsMemAvailable(t *testing.T) {
	p := writeMeminfo(t, "MemTotal:       65806152 kB\n"+
		"MemFree:         2117324 kB\n"+
		"MemAvailable:   25165824 kB\n"+
		"Buffers:          123456 kB\n")
	got, err := AvailableBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != 25165824*1024 {
		t.Errorf("AvailableBytes = %d, want %d", got, 25165824*1024)
	}
}

func TestAvailableBytesErrorsWhenMemAvailableAbsent(t *testing.T) {
	p := writeMeminfo(t, "MemTotal: 65806152 kB\nMemFree: 2117324 kB\n")
	got, err := AvailableBytes(p)
	if !errors.Is(err, ErrNoMemAvailable) {
		t.Errorf("AvailableBytes = %d, %v, want ErrNoMemAvailable", got, err)
	}
	if err != nil && !strings.Contains(err.Error(), p+" has no MemAvailable line") {
		t.Errorf("error = %q, want Python's \"<path> has no MemAvailable line\"", err)
	}
}

// "Could not measure" must stay an error in every shape: a silent (0, nil)
// would read as "no headroom" for one caller and a bug for the next.
func TestAvailableBytesErrorsWhenUnmeasurable(t *testing.T) {
	cases := map[string]string{
		"bare key":           "MemAvailable:\n",
		"non-numeric":        "MemAvailable: abc kB\n",
		"negative":           "MemAvailable: -5 kB\n",
		"overflows uint64":   "MemAvailable: " + strconv.FormatUint(1<<54, 10) + " kB\n",
		"overflows wrapping": "MemAvailable: " + strconv.FormatUint(1<<54+1, 10) + " kB\n",
		"beyond uint64":      "MemAvailable: 99999999999999999999999 kB\n",
		"empty file":         "",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := AvailableBytes(writeMeminfo(t, content))
			if err == nil {
				t.Fatalf("AvailableBytes = %d, nil; want an error", got)
			}
			if got != 0 {
				t.Errorf("AvailableBytes = %d alongside an error", got)
			}
		})
	}
}

func TestAvailableBytesErrorsOnUnreadablePath(t *testing.T) {
	got, err := AvailableBytes(filepath.Join(t.TempDir(), "no-such-meminfo"))
	if err == nil {
		t.Fatalf("AvailableBytes = %d, nil; want an error", got)
	}
	if errors.Is(err, ErrNoMemAvailable) {
		t.Errorf("an unreadable file is not an absent line: %v", err)
	}
}

func TestAvailableBytesLargestRepresentable(t *testing.T) {
	const maxKB = (1<<64 - 1) / 1024
	p := writeMeminfo(t, "MemAvailable: "+strconv.FormatUint(maxKB, 10)+" kB\n")
	got, err := AvailableBytes(p)
	if err != nil || got != maxKB*1024 {
		t.Errorf("AvailableBytes = %d, %v, want %d", got, err, uint64(maxKB)*1024)
	}
}
