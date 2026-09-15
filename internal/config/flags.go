package config

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is the prefix of every environment variable the binary reads.
const EnvPrefix = "SPARQL_SERVER_"

// EnvName returns the environment variable for a flag name:
// "update-token" becomes SPARQL_SERVER_UPDATE_TOKEN.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// listValue is a repeatable string flag. Every occurrence appends; from the
// environment the value is split on commas.
type listValue struct{ p *[]string }

func (l listValue) String() string {
	if l.p == nil {
		return ""
	}
	return strings.Join(*l.p, ",")
}

func (l listValue) Set(s string) error {
	*l.p = append(*l.p, s)
	return nil
}

// isList lets ApplyEnv split a list flag's environment value.
func (l listValue) isList() {}

type lister interface{ isList() }

// bytesValue is a byte size: a plain number of bytes or a number with a
// KB/MB/GB (powers of 1000) or KiB/MiB/GiB (powers of 1024) suffix.
type bytesValue struct{ p *int64 }

func (b bytesValue) String() string {
	if b.p == nil {
		return "0"
	}
	return FormatBytes(*b.p)
}

func (b bytesValue) Set(s string) error {
	n, err := ParseBytes(s)
	if err != nil {
		return err
	}
	*b.p = n
	return nil
}

var byteUnits = []struct {
	suffix string
	mult   int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	{"B", 1},
}

// ParseBytes parses a byte size such as "64MiB", "10MB", "512k" or "1048576".
func ParseBytes(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return 0, errors.New("empty byte size")
	}
	mult := int64(1)
	for _, u := range byteUnits {
		if strings.HasSuffix(t, u.suffix) {
			mult = u.mult
			t = strings.TrimSpace(strings.TrimSuffix(t, u.suffix))
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	if mult > 1 && n > (1<<62)/mult {
		return 0, fmt.Errorf("byte size %q is too large", s)
	}
	return n * mult, nil
}

// FormatBytes prints n in the largest binary unit that divides it.
func FormatBytes(n int64) string {
	switch {
	case n == 0:
		return "0"
	case n%(1<<30) == 0:
		return strconv.FormatInt(n>>30, 10) + "GiB"
	case n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + "MiB"
	case n%(1<<10) == 0:
		return strconv.FormatInt(n>>10, 10) + "KiB"
	}
	return strconv.FormatInt(n, 10)
}

// ApplyEnv sets every flag of fs that was not given on the command line from
// its environment variable (see EnvName), so the precedence is flag, then
// environment, then default. It must run after fs.Parse.
//
// It returns the names of SPARQL_SERVER_* variables that match no flag of any
// subcommand in known, so that a typo is reported instead of silently ignored.
func ApplyEnv(fs *flag.FlagSet, environ []string, known map[string]bool) (unknown []string, err error) {
	env := make(map[string]string)
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, EnvPrefix) {
			env[k] = v
		}
	}
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] {
			return
		}
		name := EnvName(f.Name)
		v, ok := env[name]
		if !ok {
			return
		}
		if _, isList := f.Value.(lister); isList {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					if err := f.Value.Set(part); err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", name, err))
					}
				}
			}
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %w", name, v, err))
		}
	})
	for k := range env {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown, errors.Join(errs...)
}

// durationFlag and friends keep the flag definitions below readable.
func durationFlag(fs *flag.FlagSet, p *time.Duration, name string, def time.Duration, usage string) {
	fs.DurationVar(p, name, def, usage)
}

func bytesFlag(fs *flag.FlagSet, p *int64, name string, def int64, usage string) {
	*p = def
	fs.Var(bytesValue{p}, name, usage)
}

func listFlag(fs *flag.FlagSet, p *[]string, name, usage string) {
	fs.Var(listValue{p}, name, usage)
}
